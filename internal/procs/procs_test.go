package procs

import (
	"os"
	"os/exec"
	"testing"
)

func TestParentOfSelf(t *testing.T) {
	if got := Parent(os.Getpid()); got != os.Getppid() {
		t.Fatalf("Parent(self) = %d, want %d", got, os.Getppid())
	}
}

func TestAncestorsStartAtParentAndStopAtLimit(t *testing.T) {
	got := Ancestors(os.Getpid(), 12)
	if len(got) == 0 || got[0] != os.Getppid() {
		t.Fatalf("Ancestors = %v, want the parent %d first", got, os.Getppid())
	}
	for _, p := range got {
		if p <= 1 {
			t.Fatalf("Ancestors include init or less: %v", got)
		}
	}
	if one := Ancestors(os.Getpid(), 1); len(one) != 1 {
		t.Fatalf("limit 1: %v", one)
	}
}

// A process started by this test has this process as its nearest ancestor (as a command run from an agent's shell
// has the agent among its ancestors); a pid that does not exist has none.
func TestChildSeesUsAsAncestor(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep")
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	got := Ancestors(cmd.Process.Pid, 12)
	if len(got) < 2 || got[0] != os.Getpid() || got[1] != os.Getppid() {
		t.Fatalf("child's ancestors %v, want [%d %d ...]", got, os.Getpid(), os.Getppid())
	}
	if got := Ancestors(1<<30, 12); len(got) != 0 {
		t.Fatalf("a pid that does not exist has no ancestors: %v", got)
	}
}
