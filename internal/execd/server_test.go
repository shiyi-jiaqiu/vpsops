package execd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, concurrency int) *Server {
	t.Helper()
	cfg := DefaultConfig()
	base := t.TempDir()
	cfg.Storage.JobDir = filepath.Join(base, "jobs")
	cfg.Storage.LogDir = filepath.Join(base, "logs")
	cfg.Limits.Concurrency = concurrency
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPrepareJobLockKeyConflict(t *testing.T) {
	s := newTestServer(t, 2)
	req := RunRequest{
		Mode:      "shell",
		Cmd:       "echo ok",
		Privilege: PrivilegeUser,
		Cwd:       "/tmp",
		LockKey:   "deploy:app",
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	j, reused, err := s.prepareJob(req, hashBytes(body), AuthInfo{TokenID: "ai-run"}, "127.0.0.1")
	if err != nil {
		t.Fatalf("prepare first job: %v", err)
	}
	if reused {
		t.Fatal("first job should not be reused")
	}
	if j.lockKey != req.LockKey {
		t.Fatalf("expected lock key %q, got %q", req.LockKey, j.lockKey)
	}

	_, _, err = s.prepareJob(req, hashBytes(append(body, 'x')), AuthInfo{TokenID: "ai-run"}, "127.0.0.1")
	if !errors.Is(err, errLockBusy) {
		t.Fatalf("expected lock busy, got %v", err)
	}
}

func TestReleaseJobClearsLockKey(t *testing.T) {
	s := newTestServer(t, 2)
	req := RunRequest{
		Mode:      "shell",
		Cmd:       "echo ok",
		Privilege: PrivilegeUser,
		Cwd:       "/tmp",
		LockKey:   "deploy:app",
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := s.prepareJob(req, hashBytes(body), AuthInfo{TokenID: "ai-run"}, "127.0.0.1")
	if err != nil {
		t.Fatalf("prepare first job: %v", err)
	}
	j.setResult(RunResult{
		JobID:      j.id,
		State:      StateSucceeded,
		ExitCode:   0,
		StartedAt:  time.Now().UTC(),
		FinishedAt: time.Now().UTC(),
	})
	s.releaseJob(j)

	nextBody, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.prepareJob(req, hashBytes(nextBody), AuthInfo{TokenID: "ai-run"}, "127.0.0.1"); err != nil {
		t.Fatalf("expected lock to be released: %v", err)
	}
}

func TestRequestHashCanonicalizesLegacyRoot(t *testing.T) {
	cfg := DefaultConfig()
	root := true
	legacy := RunRequest{Mode: "shell", Cmd: "id -u", Root: &root}
	modern := RunRequest{Mode: "shell", Cmd: "id -u", Privilege: PrivilegeRoot}
	if err := normalizeRequest(&legacy, cfg); err != nil {
		t.Fatal(err)
	}
	if err := normalizeRequest(&modern, cfg); err != nil {
		t.Fatal(err)
	}
	if requestHash(legacy) != requestHash(modern) {
		t.Fatal("expected legacy root and privilege root requests to hash identically")
	}
}

func TestParseTailBytes(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/jobs/job/stdout?tail_bytes=42", nil)
	n, err := parseTailBytes(r, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Fatalf("expected 42, got %d", n)
	}
	r = httptest.NewRequest("GET", "/v1/jobs/job/stdout?tail_bytes=101", nil)
	if _, err := parseTailBytes(r, 100); err == nil {
		t.Fatal("expected oversized tail_bytes to fail")
	}
}

func TestAuthenticateRequestRateLimitsFailures(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tokens = []TokenConfig{{ID: "ai-run", SHA256: SHA256Hex("good-token")}}
	cfg.Security.AuthFailureLimit = 1
	cfg.Security.AuthFailureWindowSec = 60
	base := t.TempDir()
	cfg.Storage.JobDir = filepath.Join(base, "jobs")
	cfg.Storage.LogDir = filepath.Join(base, "logs")
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/job", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("Authorization", "Bearer bad-token")
	w := httptest.NewRecorder()
	if _, ok := s.authenticateRequest(w, req); ok || w.Code != http.StatusUnauthorized {
		t.Fatalf("first bad token should be unauthorized, ok=%v code=%d", ok, w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/jobs/job", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("Authorization", "Bearer bad-token")
	w = httptest.NewRecorder()
	if _, ok := s.authenticateRequest(w, req); ok || w.Code != http.StatusTooManyRequests {
		t.Fatalf("second bad token should be rate limited, ok=%v code=%d", ok, w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/jobs/job", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("Authorization", "Bearer good-token")
	w = httptest.NewRecorder()
	if _, ok := s.authenticateRequest(w, req); !ok {
		t.Fatalf("valid token should clear failures, code=%d", w.Code)
	}
}

func TestHandleRunReturns500ForPrepareJobInternalError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tokens = []TokenConfig{{ID: "ai-run", SHA256: SHA256Hex("good-token")}}
	base := t.TempDir()
	cfg.Storage.JobDir = filepath.Join(base, "jobs")
	cfg.Storage.LogDir = filepath.Join(base, "logs")
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.store = &JobStore{dir: filepath.Join(base, "missing-parent", "jobs")}

	body := strings.NewReader(`{"mode":"shell","cmd":"echo ok","privilege":"user","cwd":"/tmp","timeout_sec":1}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/run", body)
	req.Header.Set("Authorization", "Bearer good-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleRun(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for store/internal error, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "missing-parent") {
		t.Fatalf("internal path leaked in response: %s", w.Body.String())
	}
}

func TestHandleRunReturns409ForIdempotencyConflict(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tokens = []TokenConfig{{ID: "ai-run", SHA256: SHA256Hex("good-token")}}
	base := t.TempDir()
	cfg.Storage.JobDir = filepath.Join(base, "jobs")
	cfg.Storage.LogDir = filepath.Join(base, "logs")
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	existingReq := RunRequest{
		Mode:           "shell",
		Cmd:            "echo one",
		Privilege:      PrivilegeUser,
		Cwd:            "/tmp",
		TimeoutSec:     5,
		IdempotencyKey: "same",
	}
	if err := normalizeRequest(&existingReq, cfg); err != nil {
		t.Fatal(err)
	}
	s.idempotency["ai-run:same"] = &job{
		id:   "20260426T000000-existing",
		hash: requestHash(existingReq),
		done: make(chan struct{}),
	}

	body := strings.NewReader(`{"mode":"shell","cmd":"echo two","privilege":"user","cwd":"/tmp","timeout_sec":5,"idempotency_key":"same"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/run", body)
	req.Header.Set("Authorization", "Bearer good-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleRun(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for idempotency conflict, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthorizeJobAccessUsesMetadata(t *testing.T) {
	s := newTestServer(t, 1)
	req := RunRequest{
		Mode:      "shell",
		Cmd:       "echo ok",
		Privilege: PrivilegeRoot,
		Cwd:       "/",
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := s.prepareJob(req, hashBytes(body), AuthInfo{TokenID: "ai-root", AllowRoot: true}, "203.0.113.10")
	if err != nil {
		t.Fatalf("prepare job: %v", err)
	}

	allowed, err := s.authorizeJobAccess(AuthInfo{TokenID: "ai-root", AllowRoot: true}, j.id)
	if err != nil || !allowed {
		t.Fatalf("expected root token access, allowed=%v err=%v", allowed, err)
	}
	allowed, err = s.authorizeJobAccess(AuthInfo{TokenID: "ai-run"}, j.id)
	if err != nil || allowed {
		t.Fatalf("expected other run token denied, allowed=%v err=%v", allowed, err)
	}

	s.mu.Lock()
	delete(s.jobs, j.id)
	s.mu.Unlock()
	allowed, err = s.authorizeJobAccess(AuthInfo{TokenID: "ai-root", AllowRoot: true}, j.id)
	if err != nil || !allowed {
		t.Fatalf("expected stored metadata root access, allowed=%v err=%v", allowed, err)
	}
	allowed, err = s.authorizeJobAccess(AuthInfo{TokenID: "ai-run"}, j.id)
	if err != nil || allowed {
		t.Fatalf("expected stored metadata denial, allowed=%v err=%v", allowed, err)
	}
}

func TestWriteAuditJSONL(t *testing.T) {
	s := newTestServer(t, 1)
	req := RunRequest{
		Mode:      "shell",
		Cmd:       "echo ok",
		Privilege: PrivilegeRoot,
		Cwd:       "/",
	}
	now := time.Now().UTC()
	j := &job{
		id:      "20260426T000000-abcdef12",
		tokenID: "ai-root",
		remote:  "203.0.113.10",
	}
	res := RunResult{
		JobID:      j.id,
		State:      StateSucceeded,
		ExitCode:   0,
		DurationMS: 12,
		StartedAt:  now,
		FinishedAt: now,
	}

	if err := s.writeAudit(j, req, res); err != nil {
		t.Fatalf("write audit: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.Storage.LogDir, "audit.log"))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(b, &entry); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if entry["job_id"] != j.id || entry["token_id"] != "ai-root" || entry["remote_addr"] != "203.0.113.10" {
		t.Fatalf("unexpected audit entry: %#v", entry)
	}
	if entry["cmd_preview"] != "echo ok" {
		t.Fatalf("unexpected cmd preview: %#v", entry["cmd_preview"])
	}
}

func TestClientAddressTrustsForwardedHeadersOnlyFromLoopbackPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/run", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.20, 127.0.0.1")
	if got := clientAddress(req); got != "203.0.113.20" {
		t.Fatalf("expected forwarded client from loopback proxy, got %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/run", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.20")
	if got := clientAddress(req); got != "198.51.100.10" {
		t.Fatalf("expected direct peer when non-loopback sends forwarded header, got %q", got)
	}
}
