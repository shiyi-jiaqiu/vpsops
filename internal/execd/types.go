package execd

import "time"

const (
	PrivilegeUser = "user"
	PrivilegeRoot = "root"

	StateQueued      = "queued"
	StateRunning     = "running"
	StateSucceeded   = "succeeded"
	StateFailed      = "failed"
	StateTimedOut    = "timed_out"
	StateCanceled    = "canceled"
	StateStartFailed = "start_failed"
)

type RunRequest struct {
	Mode              string            `json:"mode"`
	Cmd               string            `json:"cmd"`
	Argv              []string          `json:"argv"`
	Privilege         string            `json:"privilege"`
	Root              *bool             `json:"root,omitempty"`
	Cwd               string            `json:"cwd"`
	Env               map[string]string `json:"env"`
	Stdin             string            `json:"stdin"`
	TimeoutSec        int               `json:"timeout_sec"`
	WaitSec           int               `json:"wait_sec"`
	KillGraceSec      int               `json:"kill_grace_sec"`
	MaxStdoutBytes    int64             `json:"max_stdout_bytes"`
	MaxStderrBytes    int64             `json:"max_stderr_bytes"`
	MaxStdoutLogBytes int64             `json:"max_stdout_log_bytes"`
	MaxStderrLogBytes int64             `json:"max_stderr_log_bytes"`
	IdempotencyKey    string            `json:"idempotency_key"`
	LockKey           string            `json:"lock_key"`
	waitSecSet        bool
}

type RunResult struct {
	JobID              string    `json:"job_id"`
	State              string    `json:"state"`
	ExitCode           int       `json:"exit_code"`
	Signal             *string   `json:"signal"`
	TimedOut           bool      `json:"timed_out"`
	Stdout             string    `json:"stdout,omitempty"`
	Stderr             string    `json:"stderr,omitempty"`
	StdoutTruncated    bool      `json:"stdout_truncated"`
	StderrTruncated    bool      `json:"stderr_truncated"`
	StdoutLogTruncated bool      `json:"stdout_log_truncated"`
	StderrLogTruncated bool      `json:"stderr_log_truncated"`
	DurationMS         int64     `json:"duration_ms"`
	StartedAt          time.Time `json:"started_at"`
	FinishedAt         time.Time `json:"finished_at"`
	Error              string    `json:"error,omitempty"`
}

type JobSummary struct {
	JobID      string     `json:"job_id"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Result     *RunResult `json:"result,omitempty"`
}

type ErrorResponse struct {
	Error         string `json:"error"`
	Code          string `json:"code"`
	RetryAfterSec int    `json:"retry_after_sec,omitempty"`
}

type JobMetadata struct {
	JobID          string    `json:"job_id"`
	TokenID        string    `json:"token_id"`
	Privilege      string    `json:"privilege"`
	RemoteAddr     string    `json:"remote_addr"`
	LockKey        string    `json:"lock_key,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	RequestHash    string    `json:"request_hash,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type childSpec struct {
	Mode              string            `json:"mode"`
	Cmd               string            `json:"cmd"`
	Argv              []string          `json:"argv"`
	Privilege         string            `json:"privilege"`
	Cwd               string            `json:"cwd"`
	Env               []string          `json:"env"`
	Stdin             string            `json:"stdin"`
	TimeoutSec        int               `json:"timeout_sec"`
	KillGraceSec      int               `json:"kill_grace_sec"`
	MaxStdoutLogBytes int64             `json:"max_stdout_log_bytes"`
	MaxStderrLogBytes int64             `json:"max_stderr_log_bytes"`
	Execution         ExecutionConfig   `json:"execution"`
	Extra             map[string]string `json:"extra,omitempty"`
}

type childResult struct {
	State              string  `json:"state"`
	ExitCode           int     `json:"exit_code"`
	Signal             *string `json:"signal"`
	TimedOut           bool    `json:"timed_out"`
	StdoutLogTruncated bool    `json:"stdout_log_truncated"`
	StderrLogTruncated bool    `json:"stderr_log_truncated"`
	Error              string  `json:"error,omitempty"`
}
