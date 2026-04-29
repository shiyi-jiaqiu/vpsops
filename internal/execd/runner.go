package execd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func newJobID() string {
	now := time.Now().UTC().Format("20060102T150405")
	var b [8]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return now
	}
	return now + "-" + hex.EncodeToString(b[:])
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func runViaChild(ctx context.Context, cfg Config, store *JobStore, jobID string, req RunRequest) RunResult {
	started := time.Now().UTC()
	stdoutFile, err := store.OpenOutput(jobID, "stdout.log")
	if err != nil {
		return internalFailure(jobID, started, err)
	}
	defer stdoutFile.Close()
	stderrFile, err := store.OpenOutput(jobID, "stderr.log")
	if err != nil {
		return internalFailure(jobID, started, err)
	}
	defer stderrFile.Close()

	spec := childSpecFromRequest(req, cfg)
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return internalFailure(jobID, started, err)
	}

	cmd := exec.CommandContext(ctx, cfg.Helpers.SudoPath, sudoHelperArgs(cfg, req.Privilege)...)
	cmd.Stdin = bytes.NewReader(specBytes)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = durationSeconds(req.KillGraceSec + 2)

	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return internalFailure(jobID, started, err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return internalFailure(jobID, started, err)
	}
	resultR, resultW, err := os.Pipe()
	if err != nil {
		return internalFailure(jobID, started, err)
	}
	defer resultR.Close()
	cmd.ExtraFiles = []*os.File{resultW}

	stdoutBuf := newCappedBuffer(req.MaxStdoutBytes)
	stderrBuf := newCappedBuffer(req.MaxStderrBytes)
	if err := cmd.Start(); err != nil {
		resultW.Close()
		return RunResult{
			JobID:      jobID,
			State:      StateStartFailed,
			ExitCode:   -1,
			StartedAt:  started,
			FinishedAt: time.Now().UTC(),
			Error:      err.Error(),
		}
	}
	resultW.Close()

	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(io.MultiWriter(stdoutFile, stdoutBuf), outPipe)
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(io.MultiWriter(stderrFile, stderrBuf), errPipe)
		copyDone <- struct{}{}
	}()

	waitErr := cmd.Wait()
	<-copyDone
	<-copyDone

	resultBytes, readErr := io.ReadAll(io.LimitReader(resultR, 64<<10))
	if readErr != nil {
		return internalFailure(jobID, started, readErr)
	}
	child := decodeChildResult(resultBytes, waitErr)
	finished := time.Now().UTC()
	res := RunResult{
		JobID:              jobID,
		State:              child.State,
		ExitCode:           child.ExitCode,
		Signal:             child.Signal,
		TimedOut:           child.TimedOut,
		Stdout:             stdoutBuf.String(),
		Stderr:             stderrBuf.String(),
		StdoutTruncated:    stdoutBuf.Truncated(),
		StderrTruncated:    stderrBuf.Truncated(),
		StdoutLogTruncated: child.StdoutLogTruncated,
		StderrLogTruncated: child.StderrLogTruncated,
		DurationMS:         finished.Sub(started).Milliseconds(),
		StartedAt:          started,
		FinishedAt:         finished,
		Error:              child.Error,
	}
	return res
}

func childSpecFromRequest(req RunRequest, cfg Config) childSpec {
	return childSpec{
		Mode:              req.Mode,
		Cmd:               req.Cmd,
		Argv:              req.Argv,
		Privilege:         req.Privilege,
		Cwd:               req.Cwd,
		Env:               cleanEnv(req.Env, req.Privilege, cfg),
		Stdin:             req.Stdin,
		TimeoutSec:        req.TimeoutSec,
		KillGraceSec:      req.KillGraceSec,
		MaxStdoutLogBytes: req.MaxStdoutLogBytes,
		MaxStderrLogBytes: req.MaxStderrLogBytes,
		Execution:         cfg.Execution,
	}
}

func sudoHelperArgs(cfg Config, privilege string) []string {
	args := []string{"-n", "-C", "4"}
	if privilege == PrivilegeRoot {
		return append(args, cfg.Helpers.RootChildPath)
	}
	return append(args, "-u", cfg.Execution.RunUser, cfg.Helpers.RunChildPath)
}

func decodeChildResult(resultBytes []byte, waitErr error) childResult {
	trimmed := bytes.TrimSpace(resultBytes)
	if len(trimmed) > 0 {
		var child childResult
		if err := json.Unmarshal(trimmed, &child); err == nil && child.State != "" {
			return child
		} else if err != nil {
			exitCode, signal := protocolFailureExit(waitErr)
			return childResult{State: StateFailed, ExitCode: exitCode, Signal: signal, Error: "invalid child result: " + err.Error()}
		}
		exitCode, signal := protocolFailureExit(waitErr)
		return childResult{State: StateFailed, ExitCode: exitCode, Signal: signal, Error: "invalid child result: missing state"}
	}

	exitCode, signal := protocolFailureExit(waitErr)
	errText := "missing child result"
	if waitErr != nil {
		errText += ": " + waitErr.Error()
	}
	return childResult{State: StateFailed, ExitCode: exitCode, Signal: signal, Error: errText}
}

func protocolFailureExit(waitErr error) (int, *string) {
	exitCode, signal := exitStatus(waitErr)
	if waitErr == nil && exitCode == 0 {
		exitCode = -1
	}
	return exitCode, signal
}

func internalFailure(jobID string, started time.Time, err error) RunResult {
	return RunResult{
		JobID:      jobID,
		State:      StateStartFailed,
		ExitCode:   -1,
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
		Error:      err.Error(),
	}
}

func helperContext(req RunRequest) (context.Context, context.CancelFunc) {
	extra := req.TimeoutSec + req.KillGraceSec + 15
	if extra < 30 {
		extra = 30
	}
	return context.WithTimeout(context.Background(), time.Duration(extra)*time.Second)
}
