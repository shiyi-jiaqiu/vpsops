package execd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func ChildMain(privilege string) int {
	var spec childSpec
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&spec); err != nil {
		writeChildResult(childResult{State: StateStartFailed, ExitCode: -1, Error: "decode spec: " + err.Error()})
		return 125
	}
	if spec.Privilege != privilege {
		writeChildResult(childResult{State: StateStartFailed, ExitCode: -1, Error: "privilege mismatch"})
		return 125
	}
	res := runPayload(spec, os.Stdout, os.Stderr)
	writeChildResult(res)
	if res.State == StateSucceeded {
		return 0
	}
	if res.ExitCode >= 0 && res.ExitCode <= 125 {
		return res.ExitCode
	}
	return 125
}

func writeChildResult(res childResult) {
	f := os.NewFile(uintptr(3), "result")
	if f == nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(res)
}

func runPayload(spec childSpec, stdout, stderr io.Writer) childResult {
	if spec.TimeoutSec < 1 {
		spec.TimeoutSec = 30
	}
	if spec.KillGraceSec < 1 {
		spec.KillGraceSec = 3
	}

	ctx, cancel := context.WithTimeout(context.Background(), durationSeconds(spec.TimeoutSec))
	defer cancel()

	var cmd *exec.Cmd
	if spec.Mode == "argv" {
		if len(spec.Argv) == 0 || spec.Argv[0] == "" {
			return childResult{State: StateStartFailed, ExitCode: -1, Error: "argv[0] is required"}
		}
		cmd = exec.Command(spec.Argv[0], spec.Argv[1:]...)
	} else {
		args := append([]string{}, spec.Execution.ShellArgs...)
		args = append(args, spec.Cmd)
		cmd = exec.Command(spec.Execution.ShellPath, args...)
	}
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.Stdin = stringsReader(spec.Stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = durationSeconds(spec.KillGraceSec + 1)

	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return childResult{State: StateStartFailed, ExitCode: -1, Error: err.Error()}
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return childResult{State: StateStartFailed, ExitCode: -1, Error: err.Error()}
	}
	outWriter := newCappedTeeWriter(stdout, spec.MaxStdoutLogBytes)
	errWriter := newCappedTeeWriter(stderr, spec.MaxStderrLogBytes)

	if err := cmd.Start(); err != nil {
		return childResult{State: StateStartFailed, ExitCode: -1, Error: err.Error()}
	}

	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(outWriter, outPipe)
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(errWriter, errPipe)
		copyDone <- struct{}{}
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		timedOut = true
		killProcessGroup(cmd, syscall.SIGTERM)
		select {
		case waitErr = <-waitDone:
		case <-time.After(durationSeconds(spec.KillGraceSec)):
			killProcessGroup(cmd, syscall.SIGKILL)
			waitErr = <-waitDone
		}
	}
	<-copyDone
	<-copyDone

	exitCode, signal := exitStatus(waitErr)
	state := StateSucceeded
	if timedOut {
		state = StateTimedOut
	} else if waitErr != nil || exitCode != 0 {
		state = StateFailed
	}
	errText := ""
	if waitErr != nil && !errors.Is(waitErr, context.DeadlineExceeded) {
		errText = waitErr.Error()
	}
	return childResult{
		State:              state,
		ExitCode:           exitCode,
		Signal:             signal,
		TimedOut:           timedOut,
		StdoutLogTruncated: outWriter.Truncated(),
		StderrLogTruncated: errWriter.Truncated(),
		Error:              errText,
	}
}

func stringsReader(s string) io.Reader {
	if s == "" {
		return nil
	}
	return strings.NewReader(s)
}

func killProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

func exitStatus(err error) (int, *string) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				s := status.Signal().String()
				return -1, &s
			}
			return status.ExitStatus(), nil
		}
	}
	return -1, nil
}
