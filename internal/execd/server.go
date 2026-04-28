package execd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	cfg         Config
	store       *JobStore
	http        *http.Server
	sem         chan struct{}
	mu          sync.Mutex
	auditMu     sync.Mutex
	jobs        map[string]*job
	idempotency map[string]*job
	locks       map[string]*job
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
		log.Printf("initial job cleanup: %v", err)
	} else if len(report.DeletedIDs) > 0 {
		log.Printf("initial job cleanup deleted=%d bytes=%d", len(report.DeletedIDs), report.DeletedBytes)
	}
	s := &Server{
		cfg:         cfg,
		store:       store,
		sem:         make(chan struct{}, cfg.Limits.Concurrency),
		jobs:        map[string]*job{},
		idempotency: map[string]*job{},
		locks:       map[string]*job{},
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
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.authenticateRequest(w, r)
	if !ok {
		return
	}
	body, err := readLimitedBody(w, r, s.cfg.Limits.MaxRequestBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var req RunRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := normalizeRequest(&req, s.cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Privilege == PrivilegeRoot && !auth.AllowRoot {
		writeError(w, http.StatusForbidden, "root privilege requires root-capable token")
		return
	}

	reqHash := requestHash(req)
	j, reused, err := s.prepareJob(req, reqHash, auth, clientAddress(r))
	if err != nil {
		if errors.Is(err, errBusy) {
			writeError(w, http.StatusConflict, "executor is busy")
			return
		}
		if errors.Is(err, errLockBusy) {
			writeError(w, http.StatusConflict, "lock_key is busy")
			return
		}
		if errors.Is(err, errIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency key reused with different request")
			return
		}
		log.Printf("prepare job: %v", err)
		writeError(w, http.StatusInternalServerError, "prepare job failed")
		return
	}
	if !reused {
		go s.runJob(j, req)
	}

	select {
	case <-j.done:
		writeJSON(w, http.StatusOK, j.summary().Result)
	case <-time.After(durationSeconds(req.WaitSec)):
		writeJSON(w, http.StatusAccepted, j.summary())
	}
}

var errBusy = errors.New("busy")
var errLockBusy = errors.New("lock busy")
var errIdempotencyConflict = errors.New("idempotency key reused with different request")

func (s *Server) prepareJob(req RunRequest, reqHash string, auth AuthInfo, remote string) (*job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.IdempotencyKey != "" {
		key := auth.TokenID + ":" + req.IdempotencyKey
		if existing := s.idempotency[key]; existing != nil {
			if existing.hash != reqHash {
				return nil, false, errIdempotencyConflict
			}
			return existing, true, nil
		}
	}
	if req.LockKey != "" {
		if existing := s.locks[req.LockKey]; existing != nil {
			if !jobDone(existing) {
				return nil, false, errLockBusy
			}
			delete(s.locks, req.LockKey)
		}
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return nil, false, errBusy
	}
	jobID := newJobID()
	j := &job{
		id:      jobID,
		state:   StateQueued,
		done:    make(chan struct{}),
		started: time.Now().UTC(),
		hash:    reqHash,
		tokenID: auth.TokenID,
		remote:  remote,
		lockKey: req.LockKey,
	}
	if err := s.store.Create(jobID, req); err != nil {
		<-s.sem
		return nil, false, err
	}
	if err := s.store.SaveMetadata(JobMetadata{
		JobID:      jobID,
		TokenID:    auth.TokenID,
		Privilege:  req.Privilege,
		RemoteAddr: remote,
		LockKey:    req.LockKey,
		CreatedAt:  j.started,
	}); err != nil {
		<-s.sem
		return nil, false, err
	}
	s.jobs[jobID] = j
	if req.LockKey != "" {
		s.locks[req.LockKey] = j
	}
	if req.IdempotencyKey != "" {
		s.idempotency[auth.TokenID+":"+req.IdempotencyKey] = j
	}
	return j, false, nil
}

func (s *Server) runJob(j *job, req RunRequest) {
	defer func() {
		s.releaseJob(j)
		s.cleanupJobs()
	}()
	j.mu.Lock()
	j.state = StateRunning
	j.mu.Unlock()
	ctx, cancel := helperContext(req)
	defer cancel()
	res := runViaChild(ctx, s.cfg, s.store, j.id, req)
	if err := s.store.SaveResult(res); err != nil {
		log.Printf("save result job=%s: %v", j.id, err)
	}
	if err := s.writeAudit(j, req, res); err != nil {
		log.Printf("write audit job=%s: %v", j.id, err)
	}
	j.setResult(res)
}

func (s *Server) releaseJob(j *job) {
	s.mu.Lock()
	if j.lockKey != "" && s.locks[j.lockKey] == j {
		delete(s.locks, j.lockKey)
	}
	s.mu.Unlock()
	<-s.sem
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.authenticateRequest(w, r)
	if !ok {
		return
	}
	jobID := r.PathValue("job_id")
	if !jobIDRe.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid job_id")
		return
	}
	allowed, err := s.authorizeJobAccess(auth, jobID)
	if err != nil {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	s.mu.Lock()
	j := s.jobs[jobID]
	s.mu.Unlock()
	if j != nil {
		writeJSON(w, http.StatusOK, j.summary())
		return
	}
	res, err := s.store.ReadResult(jobID)
	if err != nil {
		writeError(w, http.StatusNotFound, "job not found")
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
			writeError(w, http.StatusBadRequest, "invalid job_id")
			return
		}
		allowed, err := s.authorizeJobAccess(auth, jobID)
		if err != nil {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		tailBytes, err := parseTailBytes(r, max(s.cfg.Limits.MaxStdoutLogBytes, s.cfg.Limits.MaxStderrLogBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		b, err := s.store.ReadOutputTail(jobID, path.Clean(name), tailBytes)
		if err != nil {
			writeError(w, http.StatusNotFound, "output not found")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}
}

func (s *Server) authenticateRequest(w http.ResponseWriter, r *http.Request) (AuthInfo, bool) {
	peer := peerAddress(r)
	auth, err := authenticate(r, s.cfg)
	if err != nil {
		if s.authLimiter != nil && s.authLimiter.recordFailureAndExceeded(peer) {
			writeError(w, http.StatusTooManyRequests, "too many authentication failures")
			return AuthInfo{}, false
		}
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return AuthInfo{}, false
	}
	if s.authLimiter != nil {
		s.authLimiter.recordSuccess(peer)
	}
	return auth, true
}

func (s *Server) authorizeJobAccess(auth AuthInfo, jobID string) (bool, error) {
	if auth.AllowRoot {
		return true, nil
	}
	s.mu.Lock()
	j := s.jobs[jobID]
	s.mu.Unlock()
	if j != nil {
		return j.tokenID == auth.TokenID, nil
	}
	meta, err := s.store.ReadMetadata(jobID)
	if err != nil {
		return false, err
	}
	return meta.TokenID == auth.TokenID, nil
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
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

func jobDone(j *job) bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
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
	entry := map[string]any{
		"ts":          time.Now().UTC(),
		"job_id":      j.id,
		"remote_addr": j.remote,
		"token_id":    j.tokenID,
		"privilege":   req.Privilege,
		"cwd":         req.Cwd,
		"cmd_hash":    hashBytes([]byte(auditCommand(req))),
		"cmd_preview": auditPreview(req),
		"state":       res.State,
		"exit_code":   res.ExitCode,
		"timed_out":   res.TimedOut,
		"duration_ms": res.DurationMS,
	}

	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.cfg.Storage.LogDir, "audit.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(entry)
}

func (s *Server) cleanupJobs() {
	protected := map[string]bool{}
	s.mu.Lock()
	for id, j := range s.jobs {
		if !jobDone(j) {
			protected[id] = true
		}
	}
	s.mu.Unlock()

	report, err := s.store.Cleanup(s.cfg.Storage, s.cfg.Limits, protected)
	if err != nil {
		log.Printf("job cleanup: %v", err)
		return
	}
	if len(report.DeletedIDs) > 0 {
		log.Printf("job cleanup deleted=%d bytes=%d", len(report.DeletedIDs), report.DeletedBytes)
	}
	s.pruneMemory(report.DeletedIDs)
}

func (s *Server) pruneMemory(deletedIDs []string) {
	deleteIDs := map[string]bool{}
	for _, id := range deletedIDs {
		deleteIDs[id] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var doneJobs []*job
	for _, j := range s.jobs {
		if jobDone(j) {
			doneJobs = append(doneJobs, j)
		}
	}
	if s.cfg.Storage.RetentionDays > 0 {
		cutoff := time.Now().Add(-time.Duration(s.cfg.Storage.RetentionDays) * 24 * time.Hour)
		for _, j := range doneJobs {
			if j.started.Before(cutoff) {
				deleteIDs[j.id] = true
			}
		}
	}
	if s.cfg.Limits.MaxJobsRetained > 0 {
		sort.Slice(doneJobs, func(i, k int) bool {
			return doneJobs[i].started.After(doneJobs[k].started)
		})
		for idx, j := range doneJobs {
			if idx >= s.cfg.Limits.MaxJobsRetained {
				deleteIDs[j.id] = true
			}
		}
	}
	if len(deleteIDs) == 0 {
		return
	}
	for id := range deleteIDs {
		delete(s.jobs, id)
	}
	for key, j := range s.idempotency {
		if deleteIDs[j.id] {
			delete(s.idempotency, key)
		}
	}
}

func auditCommand(req RunRequest) string {
	if req.Mode == "argv" {
		if b, err := json.Marshal(req.Argv); err == nil {
			return string(b)
		}
		return strings.Join(req.Argv, "\x00")
	}
	return req.Cmd
}

func auditPreview(req RunRequest) string {
	const maxPreview = 200
	preview := auditCommand(req)
	preview = strings.ReplaceAll(preview, "\n", `\n`)
	preview = strings.ReplaceAll(preview, "\r", `\r`)
	if len(preview) <= maxPreview {
		return preview
	}
	return preview[:maxPreview] + "..."
}
