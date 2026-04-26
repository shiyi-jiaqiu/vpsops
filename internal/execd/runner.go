package execd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type job struct {
	id      string
	state   string
	result  *RunResult
	done    chan struct{}
	started time.Time
	hash    string
	tokenID string
	remote  string
	lockKey string
	mu      sync.Mutex
}

func (j *job) setResult(res RunResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.result = &res
	j.state = res.State
	close(j.done)
}

func (j *job) summary() JobSummary {
	j.mu.Lock()
	defer j.mu.Unlock()
	var finished *time.Time
	if j.result != nil {
		t := j.result.FinishedAt
		finished = &t
	}
	return JobSummary{
		JobID:      j.id,
		State:      j.state,
		StartedAt:  j.started,
		FinishedAt: finished,
		Result:     j.result,
	}
}

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

	spec := childSpec{
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
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return internalFailure(jobID, started, err)
	}

	var cmd *exec.Cmd
	if req.Privilege == PrivilegeRoot {
		cmd = exec.CommandContext(ctx, cfg.Helpers.SudoPath, "-n", "-C", "4", cfg.Helpers.RootChildPath)
	} else {
		cmd = exec.CommandContext(ctx, cfg.Helpers.SudoPath, "-n", "-C", "4", "-u", cfg.Execution.RunUser, cfg.Helpers.RunChildPath)
	}
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

	var child childResult
	resultBytes, readErr := io.ReadAll(io.LimitReader(resultR, 64<<10))
	if readErr == nil && len(bytes.TrimSpace(resultBytes)) > 0 {
		readErr = json.Unmarshal(resultBytes, &child)
	}
	if readErr != nil || child.State == "" {
		child.State = StateFailed
		child.ExitCode, child.Signal = exitStatus(waitErr)
		if waitErr != nil {
			child.Error = waitErr.Error()
		}
	}
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

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
