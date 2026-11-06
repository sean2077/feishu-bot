package process

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestInteractiveRequiresTerminalBeforeLaunching(t *testing.T){
	cmd:=exec.Command("nonexistent-local-session-executable")
	cmd.Stdin=strings.NewReader("input")
	if err:=RunInteractive(context.Background(),cmd);err==nil||!strings.Contains(err.Error(),"terminal"){t.Fatal("nonterminal launch was not rejected before start",err)}
	if cmd.Process!=nil{t.Fatal("interactive child launched without terminal")}
}
