#!/usr/bin/env python3
"""Prepare and verify release payloads. Run from the repository root; no network I/O."""

from __future__ import annotations

import argparse
import datetime as dt
import gzip
import hashlib
import io
import json
from pathlib import Path
import platform
import re
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import zipfile


TARGETS = (
    ("linux", "amd64"), ("linux", "arm64"), ("windows", "amd64"),
    ("darwin", "amd64"), ("darwin", "arm64"),
)
STABLE_TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
SHA = re.compile(r"[0-9a-f]{40}")
SHA256 = re.compile(r"[0-9a-f]{64}")
GENERATED = {"SHA256SUMS.txt", "release-manifest.json"}


class ReleaseError(ValueError):
    pass


def stable_version(tag: str) -> str:
    if not STABLE_TAG.fullmatch(tag):
        raise ReleaseError("tag must be a stable vMAJOR.MINOR.PATCH without leading zeroes")
    return tag[1:]


def json_bytes(value: object) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False) + "\n").encode()


def parse_json(data: bytes, label: str) -> object:
    def unique(pairs: list[tuple[str, object]]) -> dict:
        result = {}
        for key, value in pairs:
            if key in result:
                raise ReleaseError(f"duplicate JSON key in {label}: {key}")
            result[key] = value
        return result
    try:
        return json.loads(data, object_pairs_hook=unique)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ReleaseError(f"invalid JSON in {label}") from exc


def validate_metadata(meta: object) -> dict:
    if not isinstance(meta, dict) or type(meta.get("schema")) is not int or meta["schema"] != 1:
        raise ReleaseError("unsupported release metadata schema")
    tag, version, commit, epoch = (meta.get(k) for k in ("tag", "version", "commit", "source_date_epoch"))
    if not isinstance(tag, str) or version != stable_version(tag):
        raise ReleaseError("metadata version does not match tag")
    if not isinstance(commit, str) or not SHA.fullmatch(commit):
        raise ReleaseError("metadata requires a full lowercase commit SHA")
    if type(epoch) is not int or not 0 <= epoch <= 0xFFFFFFFF:
        raise ReleaseError("source_date_epoch must be an unsigned 32-bit integer")
    return meta


def load_metadata(path: Path) -> dict:
    return validate_metadata(parse_json(path.read_bytes(), str(path)))


def run(args: list[str], *, cwd: Path, timeout: int = 30) -> str:
    try:
        result = subprocess.run(args, cwd=cwd, check=True, capture_output=True,
                                text=True, encoding="utf-8", timeout=timeout)
    except subprocess.CalledProcessError as exc:
        detail = (exc.stderr or exc.stdout or "").strip()[:2000]
        raise ReleaseError(f"{Path(args[0]).name} failed ({exc.returncode}): {detail}") from exc
    except subprocess.TimeoutExpired as exc:
        raise ReleaseError(f"{Path(args[0]).name} exceeded {timeout}s") from exc
    return result.stdout


def extract_notes(text: str, version: str) -> str:
    """Read the project's unprefixed release heading, ignoring code-fenced examples."""
    lines = text.splitlines()
    headings = []
    fence = None
    for index, line in enumerate(lines):
        if fence:
            if re.fullmatch(r" {0,3}" + re.escape(fence[0]) + "{" + str(fence[1]) + r",}[ \t]*", line):
                fence = None
            continue
        opening = re.match(r"^ {0,3}(`{3,}|~{3,})(.*)$", line)
        if opening and not (opening[1][0] == "`" and "`" in opening[2]):
            fence = (opening[1][0], len(opening[1]))
            continue
        heading = re.match(r"^ {0,3}(#{1,2})(?:[ \t]+(.*)|[ \t]*)$", line)
        if heading:
            title = re.sub(r"[ \t]+#+[ \t]*$", "", heading[2] or "").strip()
            headings.append((index, len(heading[1]), title))
    identity = re.compile(r"(?:\[v?" + re.escape(version) + r"\]|v?" + re.escape(version) + r")(?=$|[\s—-])")
    candidates = [(index, title) for index, level, title in headings
                  if level == 2 and identity.match(title)]
    if len(candidates) != 1:
        raise ReleaseError(f"CHANGELOG must contain exactly one release section for {version}; found {len(candidates)}")
    start, title = candidates[0]
    heading = re.fullmatch(re.escape(version) + r" — ([0-9]{4}-[0-9]{2}-[0-9]{2})", title)
    if not heading:
        raise ReleaseError(f"release heading must be '## {version} — YYYY-MM-DD'")
    try:
        dt.date.fromisoformat(heading[1])
    except ValueError as exc:
        raise ReleaseError("release heading has an invalid calendar date") from exc
    end = next((index for index, _, _ in headings if index > start), len(lines))
    notes = "\n".join(lines[start + 1:end]).strip()
    if not notes:
        raise ReleaseError("release notes must not be empty")
    return notes + "\n"


def write_identical_or_new(path: Path, data: bytes) -> None:
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise ReleaseError(f"refusing non-regular output: {path.name}")
    if path.exists():
        if path.read_bytes() != data:
            raise ReleaseError(f"existing output differs: {path.name}")
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix=".release-", delete=False) as temp:
        temp.write(data)
        temp_path = Path(temp.name)
    try:
        temp_path.replace(path)
    finally:
        temp_path.unlink(missing_ok=True)


def prepare(tag: str, commit: str, output: Path, repo: Path) -> dict:
    version = stable_version(tag)
    if not SHA.fullmatch(commit):
        raise ReleaseError("--commit must be a full lowercase commit SHA")
    head = run(["git", "rev-parse", "--verify", "HEAD^{commit}"], cwd=repo).strip()
    tagged = run(["git", "rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}"], cwd=repo).strip()
    if head != commit or tagged != commit:
        raise ReleaseError("HEAD, version tag and requested release commit must be identical")
    if run(["git", "status", "--porcelain", "--untracked-files=normal"], cwd=repo).strip():
        raise ReleaseError("prepare requires a clean source checkout")
    changelog = run(["git", "show", f"{commit}:CHANGELOG.md"], cwd=repo)
    notes = extract_notes(changelog, version)
    epoch = int(run(["git", "show", "-s", "--format=%ct", commit], cwd=repo).strip())
    meta = validate_metadata({"schema": 1, "tag": tag, "version": version,
                              "commit": commit, "source_date_epoch": epoch})
    outputs = {"release.json": json_bytes(meta), "release-notes.md": notes.encode()}
    for name, data in outputs.items():
        path = output / name
        if path.exists() and (path.is_symlink() or not path.is_file() or path.read_bytes() != data):
            raise ReleaseError(f"existing output differs: {name}")
    for name, data in outputs.items():
        write_identical_or_new(output / name, data)
    return meta


def target_name(meta: dict, os_name: str, arch: str) -> str:
    if (os_name, arch) not in TARGETS:
        raise ReleaseError(f"unsupported release target: {os_name}/{arch}")
    return f"feishu-bot-{meta['tag']}-{os_name}-{arch}"


def archive_name(meta: dict, os_name: str, arch: str) -> str:
    return target_name(meta, os_name, arch) + (".zip" if os_name == "windows" else ".tar.gz")


def binary_name(os_name: str) -> str:
    return "feishu-bot.exe" if os_name == "windows" else "feishu-bot"


def check_binary(data: bytes, os_name: str, arch: str) -> None:
    valid = False
    if os_name == "linux" and len(data) >= 64:
        valid = (data[:6] == b"\x7fELF\x02\x01" and
                 struct.unpack_from("<H", data, 18)[0] == {"amd64": 62, "arm64": 183}[arch] and
                 struct.unpack_from("<H", data, 16)[0] in (2, 3))
    elif os_name == "darwin" and len(data) >= 32:
        valid = (data[:4] == b"\xcf\xfa\xed\xfe" and
                 struct.unpack_from("<I", data, 4)[0] == {"amd64": 0x01000007, "arm64": 0x0100000C}[arch] and
                 struct.unpack_from("<I", data, 12)[0] == 2)
    elif os_name == "windows" and len(data) >= 64 and data[:2] == b"MZ":
        offset = struct.unpack_from("<I", data, 60)[0]
        valid = (64 <= offset <= len(data) - 26 and data[offset:offset + 4] == b"PE\0\0" and
                 struct.unpack_from("<H", data, offset + 4)[0] == 0x8664 and
                 struct.unpack_from("<H", data, offset + 24)[0] == 0x20B)
    if not valid:
        raise ReleaseError(f"binary format or architecture does not match {os_name}/{arch}")


def install_notes(meta: dict, os_name: str, arch: str) -> bytes:
    command = ".\\feishu-bot.exe" if os_name == "windows" else "./feishu-bot"
    shell = "powershell" if os_name == "windows" else "sh"
    daemon = ("macOS 当前使用前台 `run`；内置 `daemon` 管理暂不支持 macOS。\n"
              if os_name == "darwin" else
              f"需要常驻运行时，按部署文档配置环境后执行 `{command} daemon install`。\n")
    return f"""# feishu-bot {meta['tag']} ({os_name}/{arch})

本包提供 Go 网关二进制。部署需要另行安装 Git 和原生 Claude Code；Windows 使用 Git for Windows。
Node.js、npm 和 Go 编译器不是网关二进制的运行依赖。

先校验下载的压缩包与 Release 附带的 `SHA256SUMS.txt`，解压后在本目录执行：

```{shell}
{command} --version
{command} init
```

编辑新建的 `config.json`，填写应用、授权聊天、成员和模型配置，并通过环境变量配置凭据。
`config.example.json` 是参考模板；升级时保留原有配置和完整数据目录。

```{shell}
{command} doctor
{command} run
```

`doctor` 默认离线检查。真实飞书／模型验收需按文档单独进行。
{daemon}
- [部署与配置](https://github.com/sean2077/feishu-bot/blob/{meta['tag']}/docs/deployment.md)
- [迁移与回滚](https://github.com/sean2077/feishu-bot/blob/{meta['tag']}/docs/go-migration.md)
- [验证与验收](https://github.com/sean2077/feishu-bot/blob/{meta['tag']}/docs/validation.md)

源码提交：`{meta['commit']}`。本包内 `BUILDINFO.json` 记录版本、目标平台与二进制校验值。
""".encode()


def package(meta: dict, binary: Path, os_name: str, arch: str, output: Path, repo: Path) -> Path:
    validate_metadata(meta)
    prefix = target_name(meta, os_name, arch)
    if binary.is_symlink() or not binary.is_file():
        raise ReleaseError("production binary must be a regular file")
    binary_data = binary.read_bytes()
    check_binary(binary_data, os_name, arch)
    info = {**meta, "os": os_name, "arch": arch,
            "binary_sha256": hashlib.sha256(binary_data).hexdigest()}
    files = {binary_name(os_name): binary_data, "BUILDINFO.json": json_bytes(info),
             "INSTALL.md": install_notes(meta, os_name, arch)}
    for name, relative in (("config.example.json", "examples/config.example.json"),
                           ("LICENSE", "LICENSE"), ("THIRD_PARTY_NOTICES.md", "THIRD_PARTY_NOTICES.md")):
        path = repo / relative
        if path.is_symlink() or not path.is_file():
            raise ReleaseError(f"package input must be a regular file: {relative}")
        files[name] = path.read_bytes()
    blob = io.BytesIO()
    epoch = meta["source_date_epoch"]
    if os_name == "windows":
        # ZIP timestamps have a 1980 lower bound and two-second precision.
        timestamp = dt.datetime.fromtimestamp(max(epoch, 315532800), dt.timezone.utc).timetuple()[:6]
        with zipfile.ZipFile(blob, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
            for name, data in sorted(files.items()):
                member = zipfile.ZipInfo(f"{prefix}/{name}", timestamp)
                member.create_system = 3
                member.external_attr = (stat.S_IFREG | 0o644) << 16
                member.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(member, data, compresslevel=9)
    else:
        with gzip.GzipFile(filename="", fileobj=blob, mode="wb", mtime=epoch, compresslevel=9) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                for name, data in sorted(files.items()):
                    member = tarfile.TarInfo(f"{prefix}/{name}")
                    member.size, member.mtime = len(data), epoch
                    member.mode = 0o755 if name == binary_name(os_name) else 0o644
                    archive.addfile(member, io.BytesIO(data))
    path = output / archive_name(meta, os_name, arch)
    write_identical_or_new(path, blob.getvalue())
    read_archive(path, meta, os_name, arch)
    return path


def read_archive(path: Path, meta: dict, os_name: str, arch: str) -> dict[str, bytes]:
    validate_metadata(meta)
    prefix = target_name(meta, os_name, arch)
    if path.name != archive_name(meta, os_name, arch) or path.is_symlink() or not path.is_file():
        raise ReleaseError("archive name, target or file type is incorrect")
    names = {binary_name(os_name), "BUILDINFO.json", "INSTALL.md", "config.example.json", "LICENSE", "THIRD_PARTY_NOTICES.md"}
    expected = {f"{prefix}/{name}": name for name in names}
    files = {}

    def member(name: str, size: int, mode: int, regular: bool) -> str:
        if name not in expected or expected[name] in files or not regular:
            raise ReleaseError(f"unexpected, duplicate or non-regular archive member: {name}")
        short = expected[name]
        limit = 256 * 1024 * 1024 if short == binary_name(os_name) else 4 * 1024 * 1024
        if not 0 < size <= limit:
            raise ReleaseError(f"invalid archive member size: {name}")
        required_mode = 0o755 if os_name != "windows" and short == binary_name(os_name) else 0o644
        if stat.S_IMODE(mode) != required_mode:
            raise ReleaseError(f"incorrect archive permissions: {name}")
        return short

    try:
        if os_name == "windows":
            with zipfile.ZipFile(path) as archive:
                for item in archive.infolist():
                    mode = item.external_attr >> 16
                    short = member(item.filename, item.file_size, mode,
                                   item.create_system == 3 and stat.S_ISREG(mode) and not item.is_dir())
                    files[short] = archive.read(item)
        else:
            with tarfile.open(path, "r:gz") as archive:
                for item in archive:
                    short = member(item.name, item.size, item.mode, item.isfile())
                    stream = archive.extractfile(item)
                    if stream is None:
                        raise ReleaseError("archive member has no contents")
                    files[short] = stream.read()
    except (tarfile.TarError, zipfile.BadZipFile, EOFError, OSError, RuntimeError) as exc:
        raise ReleaseError(f"cannot read archive {path.name}: {exc}") from exc
    if set(files) != names:
        raise ReleaseError(f"archive does not contain the complete payload: {path.name}")
    info = parse_json(files["BUILDINFO.json"], "BUILDINFO.json")
    validate_metadata(info)
    if any(info.get(key) != value for key, value in meta.items()):
        raise ReleaseError("archive build identity differs from release metadata")
    if info.get("os") != os_name or info.get("arch") != arch:
        raise ReleaseError("archive target differs from build identity")
    digest = info.get("binary_sha256")
    if not isinstance(digest, str) or not SHA256.fullmatch(digest) or hashlib.sha256(files[binary_name(os_name)]).hexdigest() != digest:
        raise ReleaseError("binary checksum differs from BUILDINFO.json")
    check_binary(files[binary_name(os_name)], os_name, arch)
    if not isinstance(parse_json(files["config.example.json"], "config.example.json"), dict):
        raise ReleaseError("configuration example must be a JSON object")
    return files


def smoke(meta: dict, archive: Path, os_name: str, arch: str) -> None:
    files = read_archive(archive, meta, os_name, arch)
    host_os = {"Linux": "linux", "Windows": "windows", "Darwin": "darwin"}.get(platform.system())
    host_arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine().lower())
    if (host_os, host_arch) != (os_name, arch):
        raise ReleaseError(f"smoke requires a native {os_name}/{arch} runner; found {host_os}/{host_arch}")
    with tempfile.TemporaryDirectory(prefix="feishu-bot-release-smoke-") as directory:
        root = Path(directory)
        for name, data in files.items():
            path = root / name
            path.write_bytes(data)
            if name == binary_name(os_name):
                path.chmod(0o755)
        binary = str(root / binary_name(os_name))
        version = run([binary, "--version"], cwd=root).strip()
        if version != f"feishu-bot {meta['version']}":
            raise ReleaseError("packaged binary reports a different version")
        if "feishu-bot" not in run([binary, "--help"], cwd=root):
            raise ReleaseError("packaged binary did not return CLI help")
        config_path = root / "isolated" / "config.json"
        run([binary, "init", "--config", str(config_path)], cwd=root)
        actual = parse_json(config_path.read_bytes(), "initialized configuration")
        if actual != parse_json(files["config.example.json"], "configuration example"):
            raise ReleaseError("packaged binary init differs from the bundled configuration example")


def verify(meta: dict, artifacts: Path) -> dict:
    validate_metadata(meta)
    expected = {archive_name(meta, os_name, arch): (os_name, arch) for os_name, arch in TARGETS}
    entries = {path.name: path for path in artifacts.iterdir()}
    if set(entries) - GENERATED != set(expected) or any(path.is_symlink() or not path.is_file() for path in entries.values()):
        raise ReleaseError("release artifacts must contain exactly the five expected platform archives")
    assets = []
    for name, (os_name, arch) in sorted(expected.items()):
        path = entries[name]
        read_archive(path, meta, os_name, arch)
        assets.append({"name": name, "sha256": hashlib.sha256(path.read_bytes()).hexdigest(), "size": path.stat().st_size})
    manifest = {"schema": 1, "tag": meta["tag"], "commit": meta["commit"], "assets": assets}
    outputs = {"release-manifest.json": json_bytes(manifest),
               "SHA256SUMS.txt": "".join(f"{asset['sha256']}  {asset['name']}\n" for asset in assets).encode()}
    for name, data in outputs.items():
        if (artifacts / name).exists() and (artifacts / name).read_bytes() != data:
            raise ReleaseError(f"existing {name} does not match the verified artifacts")
    for name, data in outputs.items():
        write_identical_or_new(artifacts / name, data)
    return manifest


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    prepare_parser = commands.add_parser("prepare")
    prepare_parser.add_argument("--tag", required=True)
    prepare_parser.add_argument("--commit", required=True)
    prepare_parser.add_argument("--output", type=Path, required=True)
    for name in ("package", "smoke", "verify"):
        command = commands.add_parser(name)
        command.add_argument("--metadata", type=Path, required=True)
        if name != "verify":
            command.add_argument("--os", dest="os_name", required=True)
            command.add_argument("--arch", required=True)
        if name == "package":
            command.add_argument("--binary", type=Path, required=True)
            command.add_argument("--output", type=Path, required=True)
        elif name == "smoke":
            command.add_argument("--archive", type=Path, required=True)
        else:
            command.add_argument("--artifacts", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "prepare":
            prepare(args.tag, args.commit, args.output, Path.cwd())
        else:
            meta = load_metadata(args.metadata)
            if args.command == "package":
                print(package(meta, args.binary, args.os_name, args.arch, args.output, Path.cwd()))
            elif args.command == "smoke":
                smoke(meta, args.archive, args.os_name, args.arch)
            else:
                verify(meta, args.artifacts)
        return 0
    except (ReleaseError, OSError) as exc:
        print(f"release: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
