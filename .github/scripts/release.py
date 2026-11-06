#!/usr/bin/env python3
"""Prepare and verify release payloads. Run from the repository root; no network I/O."""

from __future__ import annotations

import argparse
import datetime as dt
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import re
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import uuid
import xml.sax.saxutils
import zipfile


TARGETS = (
    ("linux", "amd64"), ("linux", "arm64"), ("windows", "amd64"),
    ("darwin", "amd64"), ("darwin", "arm64"),
)
INSTALLERS = {
    ("windows", "amd64"): "msi",
    ("linux", "amd64"): "deb",
    ("linux", "arm64"): "deb",
}
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


def run(args: list[str], *, cwd: Path, timeout: int = 30, env: dict[str, str] | None = None) -> str:
    try:
        result = subprocess.run(args, cwd=cwd, check=True, capture_output=True,
                                text=True, encoding="utf-8", timeout=timeout, env=env)
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


def installer_name(meta: dict, os_name: str, arch: str) -> str:
    kind = INSTALLERS.get((os_name, arch))
    if kind is None:
        raise ReleaseError(f"unsupported installer target: {os_name}/{arch}")
    return f"{target_name(meta, os_name, arch)}.{kind}"


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


def install_notes(meta: dict, os_name: str, arch: str, kind: str = "portable") -> bytes:
    command = ".\\feishu-bot.exe" if os_name == "windows" else "./feishu-bot"
    shell = "powershell" if os_name == "windows" else "sh"
    daemon_command = command
    if kind == "msi":
        title = "本安装包提供 Go 网关二进制"
        location = ("安装器把程序安装到 `%LOCALAPPDATA%\\feishu-bot`，只复制程序和文档，"
                    "不注册后台服务；按当前用户安装，不需要管理员权限。\n")
        commands = ('$install = "$env:LOCALAPPDATA\\feishu-bot"\n'
                    '& "$install\\feishu-bot.exe" --version\n'
                    '& "$install\\feishu-bot.exe" init --config "$install\\config.json"\n'
                    '& "$install\\feishu-bot.exe" doctor --config "$install\\config.json"\n'
                    '& "$install\\feishu-bot.exe" run --config "$install\\config.json"')
        uninstall = "在 Windows“设置 > 应用”中卸载 `feishu-bot`；卸载不会删除配置、状态库或日志。\n"
    elif kind == "deb":
        title = "本安装包提供 Go 网关二进制"
        location = ("安装器把程序安装到 `/usr/bin/feishu-bot`，文档位于 `/usr/share/doc/feishu-bot/`；"
                    "只复制程序和文档，不注册后台服务。\n")
        commands = ("feishu-bot --version\nfeishu-bot init\nfeishu-bot doctor\nfeishu-bot run")
        uninstall = "使用 `sudo dpkg -r feishu-bot` 卸载；卸载不会删除配置、状态库或日志。\n"
        daemon_command = "feishu-bot"
    else:
        title = "本包提供 Go 网关二进制"
        location = "校验并解压后，在解压目录执行以下命令。\n"
        commands = f"{command} --version\n{command} init\n{command} doctor\n{command} run"
        uninstall = ""
    daemon = ("macOS 当前使用前台 `run`；内置 `daemon` 管理暂不支持 macOS。\n"
              if os_name == "darwin" else
              f"需要常驻运行时，按部署文档配置环境后执行 `{daemon_command} daemon install`。\n")
    return f"""# feishu-bot {meta['tag']} ({os_name}/{arch})

{title}。部署需要另行安装 Git 和原生 Claude Code；Windows 使用 Git for Windows。
Node.js、npm 和 Go 编译器不是网关二进制的运行依赖。

{location}
```{shell}
{commands}
```

编辑新建的 `config.json`，填写应用、授权聊天、成员和模型配置，并通过环境变量配置凭据。
`config.example.json` 是参考模板；升级时保留原有配置和完整数据目录。

`doctor` 默认离线检查。真实飞书／模型验收需按文档单独进行。
{uninstall}{daemon}
- [部署与配置](https://github.com/sean2077/feishu-bot/blob/{meta['tag']}/docs/deployment.md)
- [迁移与回滚](https://github.com/sean2077/feishu-bot/blob/{meta['tag']}/docs/go-migration.md)
- [验证与验收](https://github.com/sean2077/feishu-bot/blob/{meta['tag']}/docs/validation.md)

源码提交：`{meta['commit']}`。本包内 `BUILDINFO.json` 记录版本、目标平台与二进制校验值。
""".encode()


def release_payload(meta: dict, binary_data: bytes, os_name: str, arch: str,
                    repo: Path, kind: str = "portable") -> dict[str, bytes]:
    check_binary(binary_data, os_name, arch)
    info = {**meta, "os": os_name, "arch": arch,
            "binary_sha256": hashlib.sha256(binary_data).hexdigest()}
    files = {binary_name(os_name): binary_data, "BUILDINFO.json": json_bytes(info),
             "INSTALL.md": install_notes(meta, os_name, arch, kind)}
    for name, relative in (("config.example.json", "examples/config.example.json"),
                           ("LICENSE", "LICENSE"), ("THIRD_PARTY_NOTICES.md", "THIRD_PARTY_NOTICES.md")):
        path = repo / relative
        if path.is_symlink() or not path.is_file():
            raise ReleaseError(f"package input must be a regular file: {relative}")
        files[name] = path.read_bytes()
    return files


def package(meta: dict, binary: Path, os_name: str, arch: str, output: Path, repo: Path) -> Path:
    validate_metadata(meta)
    prefix = target_name(meta, os_name, arch)
    if binary.is_symlink() or not binary.is_file():
        raise ReleaseError("production binary must be a regular file")
    files = release_payload(meta, binary.read_bytes(), os_name, arch, repo)
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


def validate_payload(files: dict[str, bytes], meta: dict, os_name: str, arch: str) -> None:
    info = parse_json(files["BUILDINFO.json"], "BUILDINFO.json")
    validate_metadata(info)
    if any(info.get(key) != value for key, value in meta.items()):
        raise ReleaseError("build identity differs from release metadata")
    if info.get("os") != os_name or info.get("arch") != arch:
        raise ReleaseError("payload target differs from build identity")
    digest = info.get("binary_sha256")
    if not isinstance(digest, str) or not SHA256.fullmatch(digest) or hashlib.sha256(files[binary_name(os_name)]).hexdigest() != digest:
        raise ReleaseError("binary checksum differs from BUILDINFO.json")
    check_binary(files[binary_name(os_name)], os_name, arch)
    if not isinstance(parse_json(files["config.example.json"], "config.example.json"), dict):
        raise ReleaseError("configuration example must be a JSON object")


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
    validate_payload(files, meta, os_name, arch)
    return files


def _ar_archive(entries: list[tuple[str, bytes]], epoch: int) -> bytes:
    blob = bytearray(b"!<arch>\n")
    for name, data in entries:
        if not name.endswith("/") or len(name) > 15:
            raise ReleaseError(f"invalid ar member name: {name}")
        header = (f"{name:<16}{epoch:<12}{0:<6}{0:<6}{'100644':<8}{len(data):<10}`\n").encode("ascii")
        blob += header + data
        if len(data) % 2:
            blob += b"\n"
    return bytes(blob)


def _tar_gz(epoch: int, entries: list[tuple[str, bytes | None, int]]) -> bytes:
    blob = io.BytesIO()
    with gzip.GzipFile(filename="", fileobj=blob, mode="wb", mtime=epoch, compresslevel=9) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            for name, data, mode in entries:
                member = tarfile.TarInfo(name)
                member.mtime = epoch
                member.uid = member.gid = 0
                member.uname = member.gname = ""
                member.mode = mode
                if data is None:
                    member.type = tarfile.DIRTYPE
                    archive.addfile(member)
                else:
                    member.type = tarfile.REGTYPE
                    member.size = len(data)
                    archive.addfile(member, io.BytesIO(data))
    return blob.getvalue()


def _deb_control(meta: dict, arch: str, installed_size: int) -> bytes:
    return f"""Package: feishu-bot
Version: {meta['version']}
Architecture: {arch}
Maintainer: feishu-bot maintainers <maintainers@example.com>
Installed-Size: {installed_size}
Section: net
Priority: optional
Homepage: https://github.com/sean2077/feishu-bot
Description: Feishu bot backed by native Claude Code
 feishu-bot connects Feishu chats to a persistent native Claude Code
 process. This package installs the gateway binary and documentation;
 Node.js and Claude Code are installed separately.
""".encode()


def _deb_data_tar(epoch: int, files: dict[str, bytes]) -> bytes:
    ordered: list[tuple[str, bytes | None, int]] = [("./", None, 0o755)]
    directories: set[str] = set()
    for name in sorted(files):
        parts = name.split("/")
        for index in range(1, len(parts)):
            directory = "./" + "/".join(parts[:index]) + "/"
            if directory not in directories:
                directories.add(directory)
                ordered.append((directory, None, 0o755))
        ordered.append(("./" + name, files[name], 0o755 if name == "usr/bin/feishu-bot" else 0o644))
    return _tar_gz(epoch, ordered)


def _md5sums(files: dict[str, bytes]) -> bytes:
    return "".join(
        f"{hashlib.md5(files[name], usedforsecurity=False).hexdigest()}  {name}\n"
        for name in sorted(files)
    ).encode()


def package_deb(meta: dict, binary: Path, arch: str, output: Path, repo: Path) -> Path:
    validate_metadata(meta)
    if INSTALLERS.get(("linux", arch)) != "deb":
        raise ReleaseError(f"unsupported installer target: linux/{arch}")
    if binary.is_symlink() or not binary.is_file():
        raise ReleaseError("production binary must be a regular file")
    files = release_payload(meta, binary.read_bytes(), "linux", arch, repo, "deb")
    data_files = {"usr/bin/feishu-bot": files["feishu-bot"]}
    for name in ("BUILDINFO.json", "INSTALL.md", "LICENSE", "THIRD_PARTY_NOTICES.md", "config.example.json"):
        data_files[f"usr/share/doc/feishu-bot/{name}"] = files[name]
    epoch = meta["source_date_epoch"]
    installed_size = max(1, (sum(len(data) for data in data_files.values()) + 1023) // 1024)
    control = _deb_control(meta, arch, installed_size)
    control_tar = _tar_gz(epoch, [("./", None, 0o755), ("./control", control, 0o644),
                                  ("./md5sums", _md5sums(data_files), 0o644)])
    data_tar = _deb_data_tar(epoch, data_files)
    path = output / installer_name(meta, "linux", arch)
    write_identical_or_new(path, _ar_archive(
        [("debian-binary/", b"2.0\n"), ("control.tar.gz/", control_tar), ("data.tar.gz/", data_tar)],
        epoch,
    ))
    read_deb(path, meta, arch)
    return path


def _read_ar(path: Path) -> dict[str, bytes]:
    data = path.read_bytes()
    if not data.startswith(b"!<arch>\n"):
        raise ReleaseError(f"not a Debian ar archive: {path.name}")
    offset, members = 8, {}
    while offset < len(data):
        if len(data) - offset < 60:
            raise ReleaseError(f"truncated ar header: {path.name}")
        header = data[offset:offset + 60]
        if header[58:60] != b"`\n":
            raise ReleaseError(f"invalid ar header: {path.name}")
        try:
            name = header[:16].decode("ascii").rstrip()
            size = int(header[48:58].decode("ascii").strip())
        except (UnicodeDecodeError, ValueError) as exc:
            raise ReleaseError(f"invalid ar header: {path.name}") from exc
        if not name.endswith("/"):
            raise ReleaseError(f"invalid ar member name: {name}")
        name = name[:-1]
        start, end = offset + 60, offset + 60 + size
        if size < 0 or end > len(data) or name in members:
            raise ReleaseError(f"invalid or duplicate ar member: {name}")
        members[name] = data[start:end]
        offset = end
        if size % 2:
            if offset >= len(data) or data[offset:offset + 1] != b"\n":
                raise ReleaseError(f"invalid ar padding: {path.name}")
            offset += 1
    return members


def _read_tar_gz(data: bytes, label: str) -> tuple[dict[str, bytes], dict[str, int]]:
    files: dict[str, bytes] = {}
    modes: dict[str, int] = {}
    try:
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
            for item in archive:
                name = item.name
                if (name not in (".", "./") and (not name.startswith("./")
                        or name.startswith("./../") or "\\" in name
                        or any(part == ".." for part in name.split("/")))):
                    raise ReleaseError(f"unsafe {label} member: {name}")
                if item.isdir():
                    continue
                if not item.isfile() or name in files:
                    raise ReleaseError(f"unexpected or duplicate {label} member: {name}")
                if not 0 < item.size <= 256 * 1024 * 1024:
                    raise ReleaseError(f"invalid {label} member size: {name}")
                stream = archive.extractfile(item)
                if stream is None:
                    raise ReleaseError(f"{label} member has no contents: {name}")
                files[name] = stream.read()
                modes[name] = stat.S_IMODE(item.mode)
    except (tarfile.TarError, EOFError, OSError) as exc:
        raise ReleaseError(f"cannot read {label}: {exc}") from exc
    return files, modes


def _deb_fields(data: bytes) -> dict[str, str]:
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ReleaseError("control file is not UTF-8") from exc
    fields: dict[str, str] = {}
    for line in text.splitlines():
        if not line or line[0] in " \t":
            continue
        if ":" not in line:
            raise ReleaseError("malformed control field")
        key, value = line.split(":", 1)
        if not re.fullmatch(r"[A-Za-z0-9-]+", key) or key in fields:
            raise ReleaseError(f"duplicate or invalid control field: {key}")
        fields[key] = value.strip()
    return fields


def read_deb(path: Path, meta: dict, arch: str) -> dict[str, bytes]:
    validate_metadata(meta)
    if path.name != installer_name(meta, "linux", arch) or path.is_symlink() or not path.is_file():
        raise ReleaseError("Debian package name, target or file type is incorrect")
    members = _read_ar(path)
    if set(members) != {"debian-binary", "control.tar.gz", "data.tar.gz"}:
        raise ReleaseError("Debian package does not contain the complete ar payload")
    if members["debian-binary"] != b"2.0\n":
        raise ReleaseError("unsupported Debian package format")
    control_files, _ = _read_tar_gz(members["control.tar.gz"], "control archive")
    if set(control_files) != {"./control", "./md5sums"}:
        raise ReleaseError("control archive must contain only control and md5sums")
    fields = _deb_fields(control_files["./control"])
    if fields.get("Package") != "feishu-bot":
        raise ReleaseError("Debian package name is not feishu-bot")
    if fields.get("Version") != meta["version"]:
        raise ReleaseError("Debian package version differs from release metadata")
    if fields.get("Architecture") != arch:
        raise ReleaseError("Debian package architecture differs from the target")
    if not fields.get("Description"):
        raise ReleaseError("Debian package has no description")
    data_files, modes = _read_tar_gz(members["data.tar.gz"], "data archive")
    bare = {name[2:]: data for name, data in data_files.items()}
    expected = {"usr/bin/feishu-bot", "usr/share/doc/feishu-bot/BUILDINFO.json",
                "usr/share/doc/feishu-bot/INSTALL.md", "usr/share/doc/feishu-bot/LICENSE",
                "usr/share/doc/feishu-bot/THIRD_PARTY_NOTICES.md",
                "usr/share/doc/feishu-bot/config.example.json"}
    if set(bare) != expected:
        raise ReleaseError("Debian data archive does not contain the complete payload")
    for name in sorted(bare):
        required = 0o755 if name == "usr/bin/feishu-bot" else 0o644
        if modes["./" + name] != required:
            raise ReleaseError(f"incorrect Debian payload permissions: {name}")
    if control_files["./md5sums"] != _md5sums(bare):
        raise ReleaseError("Debian md5sums do not match the payload")
    payload = {"feishu-bot": bare["usr/bin/feishu-bot"]}
    for name, data in bare.items():
        if name.startswith("usr/share/doc/feishu-bot/"):
            payload[name.rsplit("/", 1)[-1]] = data
    validate_payload(payload, meta, "linux", arch)
    return bare


def _wix_guid(*parts: str) -> str:
    return "{" + str(uuid.uuid5(uuid.NAMESPACE_URL, "/".join(("feishu-bot", *parts)))).upper() + "}"


def msi_version(version: str) -> str:
    parts = version.split(".")
    if len(parts) != 3:
        raise ReleaseError("MSI ProductVersion must have three numeric fields")
    try:
        major, minor, build = (int(part) for part in parts)
    except ValueError as exc:
        raise ReleaseError("MSI ProductVersion must have three numeric fields") from exc
    if major > 255 or minor > 255 or build > 65535:
        raise ReleaseError("version cannot be represented as an MSI ProductVersion")
    return version


def _filetime(epoch: int) -> bytes:
    return struct.pack("<Q", epoch * 10_000_000 + 116444736000000000)


def _cfb_chain(start: int, fat: list[int]) -> list[int]:
    chain: list[int] = []
    seen: set[int] = set()
    sector = start
    while sector not in (0xFFFFFFFE, 0xFFFFFFFF) and sector < len(fat):
        if sector in seen:
            raise ReleaseError("cyclic CFB sector chain")
        seen.add(sector)
        chain.append(sector)
        sector = fat[sector]
    return chain


def _cfb_regular(data: bytes, start: int, size: int, sector_size: int, fat: list[int]) -> bytearray:
    chunks = []
    for sector in _cfb_chain(start, fat):
        offset = (sector + 1) * sector_size
        if offset + sector_size > len(data):
            raise ReleaseError("CFB sector is outside the file")
        chunks.append(data[offset:offset + sector_size])
    blob = b"".join(chunks)
    if len(blob) < size:
        raise ReleaseError("CFB stream is shorter than its directory entry")
    return bytearray(blob[:size])


def _cfb_directory(data: bytes, first_dir: int, sector_size: int, fat: list[int]) -> list[dict]:
    entries = []
    for sector in _cfb_chain(first_dir, fat):
        base = (sector + 1) * sector_size
        if base + sector_size > len(data):
            raise ReleaseError("CFB directory sector is outside the file")
        for index in range(0, sector_size, 128):
            raw = data[base + index:base + index + 128]
            if len(raw) != 128:
                break
            name_len = struct.unpack_from("<H", raw, 64)[0]
            name = raw[:max(0, name_len - 2)].decode("utf-16le", errors="replace")
            entries.append({
                "name": name,
                "name_len": name_len,
                "start": struct.unpack_from("<I", raw, 116)[0],
                "size": struct.unpack_from("<Q", raw, 120)[0],
            })
    return entries


def _cfb_mini_stream(data: bytes, start: int, size: int, mini_size: int,
                     mini_fat: list[int], root_stream: bytearray) -> bytearray:
    chunks = []
    for sector in _cfb_chain(start, mini_fat):
        offset = sector * mini_size
        if offset + mini_size > len(root_stream):
            raise ReleaseError("CFB mini stream is outside the root stream")
        chunks.append(root_stream[offset:offset + mini_size])
    blob = b"".join(chunks)
    if len(blob) < size:
        raise ReleaseError("CFB mini stream is shorter than its directory entry")
    return bytearray(blob[:size])


def _patch_summary_times(stream: bytearray, stamp: bytes) -> int:
    if len(stream) < 28 or struct.unpack_from("<H", stream, 0)[0] != 0xFFFE:
        return 0
    patched = 0
    sections = struct.unpack_from("<I", stream, 24)[0]
    for index in range(sections):
        section_offset = struct.unpack_from("<I", stream, 28 + index * 20 + 16)[0]
        if section_offset + 8 > len(stream):
            continue
        properties = struct.unpack_from("<I", stream, section_offset + 4)[0]
        for prop in range(properties):
            pair = section_offset + 8 + prop * 8
            if pair + 8 > len(stream):
                continue
            value_at = section_offset + struct.unpack_from("<I", stream, pair + 4)[0]
            if value_at + 12 <= len(stream) and struct.unpack_from("<I", stream, value_at)[0] == 0x40:
                stream[value_at + 4:value_at + 12] = stamp
                patched += 1
    return patched


def normalize_msi(data: bytes, epoch: int) -> bytes:
    """Make WiX output reproducible by pinning CFB and SummaryInformation timestamps."""
    if len(data) < 512 or data[:8] != b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1":
        raise ReleaseError("installer is not a Windows Installer package")
    sector_size = 1 << struct.unpack_from("<H", data, 30)[0]
    mini_size = 1 << struct.unpack_from("<H", data, 32)[0]
    if sector_size not in (512, 4096) or mini_size != 64:
        raise ReleaseError("unsupported CFB sector layout")
    fat_sectors = struct.unpack_from("<I", data, 44)[0]
    first_dir = struct.unpack_from("<I", data, 48)[0]
    mini_cutoff = struct.unpack_from("<I", data, 56)[0]
    first_mini_fat = struct.unpack_from("<I", data, 60)[0]
    mini_fat_sectors = struct.unpack_from("<I", data, 64)[0]
    first_difat = struct.unpack_from("<I", data, 68)[0]
    difat_sectors = struct.unpack_from("<I", data, 72)[0]
    fat_ids = [value for value in struct.unpack_from("<109I", data, 76) if value < 0xFFFFFFFA]
    sector = first_difat
    for _ in range(difat_sectors):
        offset = (sector + 1) * sector_size
        if offset + sector_size > len(data):
            raise ReleaseError("CFB DIFAT sector is outside the file")
        entries = struct.unpack_from(f"<{sector_size // 4}I", data, offset)
        fat_ids.extend(value for value in entries[:-1] if value < 0xFFFFFFFA)
        sector = entries[-1]
        if sector in (0xFFFFFFFE, 0xFFFFFFFF):
            break
    if len(fat_ids) != fat_sectors:
        raise ReleaseError("CFB FAT is incomplete")
    fat: list[int] = []
    for sector in fat_ids:
        offset = (sector + 1) * sector_size
        if offset + sector_size > len(data):
            raise ReleaseError("CFB FAT sector is outside the file")
        fat.extend(struct.unpack_from(f"<{sector_size // 4}I", data, offset))
    entries = _cfb_directory(data, first_dir, sector_size, fat)
    if not entries:
        raise ReleaseError("CFB has no directory entries")
    root = entries[0]
    mini_fat: list[int] = []
    if mini_fat_sectors:
        for sector in _cfb_chain(first_mini_fat, fat):
            offset = (sector + 1) * sector_size
            if offset + sector_size > len(data):
                raise ReleaseError("CFB mini FAT is outside the file")
            mini_fat.extend(struct.unpack_from(f"<{sector_size // 4}I", data, offset))
    root_stream = _cfb_regular(data, root["start"], root["size"], sector_size, fat)
    stamp = _filetime(epoch)
    result = bytearray(data)
    summary = next((entry for entry in entries if entry["name"] == "\x05SummaryInformation"), None)
    if summary is None:
        raise ReleaseError("MSI has no SummaryInformation stream")
    if summary["size"] < mini_cutoff:
        stream = _cfb_mini_stream(data, summary["start"], summary["size"], mini_size, mini_fat, root_stream)
        _patch_summary_times(stream, stamp)
        for index, sector in enumerate(_cfb_chain(summary["start"], mini_fat)):
            offset = sector * mini_size
            result_stream = stream[index * mini_size:(index + 1) * mini_size]
            root_stream[offset:offset + len(result_stream)] = result_stream
        for index, sector in enumerate(_cfb_chain(root["start"], fat)):
            offset = (sector + 1) * sector_size
            chunk = root_stream[index * sector_size:(index + 1) * sector_size]
            result[offset:offset + len(chunk)] = chunk
    else:
        stream = _cfb_regular(data, summary["start"], summary["size"], sector_size, fat)
        _patch_summary_times(stream, stamp)
        for index, sector in enumerate(_cfb_chain(summary["start"], fat)):
            offset = (sector + 1) * sector_size
            chunk = stream[index * sector_size:(index + 1) * sector_size]
            result[offset:offset + len(chunk)] = chunk
    for sector in _cfb_chain(first_dir, fat):
        base = (sector + 1) * sector_size
        for index in range(0, sector_size, 128):
            if base + index + 128 > len(result):
                break
            if struct.unpack_from("<H", result, base + index + 64)[0]:
                result[base + index + 100:base + index + 108] = stamp
                result[base + index + 108:base + index + 116] = stamp
    return bytes(result)


def msi_source(meta: dict, arch: str, files: dict[str, bytes], source_dir: Path) -> bytes:
    msi_version(meta["version"])
    names = sorted(files)
    file_elements = "\n".join(
        f'              <File Id="File{index}" Source={xml.sax.saxutils.quoteattr(str(source_dir / name))} />'
        for index, name in enumerate(names, 1)
    )
    return f"""<?xml version="1.0" encoding="utf-8"?>
<Wix xmlns="http://schemas.microsoft.com/wix/2006/wi">
  <Product Id="{_wix_guid(meta['tag'], 'product')}" Name="feishu-bot" Language="1033"
           Version="{meta['version']}" Manufacturer="feishu-bot"
           UpgradeCode="{_wix_guid('upgrade')}">
    <Package Id="{_wix_guid(meta['tag'], 'package')}" InstallerVersion="500" Compressed="yes"
             InstallScope="perUser" InstallPrivileges="limited" />
    <MajorUpgrade DowngradeErrorMessage="A newer version of feishu-bot is already installed." />
    <MediaTemplate EmbedCab="yes" />
    <Directory Id="TARGETDIR" Name="SourceDir">
      <Directory Id="LocalAppDataFolder">
        <Directory Id="INSTALLFOLDER" Name="feishu-bot">
          <Component Id="MainComponent" Guid="{_wix_guid(meta['tag'], 'component')}">
            <RegistryKey Root="HKCU" Key="Software\\feishu-bot">
              <RegistryValue Name="InstallLocation" Type="string" Value="[INSTALLFOLDER]" KeyPath="yes" />
            </RegistryKey>
{file_elements}
            <RemoveFolder Id="RemoveInstallFolder" On="uninstall" />
          </Component>
        </Directory>
      </Directory>
    </Directory>
    <Feature Id="MainFeature" Title="feishu-bot" Level="1">
      <ComponentRef Id="MainComponent" />
    </Feature>
  </Product>
</Wix>
""".encode()


def check_msi(data: bytes) -> None:
    if len(data) < 512 or data[:8] != b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1":
        raise ReleaseError("installer is not a Windows Installer package")


def package_msi(meta: dict, binary: Path, arch: str, output: Path, repo: Path, wix_bin: Path) -> Path:
    validate_metadata(meta)
    if INSTALLERS.get(("windows", arch)) != "msi":
        raise ReleaseError(f"unsupported installer target: windows/{arch}")
    if binary.is_symlink() or not binary.is_file():
        raise ReleaseError("production binary must be a regular file")
    files = release_payload(meta, binary.read_bytes(), "windows", arch, repo, "msi")
    wix_bin = wix_bin.resolve()
    candle, light = wix_bin / "candle.exe", wix_bin / "light.exe"
    for tool in (candle, light):
        if not tool.is_file():
            raise ReleaseError(f"WiX executable is missing: {tool}")
    with tempfile.TemporaryDirectory(prefix="feishu-bot-msi-") as directory:
        root = Path(directory)
        for name, data in files.items():
            path = root / name
            path.write_bytes(data)
            os.utime(path, (meta["source_date_epoch"], meta["source_date_epoch"]))
        source = root / "feishu-bot.wxs"
        source.write_bytes(msi_source(meta, arch, files, root))
        os.utime(source, (meta["source_date_epoch"], meta["source_date_epoch"]))
        obj, msi = root / "feishu-bot.wixobj", root / "feishu-bot.msi"
        env = {**os.environ, "SOURCE_DATE_EPOCH": str(meta["source_date_epoch"]), "TZ": "UTC"}
        run([str(candle), "-nologo", "-arch", "x64", "-out", str(obj), str(source)],
            cwd=root, timeout=120, env=env)
        run([str(light), "-nologo", "-out", str(msi), str(obj)],
            cwd=root, timeout=120, env=env)
        data = normalize_msi(msi.read_bytes(), meta["source_date_epoch"])
    check_msi(data)
    path = output / installer_name(meta, "windows", arch)
    write_identical_or_new(path, data)
    return path


def artifact_journey(meta: dict, binary: Path, buildinfo: Path, repo: Path) -> None:
    """Run the native/full/upgrade Go fixtures against these exact release bytes."""
    info = parse_json(buildinfo.read_bytes(), "installed BUILDINFO.json")
    with binary.open("rb") as source:
        digest = hashlib.file_digest(source, "sha256").hexdigest()
    if (info.get("version"), info.get("commit"), info.get("binary_sha256")) != (meta["version"], meta["commit"], digest):
        raise ReleaseError("journey binary identity differs from the validated release metadata")
    if not os.environ.get("CLAUDE_NATIVE_PATH"):
        raise ReleaseError("release journey requires the pinned native CLI fixture")
    for field in ("PATH", "VERSION", "COMMIT", "SHA256", "BUILDINFO"):
        if not os.environ.get("FEISHU_PREVIOUS_ARTIFACT_" + field):
            raise ReleaseError("release journey requires the distinct previous binary upgrade fixture")
    env = {**os.environ, "FEISHU_ARTIFACT_PATH": str(binary.resolve()),
           "FEISHU_ARTIFACT_VERSION": meta["version"], "FEISHU_ARTIFACT_COMMIT": meta["commit"],
           "FEISHU_ARTIFACT_SHA256": digest, "FEISHU_ARTIFACT_BUILDINFO": str(buildinfo.resolve())}
    try:
        result = subprocess.run(["go", "test", "-race", "-v", "-count=1", "-timeout", "420s",
                                 "./internal/cli", "-run", "^TestBuiltArtifact"],
                                cwd=repo, env=env, timeout=450)
    except subprocess.TimeoutExpired as exc:
        raise ReleaseError("release artifact journey exceeded 450s") from exc
    if result.returncode:
        raise ReleaseError("actual release artifact failed its native/TLS/service upgrade journey")


def smoke_installer(meta: dict, installer: Path, os_name: str, arch: str, *, journey: bool = False) -> None:
    validate_metadata(meta)
    if installer.name != installer_name(meta, os_name, arch) or installer.is_symlink() or not installer.is_file():
        raise ReleaseError("installer name, target or file type is incorrect")
    installer = installer.resolve()
    host_os = {"Linux": "linux", "Windows": "windows"}.get(platform.system())
    host_arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine().lower())
    if (host_os, host_arch) != (os_name, arch):
        raise ReleaseError(f"installer smoke requires a native {os_name}/{arch} runner; found {host_os}/{host_arch}")
    kind = INSTALLERS.get((os_name, arch))
    with tempfile.TemporaryDirectory(prefix="feishu-bot-installer-smoke-") as directory:
        root = Path(directory)
        if kind == "deb":
            elevate = [] if os.geteuid() == 0 else ["sudo", "-n"]
            installed = False
            try:
                run([*elevate, "dpkg", "-i", str(installer)], cwd=root, timeout=120)
                installed = True
                binary = "/usr/bin/feishu-bot"
                version = run([binary, "--version"], cwd=root).strip()
                if version != f"feishu-bot {meta['version']}":
                    raise ReleaseError("installed binary reports a different version")
                if "feishu-bot" not in run([binary, "--help"], cwd=root):
                    raise ReleaseError("installed binary did not return CLI help")
                config = root / "isolated" / "config.json"
                run([binary, "init", "--config", str(config)], cwd=root)
                if not config.is_file():
                    raise ReleaseError("installed binary did not create a configuration")
                if journey:
                    artifact_journey(meta, Path(binary), Path("/usr/share/doc/feishu-bot/BUILDINFO.json"), Path.cwd())
            finally:
                if installed:
                    try:
                        run([*elevate, "dpkg", "-r", "feishu-bot"], cwd=root, timeout=120)
                    except ReleaseError:
                        pass
        elif kind == "msi":
            install_dir = Path(os.environ["LOCALAPPDATA"]) / "feishu-bot"
            log = root / "msiexec.log"
            result = subprocess.run(
                ["msiexec", "/i", str(installer), "/qn", "/norestart", "/L*v", str(log)],
                capture_output=True, text=True, encoding="utf-8", timeout=180)
            if result.returncode not in (0, 3010):
                raise ReleaseError(f"msiexec install failed ({result.returncode}); see {log}")
            try:
                binary = install_dir / "feishu-bot.exe"
                if not binary.is_file():
                    raise ReleaseError(f"MSI did not install the binary at {binary}")
                version = run([str(binary), "--version"], cwd=root).strip()
                if version != f"feishu-bot {meta['version']}":
                    raise ReleaseError("installed binary reports a different version")
                if "feishu-bot" not in run([str(binary), "--help"], cwd=root):
                    raise ReleaseError("installed binary did not return CLI help")
                config = root / "isolated" / "config.json"
                run([str(binary), "init", "--config", str(config)], cwd=root)
                if not config.is_file():
                    raise ReleaseError("installed binary did not create a configuration")
                if journey:
                    artifact_journey(meta, binary, install_dir / "BUILDINFO.json", Path.cwd())
            finally:
                subprocess.run(["msiexec", "/x", str(installer), "/qn", "/norestart"],
                               capture_output=True, text=True, encoding="utf-8", timeout=180)
        else:
            raise ReleaseError(f"unsupported installer kind: {os_name}/{arch}")


def smoke(meta: dict, archive: Path, os_name: str, arch: str, *, journey: bool = False) -> None:
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
        if journey:
            artifact_journey(meta, Path(binary), root / "BUILDINFO.json", Path.cwd())


def verify(meta: dict, artifacts: Path) -> dict:
    validate_metadata(meta)
    expected = {archive_name(meta, os_name, arch): ("archive", os_name, arch)
                for os_name, arch in TARGETS}
    for (os_name, arch), kind in INSTALLERS.items():
        expected[installer_name(meta, os_name, arch)] = (kind, os_name, arch)
    entries = {path.name: path for path in artifacts.iterdir()}
    if set(entries) - GENERATED != set(expected) or any(path.is_symlink() or not path.is_file() for path in entries.values()):
        raise ReleaseError("release artifacts must contain exactly the five portable archives and three installers")
    assets = []
    for name, (kind, os_name, arch) in sorted(expected.items()):
        path = entries[name]
        if kind == "archive":
            read_archive(path, meta, os_name, arch)
        elif kind == "deb":
            read_deb(path, meta, arch)
        elif kind == "msi":
            check_msi(path.read_bytes())
        else:
            raise ReleaseError(f"unsupported release artifact: {name}")
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
            command.add_argument("--journey", action="store_true", help="run exact-artifact native/TLS/upgrade journeys")
        else:
            command.add_argument("--artifacts", type=Path, required=True)
    installer = commands.add_parser("package-installer")
    installer.add_argument("--metadata", type=Path, required=True)
    installer.add_argument("--binary", type=Path, required=True)
    installer.add_argument("--os", dest="os_name", required=True)
    installer.add_argument("--arch", required=True)
    installer.add_argument("--output", type=Path, required=True)
    installer.add_argument("--wix-bin", type=Path)
    installer_smoke = commands.add_parser("smoke-installer")
    installer_smoke.add_argument("--metadata", type=Path, required=True)
    installer_smoke.add_argument("--installer", type=Path, required=True)
    installer_smoke.add_argument("--os", dest="os_name", required=True)
    installer_smoke.add_argument("--arch", required=True)
    installer_smoke.add_argument("--journey", action="store_true", help="run installed-artifact native/TLS/upgrade journeys")
    args = parser.parse_args(argv)
    try:
        if args.command == "prepare":
            prepare(args.tag, args.commit, args.output, Path.cwd())
        else:
            meta = load_metadata(args.metadata)
            if args.command == "package":
                print(package(meta, args.binary, args.os_name, args.arch, args.output, Path.cwd()))
            elif args.command == "smoke":
                smoke(meta, args.archive, args.os_name, args.arch, journey=args.journey)
            elif args.command == "package-installer":
                kind = INSTALLERS.get((args.os_name, args.arch))
                if kind == "deb":
                    print(package_deb(meta, args.binary, args.arch, args.output, Path.cwd()))
                elif kind == "msi":
                    if args.wix_bin is None:
                        raise ReleaseError("--wix-bin is required for an MSI")
                    print(package_msi(meta, args.binary, args.arch, args.output, Path.cwd(), args.wix_bin))
                else:
                    raise ReleaseError(f"unsupported installer target: {args.os_name}/{args.arch}")
            elif args.command == "smoke-installer":
                smoke_installer(meta, args.installer, args.os_name, args.arch, journey=args.journey)
            else:
                verify(meta, args.artifacts)
        return 0
    except (ReleaseError, OSError, struct.error) as exc:
        print(f"release: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
