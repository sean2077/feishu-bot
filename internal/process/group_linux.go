//go:build linux

package process

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

type groupIdentity struct {
	proc          fs.FS
	group, leader int
	start         uint64
}
type procStatus struct {
	pid, parent  int
	pids, groups []int
}
type procStat struct {
	pid, parent, group, threads int
	state                       byte
	start                       uint64
}

func numberList(value string) ([]int, error) {
	fields := strings.Fields(value)
	result := make([]int, len(fields))
	for i, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid procfs namespace id")
		}
		result[i] = n
	}
	return result, nil
}
func parseStatus(data []byte) (procStatus, error) {
	var result procStatus
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "Pid", "PPid", "NSpid", "NSpgid":
			numbers, err := numberList(value)
			if err != nil || len(numbers) == 0 {
				return result, fmt.Errorf("invalid procfs process identity")
			}
			switch key {
			case "Pid":
				result.pid = numbers[0]
			case "PPid":
				result.parent = numbers[0]
			case "NSpid":
				result.pids = numbers
			case "NSpgid":
				result.groups = numbers
			}
		}
	}
	if result.pid <= 0 {
		return result, fmt.Errorf("procfs process identity missing")
	}
	return result, nil
}
func parseStat(data []byte) (procStat, error) {
	var result procStat
	// comm may contain spaces, newlines, and closing parentheses. The delimiter
	// before state is the final ')' in this kernel-defined record.
	open, close := strings.IndexByte(string(data), '('), strings.LastIndexByte(string(data), ')')
	if open < 1 || close < open {
		return result, fmt.Errorf("invalid procfs stat record")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data[:open])))
	if err != nil {
		return result, err
	}
	fields := strings.Fields(string(data[close+1:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return result, fmt.Errorf("incomplete procfs stat record")
	}
	result.pid = pid
	result.state = fields[0][0]
	result.parent, err = strconv.Atoi(fields[1])
	if err != nil {
		return result, err
	}
	result.group, err = strconv.Atoi(fields[2])
	if err != nil {
		return result, err
	}
	result.threads, err = strconv.Atoi(fields[17])
	if err != nil {
		return result, err
	}
	result.start, err = strconv.ParseUint(fields[19], 10, 64)
	return result, err
}
func captureGroup(pid int) (*groupIdentity, error) {
	return captureGroupFS(os.DirFS("/proc"), os.Getpid(), pid)
}
func captureGroupFS(proc fs.FS, selfPID, childPID int) (*groupIdentity, error) {
	raw, err := fs.ReadFile(proc, "self/status")
	if err != nil {
		return nil, err
	}
	self, err := parseStatus(raw)
	if err != nil {
		return nil, err
	}
	index := -1
	for i, pid := range self.pids {
		if pid == selfPID {
			index = i
		}
	}
	if index < 0 {
		// Old kernels may omit namespace fields. Only the directly matching procfs
		// namespace can then be established; never guess a host/namespace PID map.
		if len(self.pids) == 0 && self.pid == selfPID {
			index = 0
		} else {
			return nil, fmt.Errorf("cannot identify current procfs PID namespace")
		}
	}
	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil || !entry.IsDir() {
			continue
		}
		raw, err = fs.ReadFile(proc, entry.Name()+"/status")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				continue
			}
			return nil, err
		}
		status, err := parseStatus(raw)
		if err != nil {
			return nil, err
		}
		visiblePID := 0
		if index < len(status.pids) {
			visiblePID = status.pids[index]
		} else if index == 0 && len(status.pids) == 0 {
			visiblePID = status.pid
		}
		// Namespace PID numbers repeat in sibling namespaces. The actual parent PID
		// in the mounted procfs namespace binds this match to OUR direct child.
		if visiblePID != childPID || status.parent != self.pid {
			continue
		}
		raw, err = fs.ReadFile(proc, entry.Name()+"/stat")
		if err != nil {
			return nil, err
		}
		stat, err := parseStat(raw)
		if err != nil {
			return nil, err
		}
		group := stat.group
		if len(status.groups) > 0 {
			if index >= len(status.groups) || status.groups[index] != childPID {
				return nil, fmt.Errorf("child left its owned process group")
			}
			group = status.groups[0]
		}
		if stat.pid != status.pid || stat.group != group || group != status.pid {
			return nil, fmt.Errorf("child process group identity changed")
		}
		return &groupIdentity{proc: proc, group: group, leader: status.pid, start: stat.start}, nil
	}
	return nil, fmt.Errorf("child process identity unavailable before reap")
}
func (g *groupIdentity) active() (bool, error) {
	// Once the original leader's procfs PID is reused, the original process group
	// is gone: Linux reserves a PGID until its last member has left that group.
	leaderPath := strconv.Itoa(g.leader) + "/stat"
	if raw, err := fs.ReadFile(g.proc, leaderPath); err == nil {
		stat, err := parseStat(raw)
		if err != nil {
			return false, err
		}
		if stat.start != g.start {
			return false, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	entries, err := fs.ReadDir(g.proc, ".")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil || !entry.IsDir() {
			continue
		}
		raw, err := fs.ReadFile(g.proc, entry.Name()+"/stat")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return false, err
		}
		stat, err := parseStat(raw)
		if err != nil {
			return false, err
		}
		if stat.group != g.group {
			continue
		}
		if stat.state != 'Z' && stat.state != 'X' && stat.state != 'x' {
			return true, nil
		}
		// A thread-group leader can become a zombie before its other threads exit.
		if stat.threads > 1 {
			tasks, err := fs.ReadDir(g.proc, entry.Name()+"/task")
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return false, err
			}
			for _, task := range tasks {
				raw, err := fs.ReadFile(g.proc, entry.Name()+"/task/"+task.Name()+"/stat")
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					return false, err
				}
				thread, err := parseStat(raw)
				if err != nil {
					return false, err
				}
				if thread.state != 'Z' && thread.state != 'X' && thread.state != 'x' {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
