#!/usr/bin/env python3
"""Publish verified assets through a recoverable draft; never replace a release asset.

Only the release workflow calls this command with GH_TOKEN. Tests use an in-memory
GitHub adapter and do not create tags, releases, or external requests.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


class PublishError(RuntimeError):
    pass


def digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def payload(metadata: Path, notes: Path, artifacts: Path) -> dict:
    meta = json.loads(metadata.read_text(encoding="utf-8"))
    tag, commit = meta.get("tag", ""), meta.get("commit", "")
    if (meta.get("schema") != 1 or
            not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", tag) or
            not re.fullmatch(r"[0-9a-f]{40}", commit) or meta.get("version") != tag[1:]):
        raise PublishError("Invalid release metadata")
    # A tag push may expose the annotated tag object's SHA in GITHUB_SHA.
    # The prepare job supplies the independently peeled checkout commit.
    if os.environ.get("RELEASE_COMMIT", commit) != commit:
        raise PublishError("Release metadata does not match this workflow's commit")
    text = notes.read_text(encoding="utf-8").strip()
    if not text:
        raise PublishError("Release notes are empty")
    manifest_path = artifacts / "release-manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if any(manifest.get(key) != meta[key] for key in ("schema", "tag", "commit")):
        raise PublishError("Manifest identity does not match release metadata")
    expected_names = {
        f"feishu-bot-{tag}-{target}.{'zip' if target.startswith('windows') else 'tar.gz'}"
        for target in ("linux-amd64", "linux-arm64", "windows-amd64", "darwin-amd64", "darwin-arm64")
    }
    assets = manifest.get("assets", [])
    if (not isinstance(assets, list) or len(assets) != len(expected_names) or
            {item.get("name") for item in assets} != expected_names):
        raise PublishError("Manifest must contain exactly the five platform archives")
    hashes = {}
    for item in assets:
        name = item["name"]
        path = artifacts / name
        if path.is_symlink() or not path.is_file():
            raise PublishError(f"Missing or unsafe local asset: {name}")
        hashes[name] = digest(path)
        if hashes[name] != item.get("sha256") or path.stat().st_size != item.get("size"):
            raise PublishError(f"Local asset does not match manifest: {name}")
    expected_sums = "".join(f"{hashes[name]}  {name}\n" for name in sorted(hashes))
    if (artifacts / "SHA256SUMS.txt").read_text(encoding="utf-8") != expected_sums:
        raise PublishError("SHA256SUMS.txt does not match the verified archives")
    names = expected_names | {"SHA256SUMS.txt", "release-manifest.json"}
    if {path.name for path in artifacts.iterdir()} != names:
        raise PublishError("Unexpected files in release payload")
    for name in names:
        if (artifacts / name).is_symlink() or not (artifacts / name).is_file():
            raise PublishError(f"Unsafe local asset: {name}")
        hashes[name] = digest(artifacts / name)
    identity = {"schema": 1, "tag": tag, "commit": commit,
                "manifest_sha256": hashes["release-manifest.json"]}
    marker = "<!-- feishu-bot-release " + json.dumps(identity, sort_keys=True, separators=(",", ":")) + " -->"
    if "<!-- feishu-bot-release " in text:
        raise PublishError("Release notes contain a reserved publisher identity marker")
    return {"tag": tag, "commit": commit, "hashes": hashes, "marker": marker,
            "body": text + "\n\n" + marker + "\n", "artifacts": artifacts}


class GitHub:
    """Small gh adapter. Every call is explicit; failed writes are never retried blindly."""

    def __init__(self, repository: str):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise PublishError("Expected repository in owner/name form")
        self.repository = repository
        self.prefix = f"repos/{repository}"

    def run(self, *args: str, output: Path | None = None) -> str:
        command = ["gh", *args]
        if output is None:
            result = subprocess.run(command, capture_output=True, text=True, check=False)
        else:
            with output.open("wb") as stream:
                result = subprocess.run(command, stdout=stream, stderr=subprocess.PIPE, check=False)
        if result.returncode:
            error = result.stderr.decode(errors="replace") if isinstance(result.stderr, bytes) else result.stderr
            raise PublishError(f"GitHub command failed: {error.strip()}")
        return result.stdout if output is None else ""

    def api(self, endpoint: str, *, pages: bool = False) -> object:
        flags = ["--paginate", "--slurp"] if pages else []
        value = json.loads(self.run("api", f"{self.prefix}/{endpoint}", *flags))
        return [item for page in value for item in page] if pages else value

    def tag_commit(self, tag: str) -> str:
        value = self.api(f"git/ref/tags/{tag}")["object"]
        for _ in range(10):
            if value["type"] == "commit":
                return value["sha"]
            if value["type"] != "tag":
                break
            value = self.api(f"git/tags/{value['sha']}")["object"]
        raise PublishError("Version tag does not resolve to a commit")

    def release(self, tag: str) -> dict | None:
        # List includes authorized drafts; get-by-tag endpoints may omit drafts.
        matches = [item for item in self.api("releases?per_page=100", pages=True)
                   if item["tag_name"] == tag]
        if len(matches) > 1:
            raise PublishError("Multiple releases refer to this tag")
        return matches[0] if matches else None

    def create_draft(self, tag: str, body: str) -> None:
        with tempfile.TemporaryDirectory(prefix="feishu-release-notes-") as directory:
            path = Path(directory) / "notes.md"
            path.write_text(body, encoding="utf-8")
            self.run("release", "create", tag, "--repo", self.repository,
                     "--verify-tag", "--draft", "--title", f"feishu-bot {tag}",
                     "--notes-file", str(path))

    def assets(self, release_id: int) -> list:
        return self.api(f"releases/{release_id}/assets?per_page=100", pages=True)

    def download(self, asset: dict, output: Path) -> None:
        self.run("api", f"{self.prefix}/releases/assets/{asset['id']}",
                 "--header", "Accept: application/octet-stream", output=output)

    def upload(self, tag: str, path: Path) -> None:
        self.run("release", "upload", tag, str(path), "--repo", self.repository)

    def delete_failed_upload(self, asset: dict) -> None:
        self.run("api", f"{self.prefix}/releases/assets/{asset['id']}", "--method", "DELETE")

    def publish(self, release_id: int) -> None:
        # Let GitHub choose latest using its legacy date/SemVer ordering instead
        # of making a slower, older concurrent release unconditionally latest.
        self.run("api", f"{self.prefix}/releases/{release_id}", "--method", "PATCH",
                 "--field", "draft=false", "--field", "prerelease=false",
                 "--raw-field", "make_latest=legacy")


def assert_tag(api: GitHub, data: dict) -> None:
    if api.tag_commit(data["tag"]) != data["commit"]:
        raise PublishError("Remote tag moved or does not match the verified commit")


def assert_release(release: dict | None, data: dict) -> dict:
    if release is None or release.get("tag_name") != data["tag"]:
        raise PublishError("Release missing or tag mismatch")
    body = release.get("body") or ""
    if body.count(data["marker"]) != 1 or body.count("<!-- feishu-bot-release ") != 1:
        raise PublishError("Existing release belongs to a different commit or payload")
    if release.get("prerelease"):
        raise PublishError("A stable tag must not refer to a prerelease")
    return release


def verify_remote(api: GitHub, release: dict, data: dict, *, complete: bool) -> set[str]:
    assets = api.assets(release["id"])
    names = [asset["name"] for asset in assets]
    expected = set(data["hashes"])
    if len(set(names)) != len(names) or set(names) - expected:
        raise PublishError("Duplicate or unexpected remote assets; no files were replaced")
    if complete and set(names) != expected:
        raise PublishError("Published release is missing required assets")
    with tempfile.TemporaryDirectory(prefix="feishu-release-verify-") as directory:
        for asset in assets:
            name = asset["name"]
            # GitHub can leave a zero-byte starter entry after an upload 502.
            # Only this explicitly failed record in our bound draft is removed;
            # an uploaded, nonempty, or published asset is never replaced.
            if (not complete and release["draft"] and asset.get("state") == "starter"
                    and asset.get("size") == 0):
                current = assert_release(api.release(data["tag"]), data)
                if not current["draft"]:
                    raise PublishError("Release was published while recovering a failed upload")
                assert_tag(api, data)
                api.delete_failed_upload(asset)
                names.remove(name)
                continue
            if asset.get("state") != "uploaded":
                raise PublishError(f"Remote asset upload is incomplete: {name}")
            downloaded = Path(directory) / name
            api.download(asset, downloaded)
            if digest(downloaded) != data["hashes"][name]:
                raise PublishError(f"Remote asset differs from verified payload: {name}")
    return set(names)


def publish(api: GitHub, data: dict) -> str:
    assert_tag(api, data)
    release = api.release(data["tag"])
    if release is None:
        api.create_draft(data["tag"], data["body"])
        release = api.release(data["tag"])
    release = assert_release(release, data)
    if not release["draft"]:
        verify_remote(api, release, data, complete=True)
        assert_tag(api, data)
        return release["html_url"]
    existing = verify_remote(api, release, data, complete=False)
    assert_tag(api, data)
    for name in sorted(set(data["hashes"]) - existing):
        api.upload(data["tag"], data["artifacts"] / name)
    # Re-read state after uploads. A manual publication or metadata change must
    # not silently receive another mutation from this workflow.
    release = assert_release(api.release(data["tag"]), data)
    verify_remote(api, release, data, complete=True)
    assert_tag(api, data)
    if release["draft"]:
        api.publish(release["id"])
    release = assert_release(api.release(data["tag"]), data)
    if release["draft"]:
        raise PublishError("Release is still a draft after publication")
    verify_remote(api, release, data, complete=True)
    assert_tag(api, data)
    return release["html_url"]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--metadata", required=True, type=Path)
    parser.add_argument("--notes", required=True, type=Path)
    parser.add_argument("--artifacts", required=True, type=Path)
    parser.add_argument("--repository", required=True)
    args = parser.parse_args()
    try:
        data = payload(args.metadata, args.notes, args.artifacts)
        print(publish(GitHub(args.repository), data))
    except (PublishError, OSError, ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"Release publication failed: {error}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
