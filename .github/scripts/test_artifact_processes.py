"""Real owned-process cleanup and failure-path contracts for artifact supervision."""

from __future__ import annotations

import ctypes
import io
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import artifact_processes as processes


SCRIPT = Path(processes.__file__).resolve()
FIXTURE = r'''
import json, os, pathlib, signal, subprocess, sys, time
role, directory, outcome = sys.argv[1:]
root = pathlib.Path(directory)
signal.signal(signal.SIGTERM, signal.SIG_IGN)
def spawn(role):
    options = {"process_group": 0} if os.name == "posix" else {"creationflags": 0x200}
    return subprocess.Popen([sys.executable, __file__, role, directory, outcome], **options)
if role == "command":
    spawn("service")
elif role == "service":
    spawn("native")
record = {"pid": os.getpid(), "sid": os.getsid(0) if os.name == "posix" else 0,
          "group": os.getpgrp() if os.name == "posix" else 0}
pending = root / (role + ".tmp")
pending.write_text(json.dumps(record))
pending.replace(root / (role + ".json"))
if role == "command":
    while not (root / "native.json").is_file():
        time.sleep(.01)
    (root / "ready").write_text("ready")
    if outcome == "exit":
        os._exit(17)
while True:
    time.sleep(1)
'''


def wait_until(predicate, timeout=8):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(.02)
    raise AssertionError("fixture did not reach its barrier before the deadline")


class ProcessTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="artifact-process-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.fixture = self.root / "process fixture.py"
        self.fixture.write_text(FIXTURE)
        self.children = []
        self.supervisors = []
        self.addCleanup(self.cleanup)

    def cleanup(self):
        # Failure cleanup is restricted to exact fixture PIDs whose handles were
        # captured while the processes were alive. It never matches names.
        for child in self.supervisors:
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=6)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=3)
            if child.stdout:
                child.stdout.close()
            if child.stderr:
                child.stderr.close()
        for pid, handle in self.children:
            if sys.platform == "linux":
                try:
                    signal.pidfd_send_signal(handle, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                os.close(handle)
            elif os.name == "nt":
                api = ctypes.WinDLL("kernel32", use_last_error=True)
                api.TerminateProcess.argtypes = [ctypes.c_void_p, ctypes.c_uint32]
                api.CloseHandle.argtypes = [ctypes.c_void_p]
                api.TerminateProcess(handle, 1)
                api.CloseHandle(handle)
            else:
                # Darwin fixtures are direct descendants of the supervisor and
                # normally already reaped. If it still owns this SID, kill it.
                try:
                    if os.getsid(pid) == handle:
                        os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass

    def start(self, outcome="hang", timeout=15):
        child = subprocess.Popen(
            [sys.executable, str(SCRIPT), "run", "--timeout", str(timeout),
             "--cleanup-timeout", "3", "--", sys.executable, str(self.fixture),
             "command", str(self.root), outcome],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        self.supervisors.append(child)
        wait_until(lambda: (self.root / "ready").is_file() or child.poll() is not None)
        if child.poll() is not None and not (self.root / "ready").is_file():
            output, error = child.communicate()
            self.fail(f"supervisor failed before fixture startup ({child.returncode}): {output} {error}")
        records = [json.loads((self.root / (role + ".json")).read_text())
                   for role in ("command", "service", "native")]
        if os.name == "posix":
            self.assertEqual(len({record["sid"] for record in records}), 1)
            self.assertEqual(len({record["group"] for record in records}), 3)
        for record in records:
            pid = record["pid"]
            if sys.platform == "linux":
                try:
                    handle = os.pidfd_open(pid)
                except ProcessLookupError:
                    continue  # The command may intentionally have exited.
            elif os.name == "nt":
                api = ctypes.WinDLL("kernel32", use_last_error=True)
                api.OpenProcess.argtypes = [ctypes.c_uint32, ctypes.c_int, ctypes.c_uint32]
                api.OpenProcess.restype = ctypes.c_void_p
                handle = api.OpenProcess(0x00100000 | 0x0001, False, pid)
                if not handle:
                    if outcome == "exit" and record is records[0]:
                        continue
                    self.fail(f"could not acquire fixture process handle for PID {pid}")
            else:
                handle = record["sid"]
            self.children.append((pid, handle))
        return child, records

    def assert_all_exited(self):
        for pid, handle in self.children:
            if sys.platform == "linux":
                self.assertTrue(select.select([handle], [], [], 3)[0], f"PID {pid} still executing")
            elif os.name == "nt":
                api = ctypes.WinDLL("kernel32", use_last_error=True)
                api.WaitForSingleObject.argtypes = [ctypes.c_void_p, ctypes.c_uint32]
                api.WaitForSingleObject.restype = ctypes.c_uint32
                self.assertEqual(api.WaitForSingleObject(handle, 3000), 0, f"PID {pid} still executing")
            else:
                def exited():
                    try:
                        return os.getsid(pid) != handle
                    except ProcessLookupError:
                        return True
                wait_until(exited, 3)

    def test_preserves_exit_code_cwd_environment_and_literal_arguments(self):
        destination = self.root / "result.json"
        code = ("import json,os,pathlib,sys; "
                "pathlib.Path('result.json').write_text(json.dumps([os.getcwd(),os.environ['FIXTURE_VALUE'],sys.argv[1]])); "
                "sys.exit(7)")
        result = processes.run_command(
            [sys.executable, "-c", code, "--timeout literal $value"], cwd=self.root,
            env={**os.environ, "FIXTURE_VALUE": "fixture value"}, timeout=5, cleanup_timeout=3,
        )
        self.assertEqual(result, 7)
        self.assertEqual(json.loads(destination.read_text()),
                         [str(self.root), "fixture value", "--timeout literal $value"])

    def test_hard_timeout_reclaims_service_and_native_groups(self):
        started = time.monotonic()
        supervisor, _ = self.start(timeout=3)
        _, error = supervisor.communicate(timeout=10)
        self.assertEqual(supervisor.returncode, processes.TIMEOUT, error)
        self.assertLess(time.monotonic() - started, 10)
        self.assert_all_exited()

    def test_command_crash_reclaims_remaining_descendants(self):
        supervisor, records = self.start()
        if os.name == "nt":
            os.kill(records[0]["pid"], signal.SIGTERM)
        else:
            os.kill(records[0]["pid"], signal.SIGKILL)
        _, error = supervisor.communicate(timeout=8)
        self.assertNotEqual(supervisor.returncode, 0, error)
        self.assertNotEqual(supervisor.returncode, processes.SUPERVISION_FAILED, error)
        self.assert_all_exited()

    def test_normal_command_exit_also_reclaims_descendants(self):
        supervisor, _ = self.start(outcome="exit")
        _, error = supervisor.communicate(timeout=8)
        self.assertEqual(supervisor.returncode, 17, error)
        self.assert_all_exited()

    @unittest.skipIf(os.name == "nt", "POSIX signal forwarding")
    def test_interrupt_cleans_owned_session_and_preserves_other_session(self):
        outsider = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"],
                                    start_new_session=True)
        try:
            supervisor, _ = self.start()
            supervisor.send_signal(signal.SIGTERM)
            _, error = supervisor.communicate(timeout=8)
            self.assertEqual(supervisor.returncode, 128 + signal.SIGTERM, error)
            self.assert_all_exited()
            self.assertIsNone(outsider.poll(), "cleanup touched an unrelated session")
        finally:
            outsider.kill()
            outsider.wait(timeout=3)

    def test_missing_command_is_a_supervision_failure(self):
        result = processes.run_command([str(self.root / "nonexistent-executable")],
                                       timeout=2, cleanup_timeout=2)
        self.assertEqual(result, processes.SUPERVISION_FAILED)


class FailureTests(unittest.TestCase):
    def test_invalid_limits_and_shell_strings_do_not_launch(self):
        for timeout in (0, -1, float("nan"), float("inf")):
            with self.subTest(timeout=timeout), mock.patch.object(subprocess, "Popen") as popen:
                with self.assertRaises(ValueError):
                    processes.run_command(["command"], timeout=timeout)
                with self.assertRaises(ValueError):
                    processes.run_command(["command"], cleanup_timeout=timeout)
                popen.assert_not_called()
        for command in ([], "command with shell arguments", [1]):
            with self.subTest(command=command), self.assertRaises(ValueError):
                processes.run_command(command)

    @unittest.skipUnless(sys.platform == "linux", "Linux pidfd contract")
    def test_reused_pid_identity_is_never_signalled(self):
        reader = processes._LinuxProcesses()
        owned = processes._Member(99, 42, (100,), True)
        replacements = [processes._Member(99, 42, (101,), True),
                        processes._Member(99, 43, (100,), True), None]
        for replacement in replacements:
            with self.subTest(replacement=replacement), \
                    mock.patch.object(os, "pidfd_open", return_value=123), \
                    mock.patch.object(os, "close") as close, \
                    mock.patch.object(reader, "read", return_value=replacement), \
                    mock.patch.object(signal, "pidfd_send_signal") as send:
                reader.signal(owned, signal.SIGKILL)
                send.assert_not_called()
                close.assert_called_once_with(123)

    @unittest.skipUnless(sys.platform == "linux", "Linux pidfd contract")
    def test_pidfd_signals_exact_identity_and_always_closes_descriptor(self):
        reader = processes._LinuxProcesses()
        member = processes._Member(99, 42, (100,), True)
        for error in (None, PermissionError("denied")):
            with self.subTest(error=error), \
                    mock.patch.object(os, "pidfd_open", return_value=123), \
                    mock.patch.object(os, "close") as close, \
                    mock.patch.object(reader, "read", return_value=member), \
                    mock.patch.object(signal, "pidfd_send_signal", side_effect=error) as send:
                if error:
                    with self.assertRaises(PermissionError):
                        reader.signal(member, signal.SIGKILL)
                else:
                    reader.signal(member, signal.SIGKILL)
                send.assert_called_once_with(123, signal.SIGKILL)
                close.assert_called_once_with(123)

    def test_cleanup_refuses_to_succeed_when_membership_is_unknown(self):
        session = object.__new__(processes._UnixSession)
        session.sid = 42
        with mock.patch.object(session, "members", side_effect=PermissionError("denied")), \
                mock.patch.object(processes, "_reap_children"), self.assertRaises(PermissionError):
            session.cleanup(.1)

    def test_failed_unix_cleanup_stops_known_child_and_returns_failure(self):
        session = mock.Mock()
        session.cleanup.side_effect = PermissionError("membership query denied")
        child = mock.Mock()
        child.poll.return_value = None
        with mock.patch.object(processes, "_UnixSession", return_value=session), \
                mock.patch.object(processes, "_adopt_linux_orphans"), \
                mock.patch.object(processes, "_reap_children"), \
                mock.patch.object(processes, "_wait_command", return_value=0), \
                mock.patch.object(subprocess, "Popen", return_value=child), \
                mock.patch.object(sys, "stderr", new_callable=io.StringIO):
            result = processes._session_main(["fixture"], 1, 1)
        self.assertEqual(result, processes.SUPERVISION_FAILED)
        child.kill.assert_called_once()
        child.wait.assert_called_once()

    def test_cleanup_refuses_to_succeed_when_members_remain(self):
        session = object.__new__(processes._UnixSession)
        session.sid = 42
        session.processes = mock.Mock()
        member = processes._Member(99, 42, (100,), True)
        with mock.patch.object(session, "members", return_value=[member]), \
                mock.patch.object(processes, "_reap_children"), \
                self.assertRaises(processes.SupervisionError):
            session.cleanup(.02)

    def test_empty_snapshot_is_rechecked_for_a_forked_replacement(self):
        session = object.__new__(processes._UnixSession)
        session.sid = 42
        session.processes = mock.Mock()
        member = processes._Member(99, 42, (100,), True)
        with mock.patch.object(session, "members", side_effect=[[], [member], [], []]), \
                mock.patch.object(processes, "_reap_children"):
            session.cleanup(1)
        session.processes.signal.assert_called_once_with(member, signal.SIGTERM)

    def test_native_structure_layouts_use_fixed_width_windows_and_darwin_types(self):
        self.assertEqual(ctypes.sizeof(processes._BSDInfo), 136)
        self.assertEqual(ctypes.sizeof(processes._ThreadEntry), 28)
        self.assertEqual(ctypes.sizeof(processes._JobAccounting), 48)
        if ctypes.sizeof(ctypes.c_void_p) == 8:
            self.assertEqual(ctypes.sizeof(processes._JobLimits), 144)


class WindowsContracts(unittest.TestCase):
    """Also run without Windows; real platform tests above exercise the native API."""

    def setUp(self):
        self.events = []
        self.child = mock.Mock(pid=71)
        self.child.poll.return_value = 0
        self.child.wait.return_value = 0
        self.child.kill.side_effect = lambda: self.events.append("kill-suspended")
        self.job = mock.Mock()
        self.job.attach.side_effect = lambda pid: self.events.append(("attach", pid))
        self.job.resume.side_effect = lambda pid: self.events.append(("resume", pid))
        self.job.terminate.side_effect = lambda: self.events.append("terminate-job")
        self.job.active.return_value = 0
        self.job.close.side_effect = lambda: self.events.append("close-job")

    def run_supervisor(self):
        def launch(*args, **kwargs):
            self.assertEqual(kwargs["creationflags"], 0x204)
            self.events.append("create-suspended")
            return self.child
        with mock.patch.object(processes, "_WindowsJob", return_value=self.job), \
                mock.patch.object(subprocess, "Popen", side_effect=launch), \
                mock.patch.object(sys, "stderr", new_callable=io.StringIO):
            return processes._run_windows(["fixture.exe"], cwd=None, env=None,
                                          timeout=.02, cleanup_timeout=.02)

    def test_resume_follows_successful_job_attachment(self):
        self.assertEqual(self.run_supervisor(), 0)
        self.assertEqual(self.events, ["create-suspended", ("attach", 71), ("resume", 71),
                                       "terminate-job", "close-job"])
        self.job.active.assert_called()

    def test_failed_attachment_never_resumes_and_kills_suspended_child(self):
        self.job.attach.side_effect = OSError("assignment failed")
        self.assertEqual(self.run_supervisor(), processes.SUPERVISION_FAILED)
        self.job.resume.assert_not_called()
        self.child.kill.assert_called_once()
        self.job.close.assert_called_once()

    def test_failed_resume_terminates_containing_job(self):
        self.job.resume.side_effect = OSError("resume failed")
        self.assertEqual(self.run_supervisor(), processes.SUPERVISION_FAILED)
        self.job.terminate.assert_called_once()
        self.job.close.assert_called_once()

    def test_hard_timeout_kills_job_and_waits_for_zero_members(self):
        self.child.poll.side_effect = [None, None, 1]
        self.job.active.side_effect = [1, 0]
        self.assertEqual(self.run_supervisor(), processes.TIMEOUT)
        self.job.terminate.assert_called_once()
        self.assertEqual(self.job.active.call_count, 2)
        self.child.wait.assert_called_once()

    def test_unconfirmed_query_termination_or_close_fails_the_gate(self):
        for operation in ("active", "terminate", "close"):
            with self.subTest(operation=operation):
                self.setUp()
                getattr(self.job, operation).side_effect = OSError("access denied")
                self.assertEqual(self.run_supervisor(), processes.SUPERVISION_FAILED)
                self.job.close.assert_called_once()

    def test_nonempty_job_at_deadline_is_a_failure(self):
        self.job.active.return_value = 1
        self.assertEqual(self.run_supervisor(), processes.SUPERVISION_FAILED)
        self.job.close.assert_called_once()


if __name__ == "__main__":
    unittest.main()
