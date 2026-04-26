package execd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *JobStore {
	t.Helper()
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func createStoredJob(t *testing.T, store *JobStore, jobID string, created time.Time, stdout string) {
	t.Helper()
	req := RunRequest{Mode: "shell", Cmd: "echo ok", Privilege: PrivilegeUser, Cwd: "/tmp"}
	if err := store.Create(jobID, req); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMetadata(JobMetadata{
		JobID:     jobID,
		TokenID:   "ai-run",
		Privilege: PrivilegeUser,
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	f, err := store.OpenOutput(jobID, "stdout.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(stdout); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadOutputTail(t *testing.T) {
	store := newTestStore(t)
	createStoredJob(t, store, "20260426T000001-tail", time.Now(), "abcdef")

	full, err := store.ReadOutputTail("20260426T000001-tail", "stdout.log", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(full) != "abcdef" {
		t.Fatalf("unexpected full output: %q", string(full))
	}
	tail, err := store.ReadOutputTail("20260426T000001-tail", "stdout.log", 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail) != "def" {
		t.Fatalf("unexpected tail output: %q", string(tail))
	}
}

func TestCleanupMaxJobsRetained(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	createStoredJob(t, store, "20260426T000001-old", now.Add(-3*time.Hour), "old")
	createStoredJob(t, store, "20260426T000002-mid", now.Add(-2*time.Hour), "mid")
	createStoredJob(t, store, "20260426T000003-new", now.Add(-time.Hour), "new")

	report, err := store.Cleanup(StorageConfig{MaxTotalJobBytes: 1 << 20}, LimitsConfig{MaxJobsRetained: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.DeletedIDs) != 1 || report.DeletedIDs[0] != "20260426T000001-old" {
		t.Fatalf("unexpected cleanup report: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(store.dir, "20260426T000001-old")); !os.IsNotExist(err) {
		t.Fatalf("old job should be deleted, stat err=%v", err)
	}
}

func TestCleanupRetentionSkipsProtected(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	createStoredJob(t, store, "20260426T000001-old", now.Add(-48*time.Hour), "old")
	createStoredJob(t, store, "20260426T000002-new", now, "new")

	report, err := store.Cleanup(
		StorageConfig{RetentionDays: 1, MaxTotalJobBytes: 1 << 20},
		LimitsConfig{MaxJobsRetained: 10},
		map[string]bool{"20260426T000001-old": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.DeletedIDs) != 0 {
		t.Fatalf("protected job should not be deleted: %+v", report)
	}
}
