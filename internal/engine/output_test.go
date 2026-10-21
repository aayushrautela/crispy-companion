package engine

import (
	"os"
	"os/exec"
	"testing"
)

// The engine's output is the only clue to "the torrent has no peers" versus
// "the tracker is blocked", so it has to reach a foreground terminal. Forced
// on, it always inherits.
func TestInheritEngineOutputForced(t *testing.T) {
	t.Setenv("CRISPY_COMPANION_ENGINE_LOG", "1")
	var cmd exec.Cmd
	inheritEngineOutput(&cmd)
	if cmd.Stdout != os.Stdout || cmd.Stderr != os.Stderr {
		t.Fatalf("forced engine output not inherited: stdout=%v stderr=%v", cmd.Stdout, cmd.Stderr)
	}
}

// Not forced and not on a terminal (a desktop launch, or a pipe), the engine
// keeps quiet: a child writing to a closed pipe dies from SIGPIPE.
func TestInheritEngineOutputQuietWhenNotATerminal(t *testing.T) {
	if isTerminal(os.Stdout) || isTerminal(os.Stderr) {
		t.Skip("stdout/stderr is a terminal in this run")
	}
	t.Setenv("CRISPY_COMPANION_ENGINE_LOG", "")
	var cmd exec.Cmd
	inheritEngineOutput(&cmd)
	if cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatalf("engine output should stay discarded: stdout=%v stderr=%v", cmd.Stdout, cmd.Stderr)
	}
}
