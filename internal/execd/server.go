package execd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	cfg         Config
	store       *JobStore
	http        *http.Server
	lifecycle   *jobLifecycle
	auditMu     sync.Mutex
	authLimiter *authFailureLimiter
}

func NewServer(cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.Storage.LogDir, 0700); err != nil {
		return nil, err
	}
	store, err := NewJobStore(cfg.Storage.JobDir)
	if err != nil {
		return nil, err
	}
	if report, err := store.Cleanup(cfg.Storage, cfg.Limits, nil); err != nil {
		logEvent("job_cleanup_failed", map[string]any{"phase": "initial", "error": err.Error()})
	} else if len(report.DeletedIDs) > 0 {
		logEvent("job_cleanup_completed", map[string]any{
			"phase":         "initial",
			"deleted_jobs":  len(report.DeletedIDs),
			"deleted_bytes": report.DeletedBytes,
		})
	}
	s := &Server{
		cfg:         cfg,
		store:       store,
		lifecycle:   newJobLifecycle(store, cfg.Storage, cfg.Limits),
		authLimiter: newAuthFailureLimiter(cfg.Security),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/run", s.handleRun)
	mux.HandleFunc("GET /v1/jobs/{job_id}", s.handleJob)
	mux.HandleFunc("GET /v1/jobs/{job_id}/stdout", s.handleOutput("stdout.log"))
	mux.HandleFunc("GET /v1/jobs/{job_id}/stderr", s.handleOutput("stderr.log"))
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      time.Duration(max(60, cfg.Limits.MaxWaitSec+5)) * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	return s, nil
}

func (s *Server) ListenAndServe() error {
	return s.http.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	running := s.lifecycle.runningCount()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"concurrency":     s.cfg.Limits.Concurrency,
		"running_jobs":    running,
		"available_slots": max(0, s.cfg.Limits.Concurrency-running),
	})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.authenticateRequest(w, r)
	if !ok {
		return
	}
	body, err := readLimitedBody(w, r, s.cfg.Limits.MaxRequestBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "request body too large")
		return
	}
	req, err := decodeRunRequest(body)
	if err != nil {
		if requestDecodeErrorCode(err) == "invalid_json" {
			writeError(w, http.StatusBadRequest, "invalid_json", "invalid json")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		}
		return
	}
	if err := normalizeRequest(&req, s.cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Privilege == PrivilegeRoot && !auth.AllowRoot {
		writeError(w, http.StatusForbidden, "forbidden_root", "root privilege requires root-capable token")
		return
	}

	reqHash := requestHash(req)
	j, reused, err := s.lifecycle.prepareJob(req, reqHash, auth, clientAddress(r))
	if err != nil {
		if errors.Is(err, errBusy) {
			logEvent("run_rejected", runRejectFields(auth, r, "executor_busy"))
			writeErrorRetry(w, http.StatusConflict, "executor_busy", "executor is busy", 1)
			return
		}
		if errors.Is(err, errLockBusy) {
			fields := runRejectFields(auth, r, "lock_busy")
			fields["lock_key"] = req.LockKey
			logEvent("run_rejected", fields)
			writeError(w, http.StatusConflict, "lock_busy", "lock_key is busy")
			return
		}
		if errors.Is(err, errIdempotencyConflict) {
			fields := runRejectFields(auth, r, "idempotency_conflict")
			fields["idempotency_key"] = req.IdempotencyKey
			logEvent("run_rejected", fields)
			writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key reused with different request")
			return
		}
		logEvent("run_rejected", runRejectFields(auth, r, "prepare_failed", "error", err.Error()))
		writeError(w, http.StatusInternalServerError, "prepare_failed", "prepare job failed")
		return
	}
	if !reused {
		logEvent("job_queued", jobEventFields(j, req))
		go s.runJob(j, req)
	} else {
		logEvent("job_reused", jobEventFields(j, req))
	}

	select {
	case <-j.done:
		writeJSON(w, http.StatusOK, j.summary().Result)
	case <-time.After(durationSeconds(req.WaitSec)):
		writeJSON(w, http.StatusAccepted, j.summary())
	}
}

func (s *Server) runJob(j *job, req RunRequest) {
	defer func() {
		if err := s.lifecycle.releaseJob(j); err != nil {
			logEvent("job_release_failed", map[string]any{"job_id": j.id, "error": err.Error()})
		}
		s.cleanupJobs()
	}()
	s.lifecycle.markRunning(j)
	logEvent("job_started", jobEventFields(j, req))
	ctx, cancel := helperContext(req)
	defer cancel()
	res := runViaChild(ctx, s.cfg, s.store, j.id, req)
	if err := s.store.SaveResult(res); err != nil {
		logEvent("job_result_save_failed", map[string]any{"job_id": j.id, "error": err.Error()})
	}
	if err := s.writeAudit(j, req, res); err != nil {
		logEvent("audit_write_failed", map[string]any{"job_id": j.id, "error": err.Error()})
	}
	j.setResult(res)
	fields := jobEventFields(j, req)
	fields["state"] = res.State
	fields["exit_code"] = res.ExitCode
	fields["timed_out"] = res.TimedOut
	fields["duration_ms"] = res.DurationMS
	logEvent("job_finished", fields)
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.authenticateRequest(w, r)
	if !ok {
		return
	}
	jobID := r.PathValue("job_id")
	if !jobIDRe.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid_job_id", "invalid job_id")
		return
	}
	allowed, err := s.authorizeJobAccess(auth, jobID)
	if err != nil {
		writeError(w, http.StatusNotFound, "job_not_found", "job not found")
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "forbidden", "forbidden")
		return
	}
	j := s.lifecycle.getJob(jobID)
	if j != nil {
		writeJSON(w, http.StatusOK, j.summary())
		return
	}
	res, err := s.store.ReadResult(jobID)
	if err != nil {
		writeError(w, http.StatusNotFound, "job_not_found", "job not found")
		return
	}
	writeJSON(w, http.StatusOK, JobSummary{JobID: jobID, State: res.State, StartedAt: res.StartedAt, FinishedAt: &res.FinishedAt, Result: &res})
}

func (s *Server) handleOutput(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := s.authenticateRequest(w, r)
		if !ok {
			return
		}
		jobID := r.PathValue("job_id")
		if !jobIDRe.MatchString(jobID) {
			writeError(w, http.StatusBadRequest, "invalid_job_id", "invalid job_id")
			return
		}
		allowed, err := s.authorizeJobAccess(auth, jobID)
		if err != nil {
			writeError(w, http.StatusNotFound, "job_not_found", "job not found")
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "forbidden", "forbidden")
			return
		}
		tailBytes, err := parseTailBytes(r, max(s.cfg.Limits.MaxStdoutLogBytes, s.cfg.Limits.MaxStderrLogBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_tail_bytes", err.Error())
			return
		}
		b, err := s.store.ReadOutputTail(jobID, path.Clean(name), tailBytes)
		if err != nil {
			writeError(w, http.StatusNotFound, "output_not_found", "output not found")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}
}

func (s *Server) authenticateRequest(w http.ResponseWriter, r *http.Request) (AuthInfo, bool) {
	peer := peerAddress(r)
	remote := clientAddress(r)
	auth, err := authenticate(r, s.cfg)
	if err != nil {
		if s.authLimiter != nil && s.authLimiter.recordFailureAndExceeded(peer) {
			logEvent("auth_rejected", map[string]any{
				"reason":      "rate_limited",
				"peer_addr":   peer,
				"remote_addr": remote,
			})
			writeError(w, http.StatusTooManyRequests, "auth_rate_limited", "too many authentication failures")
			return AuthInfo{}, false
		}
		logEvent("auth_rejected", map[string]any{
			"reason":      "unauthorized",
			"peer_addr":   peer,
			"remote_addr": remote,
		})
		writeError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return AuthInfo{}, false
	}
	if s.authLimiter != nil {
		s.authLimiter.recordSuccess(peer)
	}
	return auth, true
}

func (s *Server) authorizeJobAccess(auth AuthInfo, jobID string) (bool, error) {
	return s.lifecycle.authorizeJobAccess(auth, jobID)
}

func parseTailBytes(r *http.Request, maxTailBytes int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("tail_bytes"))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("tail_bytes must be a non-negative integer")
	}
	if maxTailBytes > 0 && n > maxTailBytes {
		return 0, errors.New("tail_bytes is too large")
	}
	return n, nil
}

func readLimitedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func decodeRunRequest(body []byte) (RunRequest, error) {
	var req RunRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return RunRequest{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return RunRequest{}, errors.New("request contains multiple JSON values")
		}
		return RunRequest{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return RunRequest{}, err
	}
	_, req.waitSecSet = raw["wait_sec"]
	return req, nil
}

func requestDecodeErrorCode(err error) string {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "invalid_json"
	}
	return "invalid_request"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeErrorRetry(w, status, code, message, 0)
}

func writeErrorRetry(w http.ResponseWriter, status int, code, message string, retryAfterSec int) {
	if retryAfterSec > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
	}
	writeJSON(w, status, ErrorResponse{Error: message, Code: code, RetryAfterSec: retryAfterSec})
}

func requestHash(req RunRequest) string {
	canonical := req
	canonical.Root = nil
	b, err := json.Marshal(canonical)
	if err != nil {
		return hashBytes([]byte(canonical.Cmd))
	}
	return hashBytes(b)
}

func clientAddress(r *http.Request) string {
	peer := peerAddress(r)
	if isLoopbackAddress(peer) {
		if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
			if first, _, ok := strings.Cut(forwarded, ","); ok {
				return strings.TrimSpace(first)
			}
			return forwarded
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
			return realIP
		}
	}
	return peer
}

func peerAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func isLoopbackAddress(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) writeAudit(j *job, req RunRequest, res RunResult) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	return writeAuditEntry(s.cfg.Storage.LogDir, newAuditEntry(time.Now().UTC(), j, req, res))
}

func (s *Server) cleanupJobs() {
	report, err := s.lifecycle.cleanupJobs()
	if err != nil {
		logEvent("job_cleanup_failed", map[string]any{"phase": "runtime", "error": err.Error()})
		return
	}
	if len(report.DeletedIDs) > 0 {
		logEvent("job_cleanup_completed", map[string]any{
			"phase":         "runtime",
			"deleted_jobs":  len(report.DeletedIDs),
			"deleted_bytes": report.DeletedBytes,
		})
	}
}
