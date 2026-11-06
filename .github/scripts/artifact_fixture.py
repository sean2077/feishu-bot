"""Build a pinned previous executable and bind artifact tests to exact bytes.

The baseline is source from the pre-optimization main commit, built with the
same native Go toolchain. It is never a version-only copy of the new binary.
Updating this compatibility baseline is an explicit, reviewable source change.
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


BASELINE_COMMIT = "f727a9cd074e1001000fa7c14ecad38d25e369d1"
BASELINE_VERSION = "0.0.0-upgrade." + BASELINE_COMMIT


def run(args: list[str], cwd: Path, *, env: dict | None = None) -> str:
    return subprocess.check_output(args, cwd=cwd, env=env, text=True, encoding="utf-8").strip()


def record(binary: Path, version: str, commit: str, output: Path, prefix: str, repo: Path) -> dict:
    if not re.fullmatch(r"[0-9a-f]{40}", commit) or prefix not in ("FEISHU_ARTIFACT", "FEISHU_PREVIOUS_ARTIFACT"):
        raise ValueError("artifact identity requires an exact commit and known fixture prefix")
    if run(["git", "rev-parse", "HEAD"], repo) != commit:
        raise ValueError("artifact source worktree does not match the expected commit")
    binary = binary.resolve(strict=True)
    if run([str(binary), "--version"], repo) != "feishu-bot " + version:
        raise ValueError("artifact executable version differs from expected identity")
    goos, goarch = run(["go", "env", "GOOS", "GOARCH"], repo).splitlines()
    with binary.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    info = {"version": version, "commit": commit, "os": goos, "arch": goarch, "binary_sha256": digest}
    output = output.resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(info, sort_keys=True) + "\n", encoding="utf-8")
    variables = {"PATH": str(binary), "VERSION": version, "COMMIT": commit,
                 "SHA256": digest, "BUILDINFO": str(output)}
    if any("\n" in value or "\r" in value for value in variables.values()):
        raise ValueError("artifact identity cannot contain environment line breaks")
    with open(os.environ["GITHUB_ENV"], "a", encoding="utf-8") as envfile:
        for key, value in variables.items():
            envfile.write(f"{prefix}_{key}={value}\n")
    print(json.dumps({"fixture": prefix, **info}, sort_keys=True))
    return info


def baseline(repo: Path, output: Path) -> None:
    output = output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    run(["git", "cat-file", "-e", BASELINE_COMMIT + "^{commit}"], repo)
    with tempfile.TemporaryDirectory(prefix="feishu-upgrade-source-") as directory:
        source = Path(directory) / "source"
        subprocess.run(["git", "worktree", "add", "--detach", str(source), BASELINE_COMMIT], cwd=repo, check=True)
        try:
            goos = run(["go", "env", "GOOS"], source)
            binary = output / ("feishu-bot.exe" if goos == "windows" else "feishu-bot")
            subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
                            "-X github.com/sean2077/feishu-bot/internal/cli.Version=" + BASELINE_VERSION,
                            "-o", str(binary), "./cmd/feishu-bot"], cwd=source,
                           env={**os.environ, "CGO_ENABLED": "0"}, check=True)
            record(binary, BASELINE_VERSION, BASELINE_COMMIT, output / "BUILDINFO.json",
                   "FEISHU_PREVIOUS_ARTIFACT", source)
        finally:
            subprocess.run(["git", "worktree", "remove", "--force", str(source)], cwd=repo, check=True)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path.cwd())
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("baseline").add_argument("--output", type=Path, required=True)
    identity = commands.add_parser("record")
    identity.add_argument("--binary", type=Path, required=True)
    identity.add_argument("--version", required=True)
    identity.add_argument("--commit", required=True)
    identity.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "baseline":
        baseline(args.repo.resolve(), args.output)
    else:
        record(args.binary, args.version, args.commit, args.output, "FEISHU_ARTIFACT", args.repo.resolve())


if __name__ == "__main__":
    main()
