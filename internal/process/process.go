// Package process owns a child process tree and confirms its termination before
// the caller releases a conversation slot or deployment lock. It never matches
// processes by executable name or assumes a dead leader means dead descendants.
package process

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
)

// ErrUnconfirmed requires the caller to retain its deployment/worker lock.
var ErrUnconfirmed = errors.New("process termination unconfirmed")

// Tree owns the platform process group/job established at launch. Terminate and
// Close are safe to call repeatedly. Close releases resources after termination.
type Tree struct {
	mu                 sync.Mutex
	platform           *platformTree
	terminated, closed bool
	result             error
}

// Prepare must be called before cmd.Start. On Windows the child starts suspended
// until Attach assigns the Job Object, so no descendant can escape registration.
func Prepare(cmd *exec.Cmd) { prepareCommand(cmd) }

// Attach must run immediately after cmd.Start and before cmd.Wait can reap the
// process. Linux records the process group's identity in the mounted procfs PID
// namespace; Windows attaches and resumes the suspended child.
func Attach(cmd *exec.Cmd) (*Tree, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("cannot attach a process tree before start")
	}
	platform, err := attachCommand(cmd)
	if err != nil {
		return nil, errors.Join(ErrUnconfirmed, err)
	}
	return &Tree{platform: platform}, nil
}

// Terminate stops all owned descendants and waits for proof that no executing
// group/job member remains. A failure is unconfirmed cleanup, not a free slot.
// exited closes when the direct child's Wait finishes; it only shortens grace.
func (t *Tree) Terminate(exited <-chan struct{}) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.terminated {
		return t.result
	}
	if t.closed {
		return fmt.Errorf("%w: process tree already closed", ErrUnconfirmed)
	}
	t.terminated = true
	t.result = t.platform.terminate(exited)
	if t.result != nil {
		t.result = errors.Join(ErrUnconfirmed, t.result)
	}
	return t.result
}
func (t *Tree) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.platform.close()
		t.closed = true
	}
}
