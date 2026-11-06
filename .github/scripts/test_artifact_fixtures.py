"""Offline identity, hosted-runner guards and exact certificate cleanup tests."""

from __future__ import annotations

import base64
import hashlib
import json
import os
from pathlib import Path
import plistlib
import subprocess
import tempfile
import unittest
from unittest import mock

import artifact_fixture
import artifact_trust
import release


COMMIT = "1234567890abcdef1234567890abcdef12345678"


class ArtifactIdentityTests(unittest.TestCase):
    def test_record_binds_source_version_and_actual_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, manifest, envfile = root / "binary", root / "BUILDINFO.json", root / "env"
            binary.write_bytes(b"actual executable bytes")
            with mock.patch.dict(os.environ, {"GITHUB_ENV": str(envfile)}), mock.patch.object(
                    artifact_fixture, "run", side_effect=[COMMIT, "feishu-bot 1.2.3", "linux\namd64"]):
                info = artifact_fixture.record(binary, "1.2.3", COMMIT, manifest, "FEISHU_ARTIFACT", root)
            self.assertEqual(info["binary_sha256"], hashlib.sha256(binary.read_bytes()).hexdigest())
            self.assertEqual(json.loads(manifest.read_text()), info)
            self.assertIn(f"FEISHU_ARTIFACT_PATH={binary.resolve()}\n", envfile.read_text())
            self.assertIn(f"FEISHU_ARTIFACT_COMMIT={COMMIT}\n", envfile.read_text())
            self.assertIn(f"FEISHU_ARTIFACT_BUILDINFO={manifest.resolve()}\n", envfile.read_text())

    def test_wrong_source_commit_fails_before_executing_binary(self):
        with mock.patch.object(artifact_fixture, "run", return_value="0" * 40) as command:
            with self.assertRaisesRegex(ValueError, "source worktree"):
                artifact_fixture.record(Path("missing"), "1.2.3", COMMIT, Path("unused"), "FEISHU_ARTIFACT", Path.cwd())
            self.assertEqual(command.call_count, 1)

    def test_release_journey_uses_the_unpackaged_binary_identity_and_requires_upgrade(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, manifest = root / "feishu-bot", root / "BUILDINFO.json"
            binary.write_bytes(b"release bytes")
            digest = hashlib.sha256(binary.read_bytes()).hexdigest()
            info = {"version": "1.2.3", "commit": COMMIT, "binary_sha256": digest, "os": "linux", "arch": "amd64"}
            manifest.write_text(json.dumps(info))
            env = {"CLAUDE_NATIVE_PATH": "/pinned/native"}
            env.update({"FEISHU_PREVIOUS_ARTIFACT_" + field: "previous-" + field for field in ("PATH", "VERSION", "COMMIT", "SHA256", "BUILDINFO")})
            with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(
                    release.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)) as command:
                release.artifact_journey(info, binary, manifest, root)
                invocation = command.call_args
                self.assertEqual(invocation.kwargs["env"]["FEISHU_ARTIFACT_PATH"], str(binary.resolve()))
                self.assertEqual(invocation.kwargs["env"]["FEISHU_ARTIFACT_COMMIT"], COMMIT)
                self.assertEqual(invocation.kwargs["env"]["FEISHU_ARTIFACT_SHA256"], digest)
                self.assertIn("^TestBuiltArtifact", invocation.args[0])
                del os.environ["FEISHU_PREVIOUS_ARTIFACT_SHA256"]
                with self.assertRaisesRegex(release.ReleaseError, "previous binary"):
                    release.artifact_journey(info, binary, manifest, root)
            with mock.patch.object(release.subprocess, "run") as command:
                binary.write_bytes(b"different executable")
                with self.assertRaisesRegex(release.ReleaseError, "identity"):
                    release.artifact_journey(info, binary, manifest, root)
                command.assert_not_called()


class ArtifactTrustTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.environment = mock.patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted",
                                                      "FEISHU_ARTIFACT_SYSTEM_TRUST": "1", "RUNNER_TEMP": str(self.root)}, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)
        # Command seams are mocked. This is intentionally not a real CA and is
        # never imported into a development machine's certificate store.
        self.certificate = self.root / "input.pem"
        self.der = b"offline-fixture-certificate-bytes"
        self.certificate.write_text("-----BEGIN CERTIFICATE-----\n" + base64.b64encode(self.der).decode() + "\n-----END CERTIFICATE-----\n")

    def test_rejects_personal_and_self_hosted_environments_before_any_command(self):
        for key, value in (("GITHUB_ACTIONS", "false"), ("RUNNER_ENVIRONMENT", "self-hosted"), ("FEISHU_ARTIFACT_SYSTEM_TRUST", "0")):
            with self.subTest(key=key), mock.patch.dict(os.environ, {key: value}), mock.patch.object(artifact_trust, "invoke") as command:
                with self.assertRaisesRegex(artifact_trust.TrustError, "isolated GitHub-hosted"):
                    artifact_trust.install(self.certificate)
                command.assert_not_called()

    def test_windows_only_adds_and_removes_exact_current_user_fingerprint(self):
        commands, present = [], False

        def invoke(args, check=True):
            nonlocal present
            commands.append(args)
            operation = args[args.index("-Operation") + 1]
            if operation == "import":
                self.assertEqual(Path(args[args.index("-Certificate") + 1]).read_bytes(), self.der)
                present = True
            if operation == "remove":
                present = False
            return subprocess.CompletedProcess(args, 0, json.dumps({"present": present}), "")

        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(artifact_trust, "invoke", side_effect=invoke):
            record = artifact_trust.install(self.certificate)
            self.assertTrue(record.is_file())
            artifact_trust.cleanup(record)
        self.assertFalse(record.parent.exists())
        self.assertFalse(present)
        self.assertEqual([command[command.index("-Operation") + 1] for command in commands],
                         ["inspect", "import", "inspect", "remove", "inspect"])
        for command in commands:
            self.assertEqual(command[:5], ["powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-File"])
            self.assertEqual(command[command.index("-Thumbprint") + 1], hashlib.sha1(self.der).hexdigest())
            self.assertEqual(command[command.index("-Sha256") + 1], hashlib.sha256(self.der).hexdigest())

    def test_changed_certificate_record_cannot_remove_an_unrelated_root(self):
        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(
                artifact_trust, "windows_certificate", side_effect=[False, True]) as command:
            record = artifact_trust.install(self.certificate)
            state = json.loads(record.read_text())
            state["thumbprint"] = "0" * 40
            record.write_text(json.dumps(state))
            command.reset_mock()
            with self.assertRaisesRegex(artifact_trust.TrustError, "fingerprint"):
                artifact_trust.cleanup(record)
            command.assert_not_called()

    def test_macos_restores_search_list_and_removes_only_its_temporary_ca(self):
        commands, trusted = [], False
        original = "/original/login.keychain-db"

        def invoke(args, check=True):
            nonlocal trusted
            commands.append(args)
            stdout, code = "", 0
            if args[1:3] == ["list-keychains", "-d"] and "-s" not in args:
                stdout = '"' + original + '"\n'
            if "create-keychain" in args:
                Path(args[-1]).touch()
            if "add-trusted-cert" in args:
                trusted = True
            if "trust-settings-export" in args:
                entries = {"0" * 40: {"issuerName": b"unrelated-root", "trustSettings": []}}
                if trusted:
                    entries[hashlib.sha1(self.der).hexdigest().upper()] = {"issuerName": b"fixture-root"}
                Path(args[-1]).write_bytes(plistlib.dumps({"trustVersion": 1, "trustList": entries}))
            if "trust-settings-import" in args:
                settings = plistlib.loads(Path(args[-1]).read_bytes())
                self.assertEqual(settings, {"trustVersion": 1, "trustList": {"0" * 40: {"issuerName": b"unrelated-root", "trustSettings": []}}})
                trusted = False
            if "verify-cert" in args:
                code = 0 if trusted else 1
            if "delete-keychain" in args:
                Path(args[-1]).unlink()
            return subprocess.CompletedProcess(args, code, stdout, "")

        with mock.patch.object(artifact_trust.platform, "system", return_value="Darwin"), mock.patch.object(artifact_trust, "invoke", side_effect=invoke):
            record = artifact_trust.install(self.certificate)
            artifact_trust.cleanup_all()
        self.assertFalse(record.parent.exists())
        self.assertFalse(trusted)
        self.assertIn(["/usr/bin/security", "list-keychains", "-d", "user", "-s", original], commands)
        imports = [command for command in commands if "trust-settings-import" in command]
        self.assertEqual(imports, [["sudo", "-n", "/usr/bin/security", "trust-settings-import", "-d", str(record.parent / "trust-cleaned.plist")]])
        self.assertFalse(any("remove-trusted-cert" in command for command in commands))
        self.assertFalse(any("authorizationdb" in command for command in commands))

    def test_macos_privileged_security_child_has_its_own_timeout(self):
        args = ["sudo", "-n", "/usr/bin/security", "trust-settings-import", "-d", "/fixture.plist"]
        with mock.patch.object(artifact_trust.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "", "")) as command:
            artifact_trust.invoke(args)
        actual = command.call_args.args[0]
        self.assertEqual(actual[:4], ["sudo", "-n", "/usr/bin/perl", "-e"])
        self.assertIn("alarm 25", actual[4])
        self.assertEqual(actual[5:], ["--", *args[2:]])
        self.assertEqual(command.call_args.kwargs["timeout"], 30)

    def test_macos_changed_trust_settings_are_never_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            record = Path(directory) / "record.json"
            key = hashlib.sha1(self.der).hexdigest().upper()
            before = {"trustVersion": 1, "trustList": {key: {"issuerName": b"fixture"}}}
            changed = {"trustVersion": 1, "trustList": {**before["trustList"], "0" * 40: {"issuerName": b"new-root"}}}
            with mock.patch.object(artifact_trust, "macos_settings", side_effect=[before, changed]), mock.patch.object(artifact_trust, "invoke") as command:
                with self.assertRaisesRegex(artifact_trust.TrustError, "changed concurrently"):
                    artifact_trust.remove_macos_trust(record, key)
                command.assert_not_called()

    def test_windows_failed_import_is_cleaned_before_reporting_the_failure(self):
        commands = []

        def invoke(args, check=True):
            commands.append(args)
            if args[args.index("-Operation") + 1] == "import":
                raise artifact_trust.TrustError("import denied")
            return subprocess.CompletedProcess(args, 0, '{"present":false}', "")

        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(artifact_trust, "invoke", side_effect=invoke):
            with self.assertRaisesRegex(artifact_trust.TrustError, "import denied"):
                artifact_trust.install(self.certificate)
        self.assertFalse(list((self.root / "feishu-artifact-trust").glob("*/record.json")))
        self.assertFalse(any(command[command.index("-Operation") + 1] == "remove" for command in commands))

    def test_windows_access_denied_during_lookup_keeps_recovery_journal(self):
        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(
                artifact_trust, "windows_certificate", side_effect=[False, True]):
            record = artifact_trust.install(self.certificate)
        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(
                artifact_trust.subprocess, "run", return_value=subprocess.CompletedProcess([], 0x80070005, "", "access denied")) as command:
            for _ in range(2):
                with self.assertRaisesRegex(artifact_trust.TrustError, "CurrentUser Root inspect.*access denied"):
                    artifact_trust.cleanup(record)
                self.assertTrue(record.is_file())
            self.assertTrue(all(call.args[0][call.args[0].index("-Operation") + 1] == "inspect"
                                for call in command.call_args_list))

    def test_windows_preexisting_root_is_never_claimed_or_removed(self):
        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(
                artifact_trust, "windows_certificate", return_value=True) as command:
            with self.assertRaisesRegex(artifact_trust.TrustError, "already exists"):
                artifact_trust.install(self.certificate)
            self.assertEqual(command.call_count, 1)
            self.assertEqual(command.call_args.args[0], "inspect")

    def test_windows_missing_or_malformed_status_never_proves_absence(self):
        state = {"thumbprint": "a" * 40, "sha256": "b" * 64}
        for response in ("", "{}", '{"present":"false"}', '{"present":false,"error":"denied"}'):
            with self.subTest(response=response), mock.patch.object(artifact_trust, "invoke", return_value=
                    subprocess.CompletedProcess([], 0, response, "")):
                with self.assertRaisesRegex(artifact_trust.TrustError, "could not be confirmed"):
                    artifact_trust.windows_certificate("inspect", self.certificate, state)

    def test_failed_import_preserves_primary_error_and_cleanup_journal(self):
        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(
                artifact_trust, "windows_certificate", side_effect=[False, artifact_trust.TrustError("import timed out"),
                                                                     artifact_trust.TrustError("lookup denied")]):
            with self.assertRaisesRegex(artifact_trust.TrustError, "import timed out.*lookup denied"):
                artifact_trust.install(self.certificate)
        self.assertEqual(len(list((self.root / "feishu-artifact-trust").glob("*/record.json"))), 1)

    def test_timeout_identifies_operation_and_retains_bounded_os_diagnostic(self):
        args = ["powershell.exe", "-Operation", "import"]
        with mock.patch.object(artifact_trust.subprocess, "run", side_effect=
                subprocess.TimeoutExpired(args, 30, output=b"partial import diagnostic")):
            with self.assertRaisesRegex(artifact_trust.TrustError, "CurrentUser Root import: partial import diagnostic"):
                artifact_trust.invoke(args)


if __name__ == "__main__":
    unittest.main()
