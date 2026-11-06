"""Offline identity, hosted-runner guards and exact certificate cleanup tests."""

from __future__ import annotations

import base64
import hashlib
import json
import os
from pathlib import Path
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
            if "-addstore" in args:
                present = True
            if "-delstore" in args:
                present = False
            code = 0 if "-store" not in args or present else 1
            return subprocess.CompletedProcess(args, code, "", "")

        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(artifact_trust, "invoke", side_effect=invoke):
            record = artifact_trust.install(self.certificate)
            self.assertTrue(record.is_file())
            artifact_trust.cleanup(record)
        self.assertFalse(record.parent.exists())
        self.assertFalse(present)
        self.assertTrue(all(command[:2] == ["certutil", "-user"] for command in commands))
        self.assertIn(["certutil", "-user", "-delstore", "Root", hashlib.sha1(self.der).hexdigest()], commands)

    def test_changed_certificate_record_cannot_remove_an_unrelated_root(self):
        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(
                artifact_trust, "invoke", return_value=subprocess.CompletedProcess([], 0, "", "")) as command:
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
            if "remove-trusted-cert" in args:
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
        removes = [command for command in commands if "remove-trusted-cert" in command]
        self.assertEqual(removes, [["sudo", "-n", "/usr/bin/security", "remove-trusted-cert", "-d", str(record.parent / "certificate.pem")]])
        self.assertFalse(any("authorizationdb" in command for command in commands))

    def test_windows_failed_import_is_cleaned_before_reporting_the_failure(self):
        commands = []

        def invoke(args, check=True):
            commands.append(args)
            if "-addstore" in args:
                raise artifact_trust.TrustError("import denied")
            return subprocess.CompletedProcess(args, 1, "", "not found")

        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(artifact_trust, "invoke", side_effect=invoke):
            with self.assertRaisesRegex(artifact_trust.TrustError, "import denied"):
                artifact_trust.install(self.certificate)
        self.assertFalse(list((self.root / "feishu-artifact-trust").glob("*/record.json")))
        self.assertFalse(any("-delstore" in command for command in commands))


if __name__ == "__main__":
    unittest.main()
