"""Offline tests for release identities, archives and fail-closed validation."""

from __future__ import annotations

import copy
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock
import zipfile

import release


SCRIPT = Path(__file__).with_name("release.py").resolve()
TAG = "v1.2.3"
COMMIT = "1234567890abcdef1234567890abcdef12345678"
EPOCH = 1793750400  # 2026-11-04 00:00:00 UTC.
META = {"schema": 1, "tag": TAG, "version": "1.2.3", "commit": COMMIT, "source_date_epoch": EPOCH}
ARCHIVES = {
    ("linux", "amd64"): "feishu-bot-v1.2.3-linux-amd64.tar.gz",
    ("linux", "arm64"): "feishu-bot-v1.2.3-linux-arm64.tar.gz",
    ("windows", "amd64"): "feishu-bot-v1.2.3-windows-amd64.zip",
    ("darwin", "amd64"): "feishu-bot-v1.2.3-darwin-amd64.tar.gz",
    ("darwin", "arm64"): "feishu-bot-v1.2.3-darwin-arm64.tar.gz",
}


def binary_fixture(os_name: str, arch: str) -> bytes:
    """Minimal format headers, for archive tests only; these are not runnable binaries."""
    data = bytearray(256)
    if os_name == "linux":
        data[:6] = b"\x7fELF\x02\x01"
        struct.pack_into("<HH", data, 16, 2, 62 if arch == "amd64" else 183)
    elif os_name == "darwin":
        struct.pack_into("<IIII", data, 0, 0xFEEDFACF,
                         0x01000007 if arch == "amd64" else 0x0100000C, 0, 2)
    else:
        data[:2] = b"MZ"
        struct.pack_into("<I", data, 60, 128)
        data[128:132] = b"PE\0\0"
        struct.pack_into("<H", data, 132, 0x8664)
        struct.pack_into("<H", data, 152, 0x20B)
    return bytes(data)


def rewrite_tar(path: Path, transform) -> None:
    with tarfile.open(path, "r:gz") as archive:
        members = [(copy.copy(item), archive.extractfile(item).read()) for item in archive]
    members = transform(members)
    with tarfile.open(path, "w:gz") as archive:
        for item, data in members:
            item.size = len(data)
            archive.addfile(item, io.BytesIO(data))


class NotesTests(unittest.TestCase):
    def test_only_stable_versions_are_accepted(self):
        self.assertEqual(release.stable_version("v0.0.0"), "0.0.0")
        for tag in ("1.2.3", "v1.2", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2.3-rc.1",
                    "v1.2.3+build", "v1.2.3/other", "v1.2.3\n", "v１.2.3"):
            with self.subTest(tag=tag), self.assertRaises(release.ReleaseError):
                release.stable_version(tag)

    def test_fenced_headings_and_nested_headings_remain_in_notes(self):
        text = """# 版本记录
## Unreleased
```md
## 1.2.3 — 2026-11-04
example only
```
## 1.2.3 — 2026-11-04

### Added
- 新功能。

~~~md
## 1.2.3 — 2026-11-04
~~~

    ## 1.2.3 — 2026-11-04

## 1.2.2 — 2026-11-03
- Older.
"""
        self.assertEqual(release.extract_notes(text, "1.2.3"),
                         "### Added\n- 新功能。\n\n~~~md\n## 1.2.3 — 2026-11-04\n~~~\n\n    ## 1.2.3 — 2026-11-04\n")

    def test_missing_duplicate_malformed_or_empty_sections_fail(self):
        cases = (
            "## Unreleased\n- Nothing released.\n",
            "## 1.2.3 — 2026-11-04\n- First.\n## 1.2.3 — 2026-11-04\n- Second.\n",
            "## 1.2.3 — 2026-02-30\n- Bad date.\n",
            "## 1.2.3 — 2026-11-04\n \n## 1.2.2 — 2026-11-03\n- Older.\n",
            "## v1.2.3 — 2026-11-04\n- Wrong heading format.\n",
            "## [1.2.3] — 2026-11-04\n- Wrong heading format.\n",
            "## 1.2.3 - 2026-11-04\n- Wrong separator.\n",
            "## 1.2.30 — 2026-11-04\n- Different version.\n",
            "## 1.2.3 — 2026-11-04\n- First.\n## 1.2.3 invalid\n- Duplicate malformed heading.\n",
        )
        for text in cases:
            with self.subTest(text=text), self.assertRaises(release.ReleaseError):
                release.extract_notes(text, "1.2.3")


class PrepareTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="release-prepare-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.env = {**os.environ, "GIT_AUTHOR_DATE": "2026-11-04T00:00:00+00:00",
                    "GIT_COMMITTER_DATE": "2026-11-04T00:00:00+00:00"}
        self.git("init", "--quiet", "--initial-branch=main")
        self.git("config", "user.name", "Release test")
        self.git("config", "user.email", "release-test@example.invalid")
        self.git("config", "commit.gpgsign", "false")
        self.git("config", "tag.gpgsign", "false")
        self.git("config", "core.autocrlf", "false")
        (self.repo / "CHANGELOG.md").write_bytes(b"# Changelog\n\n## 1.2.3 \xe2\x80\x94 2026-11-04\n\n- A released change.\n")
        self.git("add", "CHANGELOG.md")
        self.git("commit", "--quiet", "-m", "Release test fixture")
        self.sha = self.git("rev-parse", "HEAD").strip()
        self.git("tag", "-a", TAG, "-m", "Annotated test tag")
        self.out = self.root / "prepared"

    def git(self, *args):
        return subprocess.run(["git", *args], cwd=self.repo, env=self.env, check=True,
                              capture_output=True, text=True, timeout=30).stdout

    def test_cli_prepares_exact_annotated_tag_and_notes(self):
        result = subprocess.run([sys.executable, "-B", str(SCRIPT), "prepare", "--tag", TAG,
                                 "--commit", self.sha, "--output", str(self.out)],
                                cwd=self.repo, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)
        meta = json.loads((self.out / "release.json").read_bytes())
        self.assertEqual(meta, {"schema": 1, "tag": "v1.2.3", "version": "1.2.3",
                                "commit": self.sha, "source_date_epoch": EPOCH})
        self.assertEqual((self.out / "release-notes.md").read_bytes(), b"- A released change.\n")
        release.prepare(TAG, self.sha, self.out, self.repo)

    def test_requested_sha_and_tag_must_both_match_head(self):
        with self.assertRaisesRegex(release.ReleaseError, "must be identical"):
            release.prepare(TAG, "a" * 40, self.out, self.repo)
        (self.repo / "new.txt").write_bytes(b"new commit\n")
        self.git("add", "new.txt")
        self.git("commit", "--quiet", "-m", "Another fixture commit")
        new_sha = self.git("rev-parse", "HEAD").strip()
        with self.assertRaisesRegex(release.ReleaseError, "must be identical"):
            release.prepare(TAG, new_sha, self.out, self.repo)
        self.assertFalse(self.out.exists())

    def test_dirty_source_cannot_be_labeled_as_the_tag(self):
        (self.repo / "CHANGELOG.md").write_bytes(b"uncommitted replacement\n")
        with self.assertRaisesRegex(release.ReleaseError, "clean source"):
            release.prepare(TAG, self.sha, self.out, self.repo)
        self.assertFalse(self.out.exists())

    def test_untracked_source_is_rejected(self):
        (self.repo / "untracked.go").write_bytes(b"package main\n")
        with self.assertRaisesRegex(release.ReleaseError, "clean source"):
            release.prepare(TAG, self.sha, self.out, self.repo)
        self.assertFalse(self.out.exists())

    def test_existing_metadata_is_not_overwritten(self):
        self.out.mkdir()
        original = b'{"tag":"v9.9.9"}\n'
        (self.out / "release.json").write_bytes(original)
        with self.assertRaisesRegex(release.ReleaseError, "existing output differs"):
            release.prepare(TAG, self.sha, self.out, self.repo)
        self.assertEqual((self.out / "release.json").read_bytes(), original)
        self.assertFalse((self.out / "release-notes.md").exists())


class PackagingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="release-package-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        (self.repo / "examples").mkdir(parents=True)
        (self.repo / "examples/config.example.json").write_bytes(b'{"tenantId":"tenant_example"}\n')
        (self.repo / "LICENSE").write_bytes(b"License fixture\n")
        (self.repo / "THIRD_PARTY_NOTICES.md").write_bytes(b"Third-party fixture\n")
        # These files must never be selected by directory-wide packaging.
        (self.repo / "config.json").write_bytes(b'{"private":"must not be packaged"}\n')
        (self.repo / "service.env").write_bytes(b"EXAMPLE_TOKEN=must-not-be-packaged\n")
        self.out = self.root / "dist"
        self.meta = copy.deepcopy(META)

    def make_package(self, target=("linux", "amd64"), output=None):
        os_name, arch = target
        binary = self.root / f"binary-{os_name}-{arch}"
        binary.write_bytes(binary_fixture(os_name, arch))
        return release.package(self.meta, binary, os_name, arch, output or self.out, self.repo)

    def make_all(self):
        for target in ARCHIVES:
            self.make_package(target)

    def test_all_targets_use_exact_names_and_deterministic_whitelisted_payloads(self):
        for target, name in ARCHIVES.items():
            with self.subTest(target=target):
                first = self.make_package(target)
                os.utime(self.repo / "LICENSE", (1, 2))
                second = self.make_package(target, self.root / "second")
                self.assertEqual(first.name, name)
                self.assertEqual(first.read_bytes(), second.read_bytes())
                root = name.removesuffix(".tar.gz").removesuffix(".zip")
                executable = "feishu-bot.exe" if target[0] == "windows" else "feishu-bot"
                expected = {f"{root}/{entry}" for entry in (
                    executable, "config.example.json", "INSTALL.md", "LICENSE", "THIRD_PARTY_NOTICES.md", "BUILDINFO.json")}
                if target[0] == "windows":
                    with zipfile.ZipFile(first) as archive:
                        self.assertEqual(set(archive.namelist()), expected)
                        build = json.loads(archive.read(f"{root}/BUILDINFO.json"))
                else:
                    with tarfile.open(first, "r:gz") as archive:
                        self.assertEqual(set(archive.getnames()), expected)
                        self.assertEqual(archive.getmember(f"{root}/{executable}").mode, 0o755)
                        self.assertEqual({item.mtime for item in archive}, {EPOCH})
                        build = json.load(archive.extractfile(f"{root}/BUILDINFO.json"))
                self.assertEqual((build["os"], build["arch"]), target)
                self.assertEqual(build["commit"], COMMIT)
                self.assertEqual(build["binary_sha256"], hashlib.sha256(binary_fixture(*target)).hexdigest())

    def test_wrong_binary_architecture_is_not_packaged(self):
        binary = self.root / "wrong-binary"
        binary.write_bytes(binary_fixture("linux", "amd64"))
        with self.assertRaisesRegex(release.ReleaseError, "format or architecture"):
            release.package(self.meta, binary, "linux", "arm64", self.out, self.repo)
        self.assertFalse(self.out.exists())

    def test_unsupported_target_is_rejected(self):
        with self.assertRaisesRegex(release.ReleaseError, "unsupported release target"):
            self.make_package(("windows", "arm64"))

    def test_metadata_identity_and_types_are_validated(self):
        changes = ({"tag": "v1.2.3-rc.1"}, {"version": "1.2.4"}, {"commit": "abc123"},
                   {"source_date_epoch": True}, {"source_date_epoch": -1}, {"schema": True})
        for change in changes:
            with self.subTest(change=change), self.assertRaises(release.ReleaseError):
                release.validate_metadata({**self.meta, **change})

    def test_verify_requires_the_complete_five_target_set(self):
        self.make_package()
        with self.assertRaisesRegex(release.ReleaseError, "exactly the five"):
            release.verify(self.meta, self.out)
        self.assertFalse((self.out / "release-manifest.json").exists())

    def test_verify_writes_checksums_and_a_commit_bound_manifest_idempotently(self):
        self.make_all()
        release.verify(self.meta, self.out)
        manifest = json.loads((self.out / "release-manifest.json").read_bytes())
        self.assertEqual(manifest["commit"], COMMIT)
        self.assertEqual(manifest["tag"], TAG)
        expected = [{"name": name, "sha256": hashlib.sha256((self.out / name).read_bytes()).hexdigest(),
                     "size": (self.out / name).stat().st_size} for name in sorted(ARCHIVES.values())]
        self.assertEqual(manifest["assets"], expected)
        self.assertEqual((self.out / "SHA256SUMS.txt").read_text(),
                         "".join(f"{item['sha256']}  {item['name']}\n" for item in expected))
        snapshot = {p.name: p.read_bytes() for p in self.out.iterdir()}
        release.verify(self.meta, self.out)
        self.assertEqual(snapshot, {p.name: p.read_bytes() for p in self.out.iterdir()})

    def test_extra_artifacts_are_not_silently_published(self):
        self.make_all()
        (self.out / "service.env").write_bytes(b"EXAMPLE_TOKEN=not-for-release\n")
        with self.assertRaisesRegex(release.ReleaseError, "exactly the five"):
            release.verify(self.meta, self.out)
        self.assertFalse((self.out / "SHA256SUMS.txt").exists())

    def test_mutated_binary_fails_even_before_manifest_creation(self):
        path = self.make_package()
        def change(members):
            return [(item, data[:-1] + b"X" if item.name.endswith("/feishu-bot") else data)
                    for item, data in members]
        rewrite_tar(path, change)
        with self.assertRaisesRegex(release.ReleaseError, "binary checksum"):
            release.read_archive(path, self.meta, "linux", "amd64")

    def test_altered_checksums_are_rejected_without_overwrite(self):
        self.make_all()
        release.verify(self.meta, self.out)
        path = self.out / "SHA256SUMS.txt"
        altered = b"0" * 64 + path.read_bytes()[64:]
        path.write_bytes(altered)
        with self.assertRaisesRegex(release.ReleaseError, "SHA256SUMS.txt does not match"):
            release.verify(self.meta, self.out)
        self.assertEqual(path.read_bytes(), altered)

    def test_altered_manifest_is_rejected_without_overwrite(self):
        self.make_all()
        release.verify(self.meta, self.out)
        path = self.out / "release-manifest.json"
        original = json.loads(path.read_bytes())
        original["commit"] = "f" * 40
        altered = json.dumps(original).encode()
        path.write_bytes(altered)
        with self.assertRaisesRegex(release.ReleaseError, "release-manifest.json does not match"):
            release.verify(self.meta, self.out)
        self.assertEqual(path.read_bytes(), altered)

    def test_archive_commit_mismatch_is_rejected(self):
        path = self.make_package()
        def change(members):
            return [(item, data.replace(COMMIT.encode(), b"f" * 40)
                     if item.name.endswith("/BUILDINFO.json") else data) for item, data in members]
        rewrite_tar(path, change)
        with self.assertRaisesRegex(release.ReleaseError, "build identity"):
            release.read_archive(path, self.meta, "linux", "amd64")

    def test_tar_traversal_duplicates_and_links_are_rejected_without_extraction(self):
        for kind in ("traversal", "duplicate", "link"):
            with self.subTest(kind=kind):
                path = self.make_package(output=self.root / kind)
                def change(members):
                    item = tarfile.TarInfo("../../escaped")
                    if kind == "duplicate":
                        item.name = members[0][0].name
                    if kind == "link":
                        item.name = "feishu-bot-v1.2.3-linux-amd64/config.example.json"
                        item.type, item.linkname = tarfile.SYMTYPE, "../../escaped"
                    item.mode = 0o644
                    return members + [(item, b"unexpected")]
                rewrite_tar(path, change)
                with self.assertRaisesRegex(release.ReleaseError, "unexpected, duplicate or non-regular"):
                    release.read_archive(path, self.meta, "linux", "amd64")
                self.assertFalse((self.root / "escaped").exists())

    def test_zip_symlink_member_is_rejected(self):
        path = self.make_package(("windows", "amd64"))
        with zipfile.ZipFile(path, "a") as archive:
            member = zipfile.ZipInfo("feishu-bot-v1.2.3-windows-amd64/link")
            member.create_system = 3
            member.external_attr = (stat.S_IFLNK | 0o777) << 16
            archive.writestr(member, "../../escaped")
        with self.assertRaisesRegex(release.ReleaseError, "unexpected, duplicate or non-regular"):
            release.read_archive(path, self.meta, "windows", "amd64")

    def test_missing_file_or_lost_executable_mode_is_rejected(self):
        missing = self.make_package(output=self.root / "missing")
        rewrite_tar(missing, lambda members: [(item, data) for item, data in members
                                             if not item.name.endswith("/LICENSE")])
        with self.assertRaisesRegex(release.ReleaseError, "complete payload"):
            release.read_archive(missing, self.meta, "linux", "amd64")
        wrong_mode = self.make_package(output=self.root / "mode")
        def change(members):
            for item, _ in members:
                if item.name.endswith("/feishu-bot"):
                    item.mode = 0o644
            return members
        rewrite_tar(wrong_mode, change)
        with self.assertRaisesRegex(release.ReleaseError, "incorrect archive permissions"):
            release.read_archive(wrong_mode, self.meta, "linux", "amd64")

    def test_smoke_rejects_a_foreign_architecture_before_execution(self):
        path = self.make_package(("linux", "arm64"))
        with mock.patch.object(release.platform, "system", return_value="Linux"), \
             mock.patch.object(release.platform, "machine", return_value="x86_64"), \
             mock.patch.object(release, "run") as process:
            with self.assertRaisesRegex(release.ReleaseError, "native linux/arm64 runner"):
                release.smoke(self.meta, path, "linux", "arm64")
            process.assert_not_called()

    def test_smoke_checks_binary_version_before_other_commands(self):
        path = self.make_package()
        with mock.patch.object(release.platform, "system", return_value="Linux"), \
             mock.patch.object(release.platform, "machine", return_value="x86_64"), \
             mock.patch.object(release, "run", return_value="feishu-bot 9.9.9\n") as process:
            with self.assertRaisesRegex(release.ReleaseError, "different version"):
                release.smoke(self.meta, path, "linux", "amd64")
            self.assertEqual(process.call_count, 1)
            self.assertEqual(process.call_args.args[0][1:], ["--version"])

    def test_smoke_uses_only_offline_cli_commands_and_an_isolated_configuration(self):
        path = self.make_package()
        calls = []
        def process(args, *, cwd):
            calls.append(args[1:])
            self.assertNotEqual(cwd, self.repo)
            self.assertTrue(Path(args[0]).is_file())
            if args[1] == "--version":
                return "feishu-bot 1.2.3\n"
            if args[1] == "--help":
                return "feishu-bot init|doctor|run\n"
            self.assertEqual(args[1:3], ["init", "--config"])
            config = Path(args[3])
            self.assertEqual(config.parent.parent, cwd)
            config.parent.mkdir()
            config.write_bytes(b'{"tenantId":"tenant_example"}\n')
            return "Created configuration.\n"
        with mock.patch.object(release.platform, "system", return_value="Linux"), \
             mock.patch.object(release.platform, "machine", return_value="x86_64"), \
             mock.patch.object(release, "run", side_effect=process):
            release.smoke(self.meta, path, "linux", "amd64")
        self.assertEqual([args[0] for args in calls], ["--version", "--help", "init"])


if __name__ == "__main__":
    unittest.main()
