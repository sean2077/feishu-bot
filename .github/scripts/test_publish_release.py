"""Publisher state transitions, with no network or repository writes."""

import copy
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import publish_release as publisher


class FakeGitHub:
    def __init__(self, data):
        self.commit = data["commit"]
        self.current = None
        self.files = {}
        self.states = {}
        self.writes = []
        self.fail_upload = None
        self.move_after_upload = False

    def tag_commit(self, tag):
        return self.commit

    def release(self, tag):
        return copy.deepcopy(self.current)

    def create_draft(self, tag, body):
        self.writes.append("create")
        self.current = {"id": 1, "tag_name": tag, "body": body, "draft": True,
                        "prerelease": False, "html_url": "https://example.invalid/release"}

    def assets(self, release_id):
        return [{"id": name, "name": name, "state": self.states.get(name, "uploaded"),
                 "size": len(content)} for name, content in self.files.items()]

    def delete_failed_upload(self, asset):
        self.writes.append("delete-starter:" + asset["name"])
        self.files.pop(asset["name"])
        self.states.pop(asset["name"], None)

    def download(self, asset, output):
        output.write_bytes(self.files[asset["id"]])

    def upload(self, tag, path):
        self.writes.append("upload:" + path.name)
        if path.name == self.fail_upload:
            raise publisher.PublishError("simulated interrupted upload")
        if path.name in self.files:
            raise AssertionError("publisher attempted to overwrite an asset")
        self.files[path.name] = path.read_bytes()
        if self.move_after_upload:
            self.commit = "b" * 40

    def publish(self, release_id):
        self.writes.append("publish")
        self.current["draft"] = False


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.env = patch.dict(os.environ, {}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.root = Path(self.temp.name)
        self.artifacts = self.root / "dist"
        self.artifacts.mkdir()
        self.meta = {"schema": 1, "tag": "v1.2.3", "version": "1.2.3", "commit": "a" * 40}
        self.metadata = self.root / "release.json"
        self.metadata.write_text(json.dumps(self.meta), encoding="utf-8")
        self.notes = self.root / "notes.md"
        self.notes.write_text("Release fixture notes.\n", encoding="utf-8")
        assets = []
        names = [
            f"feishu-bot-v1.2.3-{target}.{'zip' if target.startswith('windows') else 'tar.gz'}"
            for target in ("linux-amd64", "linux-arm64", "windows-amd64", "darwin-amd64", "darwin-arm64")
        ] + [
            "feishu-bot-v1.2.3-windows-amd64.msi",
            "feishu-bot-v1.2.3-linux-amd64.deb",
            "feishu-bot-v1.2.3-linux-arm64.deb",
        ]
        for name in names:
            content = name.encode()
            (self.artifacts / name).write_bytes(content)
            assets.append({"name": name, "sha256": hashlib.sha256(content).hexdigest(), "size": len(content)})
        (self.artifacts / "release-manifest.json").write_text(
            json.dumps({"schema": 1, "tag": "v1.2.3", "commit": "a" * 40, "assets": assets}), encoding="utf-8")
        (self.artifacts / "SHA256SUMS.txt").write_text("".join(
            f"{item['sha256']}  {item['name']}\n" for item in sorted(assets, key=lambda item: item["name"])),
            encoding="utf-8")
        self.data = publisher.payload(self.metadata, self.notes, self.artifacts)
        self.api = FakeGitHub(self.data)

    def test_complete_assets_are_verified_before_publication(self):
        url = publisher.publish(self.api, self.data)
        self.assertEqual(url, "https://example.invalid/release")
        self.assertEqual(self.api.writes[0], "create")
        self.assertEqual(self.api.writes[-1], "publish")
        self.assertEqual(len(self.api.files), 10)
        self.assertFalse(self.api.current["draft"])

    def test_missing_installer_cannot_be_published_as_complete(self):
        (self.artifacts / "feishu-bot-v1.2.3-windows-amd64.msi").unlink()
        with self.assertRaisesRegex(publisher.PublishError, "Missing or unsafe local asset"):
            publisher.payload(self.metadata, self.notes, self.artifacts)

    def test_interrupted_draft_resumes_without_replacing_existing_files(self):
        names = sorted(self.data["hashes"])
        self.api.fail_upload = names[2]
        with self.assertRaisesRegex(publisher.PublishError, "interrupted"):
            publisher.publish(self.api, self.data)
        self.assertTrue(self.api.current["draft"])
        self.assertEqual(set(self.api.files), set(names[:2]))
        self.api.fail_upload = None
        self.api.writes.clear()
        publisher.publish(self.api, self.data)
        for name in names[:2]:
            self.assertNotIn("upload:" + name, self.api.writes)
        self.assertNotIn("create", self.api.writes)
        self.assertEqual(self.api.writes[-1], "publish")

    def test_published_rerun_is_read_only(self):
        publisher.publish(self.api, self.data)
        self.api.writes.clear()
        publisher.publish(self.api, self.data)
        self.assertEqual(self.api.writes, [])

    def test_zero_byte_starter_in_owned_draft_can_resume(self):
        self.api.create_draft(self.data["tag"], self.data["body"])
        name = sorted(self.data["hashes"])[0]
        self.api.files[name] = b""
        self.api.states[name] = "starter"
        self.api.writes.clear()
        publisher.publish(self.api, self.data)
        self.assertEqual(self.api.writes[0], "delete-starter:" + name)
        self.assertIn("upload:" + name, self.api.writes)
        self.assertEqual(self.api.writes[-1], "publish")

    def test_nonempty_starter_is_not_deleted(self):
        self.api.create_draft(self.data["tag"], self.data["body"])
        name = sorted(self.data["hashes"])[0]
        self.api.files[name] = b"uncertain partial content"
        self.api.states[name] = "starter"
        self.api.writes.clear()
        with self.assertRaisesRegex(publisher.PublishError, "upload is incomplete"):
            publisher.publish(self.api, self.data)
        self.assertEqual(self.api.writes, [])

    def test_published_missing_asset_fails_without_repair_or_overwrite(self):
        publisher.publish(self.api, self.data)
        self.api.files.pop(next(iter(self.api.files)))
        self.api.writes.clear()
        with self.assertRaisesRegex(publisher.PublishError, "missing required"):
            publisher.publish(self.api, self.data)
        self.assertEqual(self.api.writes, [])

    def test_existing_draft_with_different_identity_is_not_modified(self):
        self.api.create_draft(self.data["tag"], "Unrelated manually created draft")
        self.api.writes.clear()
        with self.assertRaisesRegex(publisher.PublishError, "different commit or payload"):
            publisher.publish(self.api, self.data)
        self.assertEqual(self.api.writes, [])

    def test_corrupt_existing_asset_stops_before_upload(self):
        self.api.create_draft(self.data["tag"], self.data["body"])
        name = next(iter(self.data["hashes"]))
        self.api.files[name] = b"wrong contents"
        self.api.writes.clear()
        with self.assertRaisesRegex(publisher.PublishError, "differs"):
            publisher.publish(self.api, self.data)
        self.assertEqual(self.api.writes, [])

    def test_unexpected_remote_assets_stop_before_upload(self):
        self.api.create_draft(self.data["tag"], self.data["body"])
        self.api.files["old-build.zip"] = b"stale"
        self.api.writes.clear()
        with self.assertRaisesRegex(publisher.PublishError, "unexpected remote"):
            publisher.publish(self.api, self.data)
        self.assertEqual(self.api.writes, [])

    def test_tag_move_before_or_during_upload_never_publishes(self):
        self.api.commit = "b" * 40
        with self.assertRaisesRegex(publisher.PublishError, "Remote tag moved"):
            publisher.publish(self.api, self.data)
        self.assertEqual(self.api.writes, [])
        self.api.commit = self.data["commit"]
        self.api.move_after_upload = True
        with self.assertRaisesRegex(publisher.PublishError, "Remote tag moved"):
            publisher.publish(self.api, self.data)
        self.assertNotIn("publish", self.api.writes)
        self.assertTrue(self.api.current["draft"])

    def test_local_tamper_and_wrong_workflow_commit_fail_before_network(self):
        name = next(name for name in self.data["hashes"] if name.endswith("tar.gz"))
        (self.artifacts / name).write_bytes(b"tampered")
        with self.assertRaisesRegex(publisher.PublishError, "does not match manifest"):
            publisher.payload(self.metadata, self.notes, self.artifacts)
        with patch.dict(os.environ, {"RELEASE_COMMIT": "b" * 40}):
            with self.assertRaisesRegex(publisher.PublishError, "workflow's commit"):
                publisher.payload(self.metadata, self.notes, self.artifacts)

    def test_annotated_event_sha_does_not_replace_prepared_commit(self):
        with patch.dict(os.environ, {"GITHUB_SHA": "c" * 40, "RELEASE_COMMIT": "a" * 40}):
            data = publisher.payload(self.metadata, self.notes, self.artifacts)
        self.assertEqual(data["commit"], "a" * 40)

    def test_checksum_file_must_match_manifest(self):
        (self.artifacts / "SHA256SUMS.txt").write_text("outdated\n", encoding="utf-8")
        with self.assertRaisesRegex(publisher.PublishError, "SHA256SUMS"):
            publisher.payload(self.metadata, self.notes, self.artifacts)

    def test_annotated_tags_are_peeled(self):
        api = publisher.GitHub("example/repository")
        with patch.object(api, "api", side_effect=[
            {"object": {"type": "tag", "sha": "c" * 40}},
            {"object": {"type": "commit", "sha": "a" * 40}},
        ]) as request:
            self.assertEqual(api.tag_commit("v1.2.3"), "a" * 40)
        self.assertEqual(request.call_args_list[1].args[0], "git/tags/" + "c" * 40)


if __name__ == "__main__":
    unittest.main()
