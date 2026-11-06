"""Bound artifact-test subprocesses, including children in separate process groups.

Unix runs the command under a dedicated, surviving session leader. Linux signals
use pidfds; macOS rechecks libproc start times and session membership. Windows
assigns the suspended command to a non-breakaway, kill-on-close Job before its
first instruction. Neither implementation searches by executable name.

This supervisor covers a crashed or timed-out test command. Killing the Unix
session supervisor itself with SIGKILL, deliberately detached sessions, and an
entire runner shutdown still require the disposable runner's teardown. Elevated
certificate helpers have their own deadline and precise certificate journal.

Use Python 3.12 from the verification workflow. Output is inherited. Exit codes
124 and 125 mean command timeout and unconfirmed supervision/cleanup respectively.
"""

from __future__ import annotations

import argparse
import ctypes
from dataclasses import dataclass
import errno
import math
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time


TIMEOUT = 124
SUPERVISION_FAILED = 125
POLL_INTERVAL = 0.05


class SupervisionError(RuntimeError):
    pass


def _report(message: str) -> None:
    print(f"artifact processes: {message}", file=sys.stderr, flush=True)


def _exit_code(code: int) -> int:
    return 128 - code if code < 0 else code


class _Interrupts:
    """Allow cleanup to finish after SIGINT/SIGTERM, including repeated signals."""

    def __init__(self):
        self.signal = 0
        self.previous = {}

    def __enter__(self):
        if threading.current_thread() is threading.main_thread():
            for number in (signal.SIGINT, signal.SIGTERM):
                self.previous[number] = signal.signal(number, self._receive)
        return self

    def _receive(self, number, _frame):
        if not self.signal:
            self.signal = number

    def __exit__(self, *_):
        for number, previous in self.previous.items():
            signal.signal(number, previous)


def _wait_command(child, timeout: float, interrupted: _Interrupts) -> int:
    deadline = time.monotonic() + timeout
    while True:
        code = child.poll()
        if code is not None:
            return _exit_code(code)
        if interrupted.signal:
            return 128 + interrupted.signal
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            _report(f"command exceeded {timeout:g}s; reclaiming owned processes")
            return TIMEOUT
        time.sleep(min(POLL_INTERVAL, remaining))


@dataclass(frozen=True)
class _Member:
    pid: int
    sid: int
    started: tuple[int, ...]
    active: bool


class _LinuxProcesses:
    def __init__(self):
        # Some container runners mount a parent namespace's /proc. Bind the
        # visible PIDs to this session's host SID, not repeating namespace PIDs.
        status = self._ids(Path("/proc/self/status"))
        self.host_pid = int(os.readlink("/proc/self"))
        pids = status.get("NSpid", [self.host_pid])
        matches = [index for index, pid in enumerate(pids) if pid == os.getpid()]
        if not matches:
            raise SupervisionError("cannot identify the mounted procfs namespace")
        self.namespace_index = matches[-1]
        fields = self._stat(Path("/proc/self/stat"), self.host_pid)
        self.host_sid = int(fields[3])
        self.visible_sid = os.getsid(0)
        self.paths = {os.getpid(): self.host_pid}

    @staticmethod
    def _ids(path):
        return {key: [int(value) for value in values.split()]
                for line in path.read_text().splitlines()
                for key, _, values in [line.partition(":")]
                if key in ("NSpid", "NSsid")}

    @staticmethod
    def _stat(path, pid):
        raw = path.read_text()
        end = raw.rfind(")")
        fields = raw[end + 2:].split()
        if end < 0 or len(fields) < 20 or int(raw.split("(", 1)[0]) != pid:
            raise SupervisionError(f"invalid process identity for PID {pid}")
        return fields

    def pids(self):
        paths = {}
        for path in Path("/proc").iterdir():
            if not path.name.isdigit():
                continue
            try:
                host_pid = int(path.name)
                fields = self._stat(path / "stat", host_pid)
                if int(fields[3]) != self.host_sid:
                    continue
                status = self._ids(path / "status")
                pids = status.get("NSpid", [host_pid])
                sessions = status.get("NSsid", [self.host_sid])
                if (self.namespace_index >= len(pids) or self.namespace_index >= len(sessions)
                        or sessions[self.namespace_index] != self.visible_sid):
                    raise SupervisionError("session member's PID namespace identity changed")
                paths[pids[self.namespace_index]] = host_pid
            except (FileNotFoundError, ProcessLookupError):
                continue
        self.paths = paths
        return list(paths)

    def read(self, pid: int) -> _Member | None:
        host_pid = self.paths.get(pid)
        if host_pid is None:
            return None
        try:
            fields = self._stat(Path(f"/proc/{host_pid}/stat"), host_pid)
        except (FileNotFoundError, ProcessLookupError):
            return None
        if int(fields[3]) != self.host_sid:
            return None
        # The process name can contain spaces and parentheses. Fields after the
        # final ')' start at stat's state (3), with session at 6/starttime at 22.
        state, threads, started = fields[0], int(fields[17]), int(fields[19])
        return _Member(pid, self.visible_sid, (started,), state not in ("Z", "X", "x") or threads > 1)

    def signal(self, member: _Member, number: int) -> None:
        try:
            descriptor = os.pidfd_open(member.pid, 0)
        except ProcessLookupError:
            return
        try:
            current = self.read(member.pid)
            if current is None or current.sid != member.sid or current.started != member.started:
                return
            # The descriptor pins the kernel process, so exit/PID reuse after
            # the identity check cannot redirect the signal to another process.
            signal.pidfd_send_signal(descriptor, number)
        except ProcessLookupError:
            pass
        finally:
            os.close(descriptor)


class _BSDInfo(ctypes.Structure):
    # Apple's sys/proc_info.h, PROC_PIDTBSDINFO. Use fixed-width types on both
    # supported Darwin architectures; pid start times include microseconds.
    _fields_ = [(name, ctypes.c_uint32) for name in (
        "flags", "status", "xstatus", "pid", "ppid", "uid", "gid", "ruid",
        "rgid", "svuid", "svgid", "reserved",
    )] + [
        ("comm", ctypes.c_char * 16), ("name", ctypes.c_char * 32),
    ] + [(name, ctypes.c_uint32) for name in (
        "nfiles", "pgid", "jobc", "tdev", "tpgid",
    )] + [
        ("nice", ctypes.c_int32), ("start_sec", ctypes.c_uint64),
        ("start_usec", ctypes.c_uint64),
    ]


class _DarwinProcesses:
    def __init__(self):
        self.lib = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
        self.lib.proc_listallpids.argtypes = [ctypes.c_void_p, ctypes.c_int]
        self.lib.proc_listallpids.restype = ctypes.c_int
        self.lib.proc_pidinfo.argtypes = [
            ctypes.c_int, ctypes.c_int, ctypes.c_uint64, ctypes.c_void_p, ctypes.c_int,
        ]
        self.lib.proc_pidinfo.restype = ctypes.c_int

    def pids(self):
        count = self.lib.proc_listallpids(None, 0)
        if count <= 0:
            raise SupervisionError("cannot enumerate macOS processes")
        capacity = count + 128
        for _ in range(4):
            buffer = (ctypes.c_int * capacity)()
            count = self.lib.proc_listallpids(buffer, ctypes.sizeof(buffer))
            if count < 0:
                raise SupervisionError("cannot enumerate macOS processes")
            if count < capacity:
                return [pid for pid in buffer[:count] if pid > 0]
            capacity *= 2
        raise SupervisionError("macOS process snapshot did not fit its buffer")

    def read(self, pid: int) -> _Member | None:
        info = _BSDInfo()
        ctypes.set_errno(0)
        size = self.lib.proc_pidinfo(pid, 3, 0, ctypes.byref(info), ctypes.sizeof(info))
        if size != ctypes.sizeof(info):
            if size == 0 and ctypes.get_errno() in (errno.ESRCH, errno.ENOENT):
                return None
            raise SupervisionError(f"cannot read macOS identity for PID {pid}")
        try:
            sid = os.getsid(pid)
        except ProcessLookupError:
            return None
        if info.pid != pid or not info.start_sec or not 1 <= info.status <= 5:
            raise SupervisionError(f"invalid macOS identity for PID {pid}")
        return _Member(pid, sid, (info.start_sec, info.start_usec), info.status != 5)

    def signal(self, member: _Member, number: int) -> None:
        current = self.read(member.pid)
        if current is None or current.sid != member.sid or current.started != member.started:
            return
        try:
            # POSIX has no pidfd on Darwin. Check both current SID and libproc
            # creation identity immediately before the PID-targeted signal.
            if os.getsid(member.pid) == member.sid:
                os.kill(member.pid, number)
        except ProcessLookupError:
            pass


def _reap_children() -> None:
    while True:
        try:
            pid, _ = os.waitpid(-1, os.WNOHANG)
        except ChildProcessError:
            return
        if pid == 0:
            return


def _adopt_linux_orphans() -> None:
    if sys.platform != "linux":
        return
    # The session helper also reaps grandchildren after a Go test panic. This
    # matters on containers whose PID 1 does not promptly collect zombies.
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.prctl(36, 1, 0, 0, 0) != 0:  # PR_SET_CHILD_SUBREAPER
        raise OSError(ctypes.get_errno(), "cannot become artifact child subreaper")


class _UnixSession:
    def __init__(self):
        self.sid = os.getpid()
        if os.getsid(0) != self.sid:
            raise SupervisionError("artifact helper must own a new session")
        if sys.platform == "linux":
            self.processes = _LinuxProcesses()
        elif sys.platform == "darwin":
            self.processes = _DarwinProcesses()
        else:
            raise SupervisionError(f"unsupported Unix platform: {sys.platform}")
        self.leader = self.processes.read(self.sid)
        if self.leader is None or self.leader.sid != self.sid or not self.leader.active:
            raise SupervisionError("cannot confirm session leader identity")

    def members(self) -> list[_Member]:
        current = self.processes.read(self.sid)
        if current != self.leader:
            raise SupervisionError("artifact session leader identity changed")
        members = []
        for pid in self.processes.pids():
            if pid == self.sid:
                continue
            try:
                if os.getsid(pid) != self.sid:
                    continue
            except ProcessLookupError:
                continue
            member = self.processes.read(pid)
            if member is not None and member.sid == self.sid and member.active:
                members.append(member)
        return members

    def cleanup(self, timeout: float) -> None:
        deadline = time.monotonic() + timeout
        graceful = time.monotonic() + min(2.0, timeout / 3)
        term_sent = set()
        last_error = None
        empty_snapshots = 0
        while True:
            _reap_children()
            members = self.members()
            now = time.monotonic()
            if not members:
                # Re-enumerate after an empty result: an exiting parent may
                # have forked its replacement during the previous snapshot.
                empty_snapshots += 1
                if empty_snapshots == 2:
                    return
                if now >= deadline:
                    raise SupervisionError("deadline expired before confirming an empty session")
                time.sleep(min(POLL_INTERVAL, deadline - now))
                continue
            empty_snapshots = 0
            if now >= deadline:
                detail = f"; last signal error: {last_error}" if last_error else ""
                raise SupervisionError(
                    f"session {self.sid} still has executing PIDs "
                    f"{[member.pid for member in members]} after {timeout:g}s{detail}"
                )
            number = signal.SIGTERM if now < graceful else signal.SIGKILL
            for member in members:
                identity = (member.pid, member.started)
                if number == signal.SIGTERM and identity in term_sent:
                    continue
                try:
                    self.processes.signal(member, number)
                    term_sent.add(identity)
                except OSError as exc:
                    # macOS can briefly report EPERM for an exiting process.
                    # Only a subsequent empty membership check confirms exit.
                    last_error = exc
            time.sleep(min(POLL_INTERVAL, max(0.0, deadline - time.monotonic())))


def _session_main(argv: list[str], timeout: float, cleanup_timeout: float) -> int:
    child = None
    session = None
    result = SUPERVISION_FAILED
    with _Interrupts() as interrupted:
        try:
            session = _UnixSession()
            _adopt_linux_orphans()
            if interrupted.signal:
                return 128 + interrupted.signal
            child = subprocess.Popen(argv)
            result = _wait_command(child, timeout, interrupted)
        except (OSError, SupervisionError) as exc:
            _report(str(exc))
        finally:
            if session is not None:
                try:
                    session.cleanup(cleanup_timeout)
                except (OSError, SupervisionError) as exc:
                    _report(f"cleanup unconfirmed: {exc}")
                    result = SUPERVISION_FAILED
            if child is not None:
                try:
                    if result == SUPERVISION_FAILED and child.poll() is None:
                        # A failed membership query cannot authorize broad
                        # signals, but the unreaped direct child is still ours.
                        child.kill()
                    child.wait(timeout=1)
                except (OSError, subprocess.TimeoutExpired) as exc:
                    _report(f"command exit unconfirmed after session cleanup: {exc}")
                    result = SUPERVISION_FAILED
            _reap_children()
    return result


def _run_unix(argv, *, cwd, env, timeout, cleanup_timeout) -> int:
    # Keep the SID leader outside go/test. Setpgid in the service/native child
    # changes its group, but it stays in this helper's pinned session.
    command = [sys.executable, str(Path(__file__).resolve()), "_session",
               "--timeout", str(timeout), "--cleanup-timeout", str(cleanup_timeout),
               "--", *argv]
    with _Interrupts() as interrupted:
        try:
            helper = subprocess.Popen(command, cwd=cwd, env=env, start_new_session=True)
        except OSError as exc:
            _report(f"cannot start session supervisor: {exc}")
            return SUPERVISION_FAILED
        forwarded = False
        while helper.poll() is None:
            if interrupted.signal and not forwarded:
                try:
                    helper.send_signal(interrupted.signal)
                except ProcessLookupError:
                    pass
                forwarded = True
            time.sleep(POLL_INTERVAL)
        if helper.returncode < 0:
            _report("session supervisor was killed; process cleanup is unconfirmed")
            return SUPERVISION_FAILED
        return helper.returncode


class _JobBasicLimits(ctypes.Structure):
    _fields_ = [
        ("process_time", ctypes.c_int64), ("job_time", ctypes.c_int64),
        ("flags", ctypes.c_uint32), ("minimum_set", ctypes.c_size_t),
        ("maximum_set", ctypes.c_size_t), ("active_limit", ctypes.c_uint32),
        ("affinity", ctypes.c_size_t), ("priority", ctypes.c_uint32),
        ("scheduling", ctypes.c_uint32),
    ]


class _JobLimits(ctypes.Structure):
    _fields_ = [
        ("basic", _JobBasicLimits), ("io", ctypes.c_uint64 * 6),
        ("process_memory", ctypes.c_size_t), ("job_memory", ctypes.c_size_t),
        ("peak_process_memory", ctypes.c_size_t), ("peak_job_memory", ctypes.c_size_t),
    ]


class _JobAccounting(ctypes.Structure):
    _fields_ = [
        ("times", ctypes.c_int64 * 4), ("faults", ctypes.c_uint32),
        ("total", ctypes.c_uint32), ("active", ctypes.c_uint32),
        ("terminated", ctypes.c_uint32),
    ]


class _ThreadEntry(ctypes.Structure):
    _fields_ = [
        ("size", ctypes.c_uint32), ("usage", ctypes.c_uint32),
        ("thread_id", ctypes.c_uint32), ("owner_pid", ctypes.c_uint32),
        ("base_priority", ctypes.c_int32), ("delta_priority", ctypes.c_int32),
        ("flags", ctypes.c_uint32),
    ]


class _WindowsJob:
    def __init__(self):
        self.api = ctypes.WinDLL("kernel32", use_last_error=True)
        handle, integer, pointer = ctypes.c_void_p, ctypes.c_uint32, ctypes.c_void_p
        prototypes = {
            "CreateJobObjectW": ([pointer, ctypes.c_wchar_p], handle),
            "SetInformationJobObject": ([handle, integer, pointer, integer], ctypes.c_int),
            "AssignProcessToJobObject": ([handle, handle], ctypes.c_int),
            "QueryInformationJobObject": ([handle, integer, pointer, integer, pointer], ctypes.c_int),
            "TerminateJobObject": ([handle, integer], ctypes.c_int),
            "OpenProcess": ([integer, ctypes.c_int, integer], handle),
            "CreateToolhelp32Snapshot": ([integer, integer], handle),
            "Thread32First": ([handle, pointer], ctypes.c_int),
            "Thread32Next": ([handle, pointer], ctypes.c_int),
            "OpenThread": ([integer, ctypes.c_int, integer], handle),
            "ResumeThread": ([handle], integer),
            "CloseHandle": ([handle], ctypes.c_int),
        }
        for name, (args, result) in prototypes.items():
            function = getattr(self.api, name)
            function.argtypes, function.restype = args, result
        self.handle = self.api.CreateJobObjectW(None, None)
        if not self.handle:
            self._error("CreateJobObjectW")
        limits = _JobLimits()
        limits.basic.flags = 0x2000  # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE; no breakaway.
        if not self.api.SetInformationJobObject(self.handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
            error = ctypes.get_last_error()
            self.close()
            raise OSError(error, "SetInformationJobObject failed")

    @staticmethod
    def _error(operation: str):
        raise OSError(ctypes.get_last_error(), f"{operation} failed")

    def _close(self, handle):
        if not self.api.CloseHandle(handle):
            self._error("CloseHandle")

    def attach(self, pid: int) -> None:
        handle = self.api.OpenProcess(0x0100 | 0x0001 | 0x0400, False, pid)
        if not handle:
            self._error("OpenProcess")
        try:
            if not self.api.AssignProcessToJobObject(self.handle, handle):
                self._error("AssignProcessToJobObject")
        finally:
            self._close(handle)

    def resume(self, pid: int) -> None:
        snapshot = self.api.CreateToolhelp32Snapshot(0x00000004, 0)
        if snapshot == ctypes.c_void_p(-1).value:
            self._error("CreateToolhelp32Snapshot")
        try:
            entry = _ThreadEntry()
            entry.size = ctypes.sizeof(entry)
            success = self.api.Thread32First(snapshot, ctypes.byref(entry))
            resumed = False
            while success:
                if entry.owner_pid == pid:
                    thread = self.api.OpenThread(0x0002, False, entry.thread_id)
                    if not thread:
                        self._error("OpenThread")
                    try:
                        previous = self.api.ResumeThread(thread)
                        if previous == 0xffffffff:
                            self._error("ResumeThread")
                        if previous != 1:
                            raise SupervisionError("child startup thread was not suspended exactly once")
                        resumed = True
                    finally:
                        self._close(thread)
                success = self.api.Thread32Next(snapshot, ctypes.byref(entry))
            if ctypes.get_last_error() != 18:  # ERROR_NO_MORE_FILES
                self._error("enumerate startup thread")
            if not resumed:
                raise SupervisionError("child startup thread not found; refusing to run uncontained")
        finally:
            self._close(snapshot)

    def active(self) -> int:
        info = _JobAccounting()
        if not self.api.QueryInformationJobObject(self.handle, 1, ctypes.byref(info), ctypes.sizeof(info), None):
            self._error("QueryInformationJobObject")
        return info.active

    def terminate(self) -> None:
        if not self.api.TerminateJobObject(self.handle, 1):
            self._error("TerminateJobObject")

    def close(self) -> None:
        if self.handle:
            handle, self.handle = self.handle, None
            self._close(handle)


def _run_windows(argv, *, cwd, env, timeout, cleanup_timeout) -> int:
    child = None
    job = None
    attached = False
    result = SUPERVISION_FAILED
    with _Interrupts() as interrupted:
        try:
            job = _WindowsJob()
            child = subprocess.Popen(argv, cwd=cwd, env=env, creationflags=0x00000004 | 0x00000200)
            job.attach(child.pid)
            attached = True
            job.resume(child.pid)
            result = _wait_command(child, timeout, interrupted)
        except (OSError, SupervisionError) as exc:
            _report(str(exc))
        finally:
            deadline = time.monotonic() + cleanup_timeout
            try:
                if child is not None and not attached:
                    # It was never resumed, so no descendants can have started.
                    child.kill()
                if job is not None:
                    job.terminate()
                    while job.active():
                        if time.monotonic() >= deadline:
                            raise SupervisionError("Windows Job still has active processes")
                        time.sleep(min(POLL_INTERVAL, max(0.0, deadline - time.monotonic())))
                if child is not None:
                    child.wait(timeout=max(0.01, deadline - time.monotonic()))
            except (OSError, SupervisionError, subprocess.TimeoutExpired) as exc:
                _report(f"cleanup unconfirmed: {exc}")
                result = SUPERVISION_FAILED
            finally:
                if job is not None:
                    try:
                        job.close()
                    except OSError as exc:
                        _report(f"cannot close containment Job: {exc}")
                        result = SUPERVISION_FAILED
                if child is not None and child.poll() is None:
                    try:
                        # A failed Job query still closes the kill-on-close
                        # handle; collect the direct child without claiming its
                        # descendants were independently confirmed gone.
                        child.wait(timeout=max(0.01, deadline - time.monotonic()))
                    except (OSError, subprocess.TimeoutExpired) as exc:
                        _report(f"direct child exit unconfirmed: {exc}")
                        result = SUPERVISION_FAILED
    return result


def run_command(argv: list[str], *, cwd=None, env=None,
                timeout: float = 450, cleanup_timeout: float = 35) -> int:
    """Run argv with inherited output and reclaim owned descendants before return."""
    if isinstance(argv, (str, bytes)) or not argv or any(not isinstance(arg, str) for arg in argv):
        raise ValueError("command must be a nonempty list of string arguments")
    for name, value in (("timeout", timeout), ("cleanup_timeout", cleanup_timeout)):
        if not math.isfinite(value) or value <= 0:
            raise ValueError(f"{name} must be finite and positive")
    runner = _run_windows if os.name == "nt" else _run_unix
    return runner(argv, cwd=cwd, env=env, timeout=timeout, cleanup_timeout=cleanup_timeout)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("run", "_session"))
    parser.add_argument("--timeout", type=float, default=450)
    parser.add_argument("--cleanup-timeout", type=float, default=35)
    arguments = list(sys.argv[1:] if argv is None else argv)
    if "--" not in arguments:
        parser.parse_args(arguments)  # Preserve --help and ordinary parser errors.
        parser.error("separate the supervised command with --")
    separator = arguments.index("--")
    args = parser.parse_args(arguments[:separator])
    command = arguments[separator + 1:]
    try:
        if args.mode == "_session":
            if not command or any(not math.isfinite(value) or value <= 0 for value in (
                args.timeout, args.cleanup_timeout,
            )):
                raise ValueError("session command and positive finite deadlines are required")
            return _session_main(command, args.timeout, args.cleanup_timeout)
        return run_command(command, timeout=args.timeout, cleanup_timeout=args.cleanup_timeout)
    except ValueError as exc:
        parser.error(str(exc))


if __name__ == "__main__":
    sys.exit(main())
