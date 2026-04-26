package execd

import (
	"bytes"
	"testing"
	"time"
)

func testSpec(cmd string, timeout int) childSpec {
	cfg := DefaultConfig()
	return childSpec{
		Mode:              "shell",
		Cmd:               cmd,
		Privilege:         PrivilegeUser,
		Cwd:               "/tmp",
		Env:               cleanEnv(nil, PrivilegeUser, cfg),
		TimeoutSec:        timeout,
		KillGraceSec:      1,
		MaxStdoutLogBytes: 1024,
		MaxStderrLogBytes: 1024,
		Execution:         cfg.Execution,
	}
}

func TestRunPayloadSuccessAndStderr(t *testing.T) {
	var out, errb bytes.Buffer
	res := runPayload(testSpec("echo ok; echo warn >&2", 2), &out, &errb)
	if res.State != StateSucceeded || res.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if out.String() != "ok\n" {
		t.Fatalf("unexpected stdout: %q", out.String())
	}
	if errb.String() != "warn\n" {
		t.Fatalf("unexpected stderr: %q", errb.String())
	}
}

func TestRunPayloadExitCode(t *testing.T) {
	var out, errb bytes.Buffer
	res := runPayload(testSpec("exit 7", 2), &out, &errb)
	if res.State != StateFailed || res.ExitCode != 7 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestRunPayloadTimeoutKillsProcessGroup(t *testing.T) {
	var out, errb bytes.Buffer
	start := time.Now()
	res := runPayload(testSpec("sleep 20 & wait", 1), &out, &errb)
	if res.State != StateTimedOut || !res.TimedOut {
		t.Fatalf("unexpected result: %+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout path took too long: %s", time.Since(start))
	}
}
