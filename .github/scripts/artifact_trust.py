"""Temporary OS trust for actual-artifact TLS fixtures on disposable hosted CI.

This is test tooling, never shipped or consulted by the production executable.
No TLS verification or authorization policy is relaxed. A journal permits the
workflow's always-run cleanup to recover after an interrupted Go test process.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import re
import secrets
import shlex
import shutil
import ssl
import subprocess
import sys
import uuid


class TrustError(RuntimeError):
    pass


def hosted_root() -> Path:
    if (os.environ.get("GITHUB_ACTIONS") != "true"
            or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted"
            or os.environ.get("FEISHU_ARTIFACT_SYSTEM_TRUST") != "1"):
        raise TrustError("OS trust fixtures require explicit opt-in on an isolated GitHub-hosted runner")
    temporary = os.environ.get("RUNNER_TEMP", "")
    if not temporary or not Path(temporary).is_dir():
        raise TrustError("hosted runner temporary directory is missing")
    return Path(temporary).resolve() / "feishu-artifact-trust"


def macos_job_identity() -> dict[str, str]:
    """Bind an explicitly job-scoped CA to the documented disposable VM."""
    hosted_root()
    if (platform.system() != "Darwin" or os.environ.get("FEISHU_ARTIFACT_MACOS_TRUST_SCOPE") != "hosted_job"
            or os.environ.get("FEISHU_ARTIFACT_RUNNER_LABEL") not in ("macos-15", "macos-15-intel")):
        raise TrustError("macOS trust requires explicit hosted_job scope on a supported disposable macOS VM")
    identity = {name: os.environ.get(variable, "") for name, variable in (
        ("repository", "GITHUB_REPOSITORY"), ("workflow_ref", "GITHUB_WORKFLOW_REF"),
        ("run_id", "GITHUB_RUN_ID"), ("run_attempt", "GITHUB_RUN_ATTEMPT"),
        ("job", "GITHUB_JOB"), ("runner_label", "FEISHU_ARTIFACT_RUNNER_LABEL"),
    )}
    if (not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", identity["repository"])
            or not re.fullmatch(r"[1-9][0-9]{0,19}", identity["run_id"])
            or not re.fullmatch(r"[1-9][0-9]{0,9}", identity["run_attempt"])):
        raise TrustError("macOS hosted-job trust requires a valid workflow/run/attempt identity")
    workflow = re.fullmatch(re.escape(identity["repository"]) + r"/\.github/workflows/(ci|release)\.yml@refs/[A-Za-z0-9_./-]+",
                            identity["workflow_ref"])
    if not workflow or identity["job"] != {"ci": "cross-platform", "release": "build"}[workflow[1]]:
        raise TrustError("macOS hosted-job trust requires this repository's artifact verification job")
    return identity


def invoke(args: list[str], *, check: bool = True) -> subprocess.CompletedProcess:
    # Include the operation, never arbitrary arguments (a keychain command can
    # contain its password). Preserve bounded OS output on timeout as well.
    if args[:3] == ["sudo", "-n", "/usr/bin/security"]:
        operation = "security " + args[3]
    elif args[0] == "/usr/bin/security":
        operation = "security " + args[1]
    elif Path(args[0]).name == "powershell.exe" and "-Operation" in args:
        scope = args[args.index("-StoreScope") + 1] if "-StoreScope" in args else "CurrentUser"
        operation = scope + " Root " + args[args.index("-Operation") + 1]
    else:
        operation = Path(args[0]).name
    command = args
    if args[:3] == ["sudo", "-n", "/usr/bin/security"]:
        # Bound the privileged child itself. Killing only the unprivileged
        # sudo monitor on timeout can leave a root security process behind.
        # alarm survives exec; only our fixed security invocation is wrapped.
        command = ["sudo", "-n", "/usr/bin/perl", "-e", 'alarm 25; exec @ARGV or die "exec security failed";', "--", *args[2:]]
    try:
        result = subprocess.run(command, capture_output=True, text=True,
                                encoding="utf-8", errors="replace", timeout=30)
    except subprocess.TimeoutExpired as exc:
        diagnostic = exc.stderr or exc.stdout or ""
        if isinstance(diagnostic, bytes):
            diagnostic = diagnostic.decode("utf-8", errors="replace")
        raise TrustError(f"OS certificate operation timed out: {operation}: {diagnostic[-2000:]}") from exc
    if check and result.returncode:
        # Arguments can contain the ephemeral keychain password. Never print
        # them; the OS diagnostic is sufficient to identify an approval block.
        detail = result.stderr[-2000:] or result.stdout[-2000:]
        if result.returncode in (-14, 142) and args[:3] == ["sudo", "-n", "/usr/bin/security"]:
            detail = "privileged security child reached its 25s deadline; " + detail
        raise TrustError(f"OS certificate operation failed ({operation}, exit {result.returncode}): {detail}")
    return result


def windows_administrator() -> None:
    # The hosted job must already have the administrator token. Do not launch
    # an elevated process, request credentials, change policy or relax ACLs.
    script = "$ErrorActionPreference='Stop'; $p=[Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent()); @{administrator=$p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)} | ConvertTo-Json -Compress"
    result = invoke(["powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script])
    try:
        identity = json.loads(result.stdout)
    except ValueError as exc:
        raise TrustError("existing Windows administrator token could not be confirmed") from exc
    if not isinstance(identity, dict) or set(identity) != {"administrator"} or identity["administrator"] is not True:
        raise TrustError("temporary LocalMachine Root fixture requires an existing administrator token; no elevation attempted")


def windows_certificate(operation: str, certificate: Path, state: dict) -> bool:
    # certutil -store matches a CertId against every supported identifier and
    # may be slow on populated hosted-runner stores. The normal PowerShell
    # Certificate provider imports/removes the certificate; a read-only .NET
    # lookup matches only its exact thumbprint and verifies its SHA256 bytes.
    # No unprotected store flags, policy changes or chain-validation bypasses.
    # Old interrupted journals retain their original user-store scope.
    scope = state.get("windows_scope", "CurrentUser")
    if scope not in ("CurrentUser", "LocalMachine"):
        raise TrustError("unknown Windows certificate cleanup scope")
    helper = Path(__file__).with_name("artifact_windows_trust.ps1")
    result = invoke(["powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive",
                     "-File", str(helper), "-Operation", operation,
                     "-StoreScope", scope,
                     "-Certificate", str(certificate), "-Thumbprint", state["thumbprint"],
                     "-Sha256", state["sha256"]])
    if len(result.stdout) > 4096:
        raise TrustError(scope + " Root certificate response is too large")
    try:
        status = json.loads(result.stdout)
    except ValueError as exc:
        raise TrustError(scope + " Root certificate presence could not be confirmed") from exc
    if not isinstance(status, dict) or set(status) != {"present"} or type(status["present"]) is not bool:
        raise TrustError(scope + " Root certificate presence could not be confirmed")
    return status["present"]


def macos_settings(path: Path) -> dict:
    result = invoke(["/usr/bin/security", "trust-settings-export", "-d", str(path)], check=False)
    if result.returncode:
        if "No Trust Settings were found" in result.stderr:
            return {"trustVersion": 1, "trustList": {}}
        raise TrustError("cannot export administrator trust settings: " + result.stderr[-2000:])
    if not path.is_file() or path.stat().st_size > 16 << 20:
        raise TrustError("administrator trust-settings export is missing or too large")
    settings = plistlib.loads(path.read_bytes())
    if not isinstance(settings, dict) or settings.get("trustVersion") != 1 or not isinstance(settings.get("trustList"), dict):
        raise TrustError("unsupported administrator trust-settings schema")
    return settings


def remove_macos_trust(record: Path, thumbprint: str) -> None:
    # remove-trusted-cert hangs on affected hosted macOS 15 images. Use the
    # supported external representation API instead, removing only our exact
    # SHA1 key (Apple TrustSettingsSchema.h), never replacing a pre-test
    # snapshot that could discard unrelated newly added trust settings.
    before = macos_settings(record.parent / "trust-before.plist")
    key = thumbprint.upper()
    if key not in before["trustList"]:
        return
    expected = copy.deepcopy(before)
    del expected["trustList"][key]
    replacement = record.parent / "trust-cleaned.plist"
    replacement.write_bytes(plistlib.dumps(expected))
    replacement.chmod(0o600)
    if macos_settings(record.parent / "trust-recheck.plist") != before:
        raise TrustError("administrator trust settings changed concurrently; cleanup not applied")
    invoke(["sudo", "-n", "/usr/bin/security", "trust-settings-import", "-d", str(replacement)])
    if macos_settings(record.parent / "trust-after.plist") != expected:
        raise TrustError("administrator trust-settings cleanup could not be confirmed exactly")


def save(record: Path, state: dict) -> None:
    pending = record.with_suffix(".tmp")
    pending.write_text(json.dumps(state, sort_keys=True) + "\n", encoding="utf-8")
    pending.replace(record)


def load(record: Path) -> tuple[Path, dict]:
    root = hosted_root()
    record = record.resolve()
    if record.parent.parent != root or record.name != "record.json" or record.is_symlink():
        raise TrustError("certificate cleanup record is outside its fixture directory")
    state = json.loads(record.read_text(encoding="utf-8"))
    certificate = record.parent / "certificate.pem"
    if state.get("schema") != 1 or state.get("platform") != platform.system() or certificate.is_symlink():
        raise TrustError("certificate cleanup record does not match this runner")
    der = ssl.PEM_cert_to_DER_cert(certificate.read_text(encoding="ascii"))
    if hashlib.sha256(der).hexdigest() != state.get("sha256") or hashlib.sha1(der).hexdigest() != state.get("thumbprint"):
        raise TrustError("certificate cleanup fingerprint changed")
    scope = state.get("trust_scope", "fixture")
    if scope == "hosted_job":
        if state.get("job_identity") != macos_job_identity():
            raise TrustError("certificate journal belongs to a different hosted job or run attempt")
    elif scope != "fixture":
        raise TrustError("unknown certificate cleanup lifetime")
    return record, state


def install(certificate: Path) -> Path:
    root = hosted_root()
    operating_system = platform.system()
    if operating_system not in ("Windows", "Darwin"):
        raise TrustError("only Windows/macOS need OS trust installation")
    if operating_system == "Windows":
        windows_administrator()
    # This lifetime is chosen before any certificate/keychain mutation. It is
    # never a fallback after a failed attempt to revoke administrator trust.
    job_identity = macos_job_identity() if operating_system == "Darwin" else None
    pem = certificate.read_text(encoding="ascii")
    if len(pem) > 16384 or pem.count("-----BEGIN CERTIFICATE-----") != 1:
        raise TrustError("fixture must contain exactly one bounded CA certificate")
    der = ssl.PEM_cert_to_DER_cert(pem)
    entry = root / uuid.uuid4().hex
    entry.mkdir(parents=True, mode=0o700)
    local = entry / "certificate.pem"
    local.write_text(pem, encoding="ascii")
    local.chmod(0o600)
    record = entry / "record.json"
    state = {"schema": 1, "platform": operating_system,
             "sha256": hashlib.sha256(der).hexdigest(),
             "thumbprint": hashlib.sha1(der).hexdigest(),
             "trust_attempted": False, "keychain_created": False,
             "search_changed": False, "original_keychains": []}
    if operating_system == "Windows":
        # CurrentUser Root requires UI consent on the hosted image. Use the
        # documented administrator store with the existing authorized token.
        state["windows_scope"] = "LocalMachine"
    if job_identity is not None:
        state["trust_scope"] = "hosted_job"
        state["job_identity"] = job_identity
    save(record, state)
    try:
        if operating_system == "Windows":
            # Import a DER file through the documented PKIClient cmdlet. Keep
            # the PEM and both fingerprints for independently verified cleanup.
            encoded = entry / "certificate.cer"
            encoded.write_bytes(der)
            encoded.chmod(0o600)
            if windows_certificate("inspect", encoded, state):
                raise TrustError("fixture certificate already exists in LocalMachine Root")
            state["trust_attempted"] = True
            save(record, state)
            if not windows_certificate("import", encoded, state):
                raise TrustError("temporary certificate import could not be confirmed")
        else:
            security = "/usr/bin/security"
            keychain = entry / "fixture.keychain-db"
            state["original_keychains"] = shlex.split(invoke([security, "list-keychains", "-d", "user"]).stdout)
            password = secrets.token_hex(24)
            # Journal before each mutation so partial success can be cleaned.
            state["keychain_created"] = True
            save(record, state)
            invoke([security, "create-keychain", "-p", password, str(keychain)])
            invoke([security, "unlock-keychain", "-p", password, str(keychain)])
            state["search_changed"] = True
            save(record, state)
            invoke([security, "list-keychains", "-d", "user", "-s", *state["original_keychains"], str(keychain)])
            state["trust_attempted"] = True
            save(record, state)
            # The certificate lives in a dedicated user keychain. macOS's
            # documented admin trust domain avoids a user GUI authentication
            # prompt; sudo must already be allowed by the disposable runner.
            invoke(["sudo", "-n", security, "add-trusted-cert", "-d", "-r", "trustRoot",
                    "-p", "ssl", "-s", "open.feishu.cn", "-k", str(keychain), str(local)])
            invoke([security, "verify-cert", "-c", str(local), "-p", "ssl", "-s", "open.feishu.cn"])
        return record
    except BaseException as original:
        # Keep the journal if cleanup itself fails, so the workflow can retry.
        try:
            receipt = cleanup(record)
            if receipt is not None:
                print(json.dumps(receipt, sort_keys=True), file=sys.stderr)
        except Exception as recovery:
            raise TrustError(f"certificate install failed: {original}; cleanup failed: {recovery}") from original
        raise


def cleanup(record: Path) -> dict | None:
    record, state = load(record)
    if state.get("trust_scope") == "hosted_job":
        # A later failed sweep must not publish an earlier successful receipt
        # as evidence that user resources are still released.
        (record.parent / "receipt.json").unlink(missing_ok=True)
    local = record.parent / "certificate.pem"
    failures = []
    if state["platform"] == "Windows":
        if state["trust_attempted"]:
            encoded = record.parent / "certificate.cer"
            if windows_certificate("inspect", encoded, state):
                windows_certificate("remove", encoded, state)
            if windows_certificate("inspect", encoded, state):
                raise TrustError("temporary certificate remains in " + state.get("windows_scope", "CurrentUser") + " Root")
    else:
        security = "/usr/bin/security"
        # A hosted_job record deliberately retains administrator trust until
        # GitHub decommissions this VM. Requesting removal of the final entry
        # requires a different, interactive macOS authorization. Do not request
        # or bypass it, and do not report that trust was revoked synchronously.
        # Legacy records retain their original strict per-fixture behavior.
        job_scoped = state.get("trust_scope", "fixture") == "hosted_job"
        if state["trust_attempted"] and not job_scoped:
            try:
                remove_macos_trust(record, state["thumbprint"])
                if invoke([security, "verify-cert", "-c", str(local), "-p", "ssl", "-s", "open.feishu.cn"], check=False).returncode == 0:
                    failures.append("temporary CA remains trusted after removal")
            except TrustError as exc:
                failures.append(str(exc))
        if state["search_changed"]:
            try:
                invoke([security, "list-keychains", "-d", "user", "-s", *state["original_keychains"]])
                restored = shlex.split(invoke([security, "list-keychains", "-d", "user"]).stdout)
                if restored != state["original_keychains"]:
                    raise TrustError("original user keychain search list was not restored exactly")
                state["search_changed"] = False
                save(record, state)
            except TrustError as exc:
                failures.append(str(exc))
        keychain = record.parent / "fixture.keychain-db"
        if state["keychain_created"]:
            try:
                if keychain.exists() or keychain.is_symlink():
                    invoke([security, "delete-keychain", str(keychain)])
                if keychain.exists() or keychain.is_symlink():
                    raise TrustError("temporary user keychain removal could not be confirmed")
                state["keychain_created"] = False
                save(record, state)
            except TrustError as exc:
                failures.append(str(exc))
        if job_scoped:
            try:
                if keychain.exists() or keychain.is_symlink():
                    raise TrustError("temporary user keychain still exists")
                current = shlex.split(invoke([security, "list-keychains", "-d", "user"]).stdout)
                if any(Path(item).resolve() == keychain for item in current):
                    raise TrustError("temporary user keychain remains in the search list")
            except TrustError as exc:
                failures.append(str(exc))
    if failures:
        raise TrustError("certificate cleanup incomplete: " + "; ".join(failures))
    if state.get("trust_scope") == "hosted_job":
        # Keep a public-only receipt and journal for the always-run sweep and
        # CI artifact. The fixture's private key was never written here.
        state["administrator_trust"] = "awaiting_hosted_runner_teardown"
        save(record, state)
        receipt = {"schema": 1, "platform": "Darwin", "trust_scope": "hosted_job",
                   "job_identity": state["job_identity"], "sha256": state["sha256"], "thumbprint": state["thumbprint"],
                   "user_keychain": "removed", "user_search_list": "restored",
                   "administrator_trust": "awaiting_hosted_runner_teardown",
                   "revocation": "deferred to disposable runner teardown; not verified inside the job"}
        report = record.parent / "receipt.json"
        report.write_text(json.dumps(receipt, sort_keys=True) + "\n", encoding="utf-8")
        report.chmod(0o600)
        return receipt
    shutil.rmtree(record.parent)
    return None


def cleanup_all() -> None:
    root = hosted_root()
    failures = []
    for record in sorted(root.glob("*/record.json")):
        try:
            receipt = cleanup(record)
            if receipt is not None:
                print(json.dumps(receipt, sort_keys=True))
        except (TrustError, OSError, ValueError) as exc:
            failures.append(str(exc))
    if failures:
        raise TrustError("; ".join(failures))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("install").add_argument("--certificate", type=Path, required=True)
    commands.add_parser("cleanup").add_argument("--record", type=Path, required=True)
    commands.add_parser("cleanup-all")
    args = parser.parse_args()
    try:
        if args.command == "install":
            print(install(args.certificate))
        elif args.command == "cleanup":
            receipt = cleanup(args.record)
            if receipt is not None:
                print(json.dumps(receipt, sort_keys=True))
        else:
            cleanup_all()
    except (TrustError, OSError, ValueError) as exc:
        print(f"artifact trust: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
