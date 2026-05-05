package execd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestLifecycle(t *testing.T, concurrency int) *jobLifecycle {
	t.Helper()
	cfg := DefaultConfig()
	base := t.TempDir()
	cfg.Storage.JobDir = filepath.Join(base, "jobs")
	cfg.Storage.LogDir = filepath.Join(base, "logs")
	cfg.Limits.Concurrency = concurrency
	store, err := NewJobStore(cfg.Storage.JobDir)
	if err != nil {
		t.Fatal(err)
	}
	return newJobLifecycle(store, cfg.Storage, cfg.Limits)
}

func normalizedLifecycleRequest(t *testing.T, cmd string) RunRequest {
	t.Helper()
	req := RunRequest{
		Mode:           "shell",
		Cmd:            cmd,
		Privilege:      PrivilegeUser,
		Cwd:            "/tmp",
		TimeoutSec:     1,
		IdempotencyKey: "same",
		LockKey:        "deploy:app",
	}
	if err := normalizeRequest(&req, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestJobLifecycleWritesIdempotencyMetadata(t *testing.T) {
	lifecycle := newTestLifecycle(t, 1)
	req := normalizedLifecycleRequest(t, "printf ok")
	reqHash := requestHash(req)

	j, reused, err := lifecycle.prepareJob(req, reqHash, AuthInfo{TokenID: "ai-run"}, "203.0.113.10")
	if err != nil {
		t.Fatalf("prepare job: %v", err)
	}
	if reused {
		t.Fatal("first job should not be reused")
	}

	meta, err := lifecycle.store.ReadMetadata(j.id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.IdempotencyKey != "same" || meta.RequestHash != reqHash || meta.LockKey != "deploy:app" {
		t.Fatalf("metadata did not capture lifecycle indexes: %+v", meta)
	}

	reusedJob, reused, err := lifecycle.prepareJob(req, reqHash, AuthInfo{TokenID: "ai-run"}, "203.0.113.10")
	if err != nil {
		t.Fatalf("prepare idempotent job: %v", err)
	}
	if !reused || reusedJob != j {
		t.Fatalf("expected same lifecycle job to be reused, reused=%v", reused)
	}
}

func TestJobLifecycleCleansPartialJobWhenMetadataWriteFails(t *testing.T) {
	lifecycle := newTestLifecycle(t, 1)
	req := normalizedLifecycleRequest(t, "printf ok")
	reqHash := requestHash(req)

	jobsDir := lifecycle.store.dir
	if err := os.Chmod(jobsDir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(jobsDir, 0700)

	_, _, err := lifecycle.prepareJob(req, reqHash, AuthInfo{TokenID: "ai-run"}, "203.0.113.10")
	if err == nil {
		t.Fatal("expected prepare job to fail")
	}
	entries, readErr := os.ReadDir(jobsDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial job directory should be removed, entries=%v", entries)
	}
	if len(lifecycle.sem) != 0 {
		t.Fatalf("concurrency slot should be released, len=%d", len(lifecycle.sem))
	}
}

func TestJobLifecyclePrunesDoneIndexes(t *testing.T) {
	lifecycle := newTestLifecycle(t, 2)
	lifecycle.limits.MaxJobsRetained = 1
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	lifecycle.now = func() time.Time { return now }

	oldReq := normalizedLifecycleRequest(t, "printf old")
	oldReq.IdempotencyKey = "old"
	oldHash := requestHash(oldReq)
	oldJob, _, err := lifecycle.prepareJob(oldReq, oldHash, AuthInfo{TokenID: "ai-run"}, "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	oldJob.started = now.Add(-time.Hour)
	oldJob.setResult(RunResult{JobID: oldJob.id, State: StateSucceeded, ExitCode: 0, StartedAt: oldJob.started, FinishedAt: oldJob.started})
	if err := lifecycle.releaseJob(oldJob); err != nil {
		t.Fatal(err)
	}

	newReq := normalizedLifecycleRequest(t, "printf new")
	newReq.IdempotencyKey = "new"
	newHash := requestHash(newReq)
	newJob, _, err := lifecycle.prepareJob(newReq, newHash, AuthInfo{TokenID: "ai-run"}, "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	newJob.setResult(RunResult{JobID: newJob.id, State: StateSucceeded, ExitCode: 0, StartedAt: now, FinishedAt: now})
	if err := lifecycle.releaseJob(newJob); err != nil {
		t.Fatal(err)
	}

	lifecycle.pruneMemory(nil)
	if lifecycle.getJob(oldJob.id) != nil {
		t.Fatal("old done job should be pruned from memory")
	}
	if lifecycle.idempotency[idempotencyIndexKey("ai-run", "old")] != nil {
		t.Fatal("old idempotency index should be pruned")
	}
	if lifecycle.getJob(newJob.id) == nil {
		t.Fatal("new done job should be retained")
	}
}

func TestJobLifecycleDetectsDoubleRelease(t *testing.T) {
	lifecycle := newTestLifecycle(t, 1)
	req := normalizedLifecycleRequest(t, "printf ok")
	j, _, err := lifecycle.prepareJob(req, requestHash(req), AuthInfo{TokenID: "ai-run"}, "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.releaseJob(j); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.releaseJob(j); !errors.Is(err, errConcurrencySlotPanic) {
		t.Fatalf("expected double release guard, got %v", err)
	}
}

func TestJobCancelBeforeRunnerStartsCancelsWhenRunnerBindsContext(t *testing.T) {
	l := newTestLifecycle(t, 1)
	req := normalizedLifecycleRequest(t, "sleep 60")
	j, _, err := l.prepareJob(req, requestHash(req), AuthInfo{TokenID: "ai-run"}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.cancelJob(j.id); err != nil {
		t.Fatal(err)
	}
	canceled := false
	j.setCancel(func() { canceled = true })
	l.markRunning(j)

	if !canceled {
		t.Fatal("expected cancel func to run when runner binds after cancel request")
	}
	if got := j.summary().State; got != StateCanceled {
		t.Fatalf("expected canceled state to survive markRunning, got %q", got)
	}
}
