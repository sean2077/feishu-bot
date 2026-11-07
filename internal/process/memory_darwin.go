package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func residentRows(ctx context.Context) ([]residentProcess, int, error) {
	// ps exposes resident size without CGO. Exclude the collector itself from
	// its parent's tree so observing memory does not consume the admission cap.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,rss=")
	cmd.WaitDelay = time.Second
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	collector := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		return nil, 0, err
	}
	var rows []residentProcess
	for _, line := range strings.Split(output.String(), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return nil, 0, errors.New("invalid process RSS observation")
		}
		pid, ep := strconv.Atoi(f[0])
		parent, epp := strconv.Atoi(f[1])
		rss, er := strconv.ParseInt(f[2], 10, 64)
		if ep != nil || epp != nil || er != nil || rss < 0 || rss > (1<<63-1)/1024 {
			return nil, 0, errors.New("invalid process RSS observation")
		}
		if pid != collector {
			rows = append(rows, residentProcess{pid, parent, rss * 1024})
		}
	}
	return rows, os.Getpid(), nil
}
