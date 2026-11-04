package native

import (
	"os/exec"

	"github.com/sean2077/feishu-bot/internal/process"
)

type processTree struct{ guard *process.Tree }

func prepareProcess(cmd *exec.Cmd) { process.Prepare(cmd) }
func attachProcess(cmd *exec.Cmd) (*processTree, error) {
	guard, err := process.Attach(cmd)
	if err != nil {
		return nil, err
	}
	return &processTree{guard: guard}, nil
}
func (t *processTree) terminate(exited <-chan struct{}) error { return t.guard.Terminate(exited) }
func (t *processTree) close()                                 { t.guard.Close() }
