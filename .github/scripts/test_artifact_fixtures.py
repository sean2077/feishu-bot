"""Offline identity, hosted-runner guards and exact certificate cleanup tests."""

from __future__ import annotations

import base64
import hashlib
import io
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
                    release.artifact_processes, "run_command", return_value=0) as command:
                release.artifact_journey(info, binary, manifest, root)
                invocation = command.call_args
                self.assertEqual(invocation.kwargs["env"]["FEISHU_ARTIFACT_PATH"], str(binary.resolve()))
                self.assertEqual(invocation.kwargs["env"]["FEISHU_ARTIFACT_COMMIT"], COMMIT)
                self.assertEqual(invocation.kwargs["env"]["FEISHU_ARTIFACT_SHA256"], digest)
                self.assertIn("^TestBuiltArtifact", invocation.args[0])
                self.assertEqual(invocation.kwargs["timeout"], 450)
                self.assertEqual(invocation.kwargs["cleanup_timeout"], 35)
                for result, message in ((124, "exceeded 450s"), (125, "cleanup could not be confirmed"),
                                        (2, "journey \\(exit 2\\)")):
                    command.return_value = result
                    with self.assertRaisesRegex(release.ReleaseError, message):
                        release.artifact_journey(info, binary, manifest, root)
                del os.environ["FEISHU_PREVIOUS_ARTIFACT_SHA256"]
                with self.assertRaisesRegex(release.ReleaseError, "previous binary"):
                    release.artifact_journey(info, binary, manifest, root)
            with mock.patch.object(release.artifact_processes, "run_command") as command:
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
                                                      "FEISHU_ARTIFACT_SYSTEM_TRUST": "1", "RUNNER_TEMP": str(self.root),
                                                      "FEISHU_ARTIFACT_MACOS_TRUST_SCOPE": "hosted_job",
                                                      "FEISHU_ARTIFACT_RUNNER_LABEL": "macos-15",
                                                      "GITHUB_REPOSITORY": "sean2077/feishu-bot",
                                                      "GITHUB_WORKFLOW_REF": "sean2077/feishu-bot/.github/workflows/ci.yml@refs/heads/main",
                                                      "GITHUB_RUN_ID": "12345", "GITHUB_RUN_ATTEMPT": "1",
                                                      "GITHUB_JOB": "cross-platform"}, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.administrator_check = artifact_trust.windows_administrator
        admin = mock.patch.object(artifact_trust, "windows_administrator")
        admin.start()
        self.addCleanup(admin.stop)
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

    def test_windows_only_adds_and_removes_exact_authorized_machine_fingerprint(self):
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
            self.assertEqual(command[command.index("-StoreScope") + 1], "LocalMachine")

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

    def test_legacy_macos_journal_is_not_upgraded_to_job_lifetime_by_environment(self):
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
            legacy = json.loads(record.read_text())
            del legacy["trust_scope"]
            del legacy["job_identity"]
            artifact_trust.save(record, legacy)
            artifact_trust.cleanup_all()
        self.assertFalse(record.parent.exists())
        self.assertFalse(trusted)
        self.assertIn(["/usr/bin/security", "list-keychains", "-d", "user", "-s", original], commands)
        imports = [command for command in commands if "trust-settings-import" in command]
        self.assertEqual(imports, [["sudo", "-n", "/usr/bin/security", "trust-settings-import", "-d", str(record.parent / "trust-cleaned.plist")]])
        self.assertFalse(any("remove-trusted-cert" in command for command in commands))
        self.assertFalse(any("authorizationdb" in command for command in commands))

    def macos_commands(self):
        state = {"commands": [], "search": ["/original/login.keychain-db"], "trusted": False,
                 "fail_restore": False, "fail_delete": False}
        def invoke(args, check=True):
            state["commands"].append(args)
            output = ""
            if "list-keychains" in args:
                if "-s" in args:
                    items = args[args.index("-s") + 1:]
                    if state["fail_restore"] and items == ["/original/login.keychain-db"]:
                        raise artifact_trust.TrustError("user search list restoration denied")
                    state["search"] = items
                else:
                    output = "\n".join('"' + item + '"' for item in state["search"])
            elif "create-keychain" in args:
                Path(args[-1]).touch()
            elif "add-trusted-cert" in args:
                state["trusted"] = True
            elif "delete-keychain" in args:
                if state["fail_delete"]:
                    raise artifact_trust.TrustError("user keychain deletion denied")
                Path(args[-1]).unlink()
            elif "trust-settings-import" in args or "remove-trusted-cert" in args:
                raise AssertionError("hosted_job lifetime must not request per-test administrator trust removal")
            return subprocess.CompletedProcess(args, 0, output, "")
        return state, invoke

    def test_macos_job_lifetime_releases_user_resources_and_reports_deferred_trust(self):
        state, invoke = self.macos_commands()
        with mock.patch.object(artifact_trust.platform, "system", return_value="Darwin"), mock.patch.object(
                artifact_trust, "invoke", side_effect=invoke):
            record = artifact_trust.install(self.certificate)
            receipt = artifact_trust.cleanup(record)
            self.assertTrue(state["trusted"])
            self.assertEqual(state["search"], ["/original/login.keychain-db"])
            self.assertFalse((record.parent / "fixture.keychain-db").exists())
            self.assertTrue(record.is_file())
            self.assertEqual(receipt["administrator_trust"], "awaiting_hosted_runner_teardown")
            self.assertEqual(receipt["job_identity"]["run_id"], "12345")
            self.assertEqual(json.loads((record.parent / "receipt.json").read_text()), receipt)
            self.assertNotIn("PRIVATE KEY", "".join(path.read_text() for path in record.parent.iterdir()))
            with mock.patch("sys.stdout", new_callable=io.StringIO) as output:
                artifact_trust.cleanup_all()
            self.assertEqual(json.loads(output.getvalue()), receipt)
        self.assertFalse(any("authorizationdb" in command for command in state["commands"]))
        self.assertEqual(sum("verify-cert" in command for command in state["commands"]), 1)

    def test_macos_job_lifetime_rejects_missing_scope_wrong_vm_and_incomplete_job_before_mutation(self):
        changes = (("FEISHU_ARTIFACT_MACOS_TRUST_SCOPE", ""), ("FEISHU_ARTIFACT_MACOS_TRUST_SCOPE", "fixture"),
                   ("FEISHU_ARTIFACT_RUNNER_LABEL", "macos-latest"), ("GITHUB_RUN_ID", ""),
                   ("GITHUB_RUN_ATTEMPT", "0"), ("GITHUB_JOB", "unrelated"),
                   ("GITHUB_WORKFLOW_REF", "other/project/.github/workflows/ci.yml@refs/heads/main"))
        for key, value in changes:
            with self.subTest(key=key, value=value), mock.patch.dict(os.environ, {key: value}), mock.patch.object(
                    artifact_trust.platform, "system", return_value="Darwin"), mock.patch.object(artifact_trust, "invoke") as command:
                with self.assertRaises(artifact_trust.TrustError):
                    artifact_trust.install(self.certificate)
                command.assert_not_called()
                self.assertFalse((self.root / "feishu-artifact-trust").exists())

    def test_macos_job_journal_cannot_be_replayed_in_another_run_attempt_job_or_workflow(self):
        state, invoke = self.macos_commands()
        with mock.patch.object(artifact_trust.platform, "system", return_value="Darwin"), mock.patch.object(
                artifact_trust, "invoke", side_effect=invoke) as command:
            record = artifact_trust.install(self.certificate)
            changes = ({"GITHUB_RUN_ID": "56789"}, {"GITHUB_RUN_ATTEMPT": "2"},
                       {"FEISHU_ARTIFACT_RUNNER_LABEL": "macos-15-intel"},
                       {"GITHUB_WORKFLOW_REF": "sean2077/feishu-bot/.github/workflows/release.yml@refs/tags/v1.2.3", "GITHUB_JOB": "build"})
            for env in changes:
                with self.subTest(env=env), mock.patch.dict(os.environ, env):
                    command.reset_mock()
                    with self.assertRaisesRegex(artifact_trust.TrustError, "different hosted job"):
                        artifact_trust.cleanup(record)
                    command.assert_not_called()
                    self.assertTrue(record.is_file())

    def test_macos_user_resource_cleanup_failure_still_fails_the_job_scoped_fixture(self):
        for failure in ("fail_restore", "fail_delete"):
            state, invoke = self.macos_commands()
            with self.subTest(failure=failure), mock.patch.object(artifact_trust.platform, "system", return_value="Darwin"), mock.patch.object(
                    artifact_trust, "invoke", side_effect=invoke):
                record = artifact_trust.install(self.certificate)
                state[failure] = True
                with self.assertRaisesRegex(artifact_trust.TrustError, "certificate cleanup incomplete"):
                    artifact_trust.cleanup(record)
                self.assertTrue(record.is_file())
                self.assertFalse((record.parent / "receipt.json").exists())

    def test_macos_always_sweep_detects_a_keychain_reintroduced_after_cleanup(self):
        state, invoke = self.macos_commands()
        with mock.patch.object(artifact_trust.platform, "system", return_value="Darwin"), mock.patch.object(
                artifact_trust, "invoke", side_effect=invoke):
            record = artifact_trust.install(self.certificate)
            artifact_trust.cleanup(record)
            state["search"].append(str(record.parent / "fixture.keychain-db"))
            with self.assertRaisesRegex(artifact_trust.TrustError, "remains in the search list"):
                artifact_trust.cleanup_all()
            self.assertFalse((record.parent / "receipt.json").exists())

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
                with self.assertRaisesRegex(artifact_trust.TrustError, "LocalMachine Root inspect.*access denied"):
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

    def test_windows_non_administrator_fails_before_any_trust_mutation_or_journal(self):
        with mock.patch.object(artifact_trust.platform, "system", return_value="Windows"), mock.patch.object(
                artifact_trust, "windows_administrator", wraps=self.administrator_check), mock.patch.object(
                artifact_trust, "invoke", return_value=subprocess.CompletedProcess([], 0, '{"administrator":false}', "")) as command:
            with self.assertRaisesRegex(artifact_trust.TrustError, "existing administrator token; no elevation attempted"):
                artifact_trust.install(self.certificate)
            self.assertEqual(command.call_count, 1)
            self.assertIn("-Command", command.call_args.args[0])
            self.assertFalse((self.root / "feishu-artifact-trust").exists())


if __name__ == "__main__":
    unittest.main()
