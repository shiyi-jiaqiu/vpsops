package execd

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type JobStore struct {
	dir string
}

type CleanupReport struct {
	DeletedIDs     []string
	DeletedBytes   int64
	RemainingJobs  int
	RemainingBytes int64
}

type jobDirInfo struct {
	id      string
	path    string
	created time.Time
	size    int64
}

func NewJobStore(dir string) (*JobStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &JobStore{dir: dir}, nil
}

func (s *JobStore) jobDir(jobID string) (string, error) {
	if !jobIDRe.MatchString(jobID) {
		return "", errors.New("invalid job_id")
	}
	return filepath.Join(s.dir, jobID), nil
}

func (s *JobStore) Create(jobID string, req RunRequest) error {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	return writeJSONAtomic(filepath.Join(dir, "input.json"), req, 0600)
}

func (s *JobStore) SaveMetadata(meta JobMetadata) error {
	dir, err := s.jobDir(meta.JobID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(dir, "metadata.json"), meta, 0600)
}

func (s *JobStore) ReadMetadata(jobID string) (JobMetadata, error) {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return JobMetadata{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return JobMetadata{}, err
	}
	var meta JobMetadata
	if err := json.Unmarshal(b, &meta); err != nil {
		return JobMetadata{}, err
	}
	return meta, nil
}

func (s *JobStore) OpenOutput(jobID, name string) (*os.File, error) {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return nil, err
	}
	switch name {
	case "stdout.log", "stderr.log":
	default:
		return nil, errors.New("invalid output file")
	}
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
}

func (s *JobStore) SaveResult(res RunResult) error {
	dir, err := s.jobDir(res.JobID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(dir, "result.json"), res, 0600)
}

func (s *JobStore) ReadResult(jobID string) (RunResult, error) {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return RunResult{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return RunResult{}, err
	}
	var res RunResult
	if err := json.Unmarshal(b, &res); err != nil {
		return RunResult{}, err
	}
	return res, nil
}

func (s *JobStore) Remove(jobID string) error {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func (s *JobStore) ReadOutput(jobID, name string) ([]byte, error) {
	dir, err := s.jobDir(jobID)
	if err != nil {
		return nil, err
	}
	switch name {
	case "stdout.log", "stderr.log":
	default:
		return nil, errors.New("invalid output file")
	}
	return os.ReadFile(filepath.Join(dir, name))
}

func (s *JobStore) ReadOutputTail(jobID, name string, tailBytes int64) ([]byte, error) {
	if tailBytes <= 0 {
		return s.ReadOutput(jobID, name)
	}
	dir, err := s.jobDir(jobID)
	if err != nil {
		return nil, err
	}
	switch name {
	case "stdout.log", "stderr.log":
	default:
		return nil, errors.New("invalid output file")
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > tailBytes {
		if _, err := f.Seek(-tailBytes, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}

func (s *JobStore) Cleanup(storage StorageConfig, limits LimitsConfig, protected map[string]bool) (CleanupReport, error) {
	jobs, err := s.listJobDirs()
	if err != nil {
		return CleanupReport{}, err
	}
	report := CleanupReport{}
	totalBytes := int64(0)
	for _, j := range jobs {
		totalBytes += j.size
	}

	deleteSet := map[string]jobDirInfo{}
	markDelete := func(j jobDirInfo) {
		if protected[j.id] {
			return
		}
		if _, ok := deleteSet[j.id]; ok {
			return
		}
		deleteSet[j.id] = j
	}

	if storage.RetentionDays > 0 {
		cutoff := time.Now().Add(-time.Duration(storage.RetentionDays) * 24 * time.Hour)
		for _, j := range jobs {
			if j.created.Before(cutoff) {
				markDelete(j)
			}
		}
	}

	sort.Slice(jobs, func(i, k int) bool {
		return jobs[i].created.After(jobs[k].created)
	})
	if limits.MaxJobsRetained > 0 {
		kept := 0
		for _, j := range jobs {
			if protected[j.id] {
				kept++
				continue
			}
			if _, deleting := deleteSet[j.id]; deleting {
				continue
			}
			if kept >= limits.MaxJobsRetained {
				markDelete(j)
				continue
			}
			kept++
		}
	}

	totalAfterDeletes := totalBytes
	for _, j := range deleteSet {
		totalAfterDeletes -= j.size
	}
	if storage.MaxTotalJobBytes > 0 && totalAfterDeletes > storage.MaxTotalJobBytes {
		sort.Slice(jobs, func(i, k int) bool {
			return jobs[i].created.Before(jobs[k].created)
		})
		for _, j := range jobs {
			if totalAfterDeletes <= storage.MaxTotalJobBytes {
				break
			}
			if protected[j.id] {
				continue
			}
			if _, deleting := deleteSet[j.id]; deleting {
				continue
			}
			markDelete(j)
			totalAfterDeletes -= j.size
		}
	}

	for _, j := range deleteSet {
		if err := os.RemoveAll(j.path); err != nil {
			return report, err
		}
		report.DeletedIDs = append(report.DeletedIDs, j.id)
		report.DeletedBytes += j.size
	}
	sort.Strings(report.DeletedIDs)
	report.RemainingJobs = len(jobs) - len(deleteSet)
	report.RemainingBytes = totalBytes - report.DeletedBytes
	return report, nil
}

func (s *JobStore) listJobDirs() ([]jobDirInfo, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	jobs := make([]jobDirInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !jobIDRe.MatchString(entry.Name()) {
			continue
		}
		fullPath := filepath.Join(s.dir, entry.Name())
		info, err := os.Stat(fullPath)
		if err != nil {
			return nil, err
		}
		created := info.ModTime()
		if meta, err := s.ReadMetadata(entry.Name()); err == nil && !meta.CreatedAt.IsZero() {
			created = meta.CreatedAt
		}
		size, err := dirSize(fullPath)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, jobDirInfo{
			id:      entry.Name(),
			path:    fullPath,
			created: created,
			size:    size,
		})
	}
	return jobs, nil
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Clean(p) == filepath.Clean(root) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func writeJSONAtomic(path string, v any, mode os.FileMode) error {
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
