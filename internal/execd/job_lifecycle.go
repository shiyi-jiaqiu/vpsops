package execd

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var (
	errBusy                 = errors.New("busy")
	errLockBusy             = errors.New("lock busy")
	errIdempotencyConflict  = errors.New("idempotency key reused with different request")
	errConcurrencySlotPanic = errors.New("job lifecycle concurrency slot is empty")
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

func jobDone(j *job) bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

type jobLifecycle struct {
	store       *JobStore
	storage     StorageConfig
	limits      LimitsConfig
	sem         chan struct{}
	mu          sync.Mutex
	jobs        map[string]*job
	idempotency map[string]*job
	locks       map[string]*job
	now         func() time.Time
}

func newJobLifecycle(store *JobStore, storage StorageConfig, limits LimitsConfig) *jobLifecycle {
	return &jobLifecycle{
		store:       store,
		storage:     storage,
		limits:      limits,
		sem:         make(chan struct{}, limits.Concurrency),
		jobs:        map[string]*job{},
		idempotency: map[string]*job{},
		locks:       map[string]*job{},
		now:         func() time.Time { return time.Now().UTC() },
	}
}

func (l *jobLifecycle) prepareJob(req RunRequest, reqHash string, auth AuthInfo, remote string) (*job, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if req.IdempotencyKey != "" {
		key := idempotencyIndexKey(auth.TokenID, req.IdempotencyKey)
		if existing := l.idempotency[key]; existing != nil {
			if existing.hash != reqHash {
				return nil, false, errIdempotencyConflict
			}
			return existing, true, nil
		}
	}
	if req.LockKey != "" {
		if existing := l.locks[req.LockKey]; existing != nil {
			if !jobDone(existing) {
				return nil, false, errLockBusy
			}
			delete(l.locks, req.LockKey)
		}
	}
	select {
	case l.sem <- struct{}{}:
	default:
		return nil, false, errBusy
	}

	jobID := newJobID()
	j := &job{
		id:      jobID,
		state:   StateQueued,
		done:    make(chan struct{}),
		started: l.now(),
		hash:    reqHash,
		tokenID: auth.TokenID,
		remote:  remote,
		lockKey: req.LockKey,
	}
	if err := l.store.Create(jobID, req); err != nil {
		l.releaseSlotLocked()
		return nil, false, err
	}
	if err := l.store.SaveMetadata(JobMetadata{
		JobID:          jobID,
		TokenID:        auth.TokenID,
		Privilege:      req.Privilege,
		RemoteAddr:     remote,
		LockKey:        req.LockKey,
		IdempotencyKey: req.IdempotencyKey,
		RequestHash:    reqHash,
		CreatedAt:      j.started,
	}); err != nil {
		l.releaseSlotLocked()
		_ = l.store.Remove(jobID)
		return nil, false, err
	}
	l.jobs[jobID] = j
	if req.LockKey != "" {
		l.locks[req.LockKey] = j
	}
	if req.IdempotencyKey != "" {
		l.idempotency[idempotencyIndexKey(auth.TokenID, req.IdempotencyKey)] = j
	}
	return j, false, nil
}

func (l *jobLifecycle) markRunning(j *job) {
	j.mu.Lock()
	j.state = StateRunning
	j.mu.Unlock()
}

func (l *jobLifecycle) releaseJob(j *job) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if j.lockKey != "" && l.locks[j.lockKey] == j {
		delete(l.locks, j.lockKey)
	}
	return l.releaseSlotLocked()
}

func (l *jobLifecycle) releaseSlotLocked() error {
	select {
	case <-l.sem:
		return nil
	default:
		return errConcurrencySlotPanic
	}
}

func (l *jobLifecycle) getJob(jobID string) *job {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.jobs[jobID]
}

func (l *jobLifecycle) runningCount() int {
	return len(l.sem)
}

func (l *jobLifecycle) authorizeJobAccess(auth AuthInfo, jobID string) (bool, error) {
	if auth.AllowRoot {
		return true, nil
	}
	if j := l.getJob(jobID); j != nil {
		return j.tokenID == auth.TokenID, nil
	}
	meta, err := l.store.ReadMetadata(jobID)
	if err != nil {
		return false, err
	}
	return meta.TokenID == auth.TokenID, nil
}

func (l *jobLifecycle) cleanupJobs() (CleanupReport, error) {
	report, err := l.store.Cleanup(l.storage, l.limits, l.protectedIDs())
	if err != nil {
		return report, err
	}
	l.pruneMemory(report.DeletedIDs)
	return report, nil
}

func (l *jobLifecycle) protectedIDs() map[string]bool {
	protected := map[string]bool{}
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, j := range l.jobs {
		if !jobDone(j) {
			protected[id] = true
		}
	}
	return protected
}

func (l *jobLifecycle) pruneMemory(deletedIDs []string) {
	deleteIDs := map[string]bool{}
	for _, id := range deletedIDs {
		deleteIDs[id] = true
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	var doneJobs []*job
	for _, j := range l.jobs {
		if jobDone(j) {
			doneJobs = append(doneJobs, j)
		}
	}
	if l.storage.RetentionDays > 0 {
		cutoff := l.now().Add(-time.Duration(l.storage.RetentionDays) * 24 * time.Hour)
		for _, j := range doneJobs {
			if j.started.Before(cutoff) {
				deleteIDs[j.id] = true
			}
		}
	}
	if l.limits.MaxJobsRetained > 0 {
		sort.Slice(doneJobs, func(i, k int) bool {
			return doneJobs[i].started.After(doneJobs[k].started)
		})
		for idx, j := range doneJobs {
			if idx >= l.limits.MaxJobsRetained {
				deleteIDs[j.id] = true
			}
		}
	}
	if len(deleteIDs) == 0 {
		return
	}
	for id := range deleteIDs {
		delete(l.jobs, id)
	}
	for key, j := range l.idempotency {
		if deleteIDs[j.id] {
			delete(l.idempotency, key)
		}
	}
}

func idempotencyIndexKey(tokenID, idempotencyKey string) string {
	return tokenID + ":" + idempotencyKey
}
