package execd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

type Config struct {
	Listen    string          `json:"listen"`
	Tokens    []TokenConfig   `json:"tokens"`
	Limits    LimitsConfig    `json:"limits"`
	Security  SecurityConfig  `json:"security"`
	Execution ExecutionConfig `json:"execution"`
	Env       EnvConfig       `json:"env"`
	Storage   StorageConfig   `json:"storage"`
	Helpers   HelpersConfig   `json:"helpers"`
}

type TokenConfig struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	AllowRoot bool   `json:"allow_root"`
}

type LimitsConfig struct {
	DefaultTimeoutSec     int   `json:"default_timeout_sec"`
	MaxTimeoutSec         int   `json:"max_timeout_sec"`
	DefaultWaitSec        int   `json:"default_wait_sec"`
	MaxWaitSec            int   `json:"max_wait_sec"`
	DefaultKillGraceSec   int   `json:"default_kill_grace_sec"`
	MaxRequestBytes       int64 `json:"max_request_bytes"`
	MaxCmdBytes           int64 `json:"max_cmd_bytes"`
	MaxStdinBytes         int64 `json:"max_stdin_bytes"`
	DefaultStdoutBytes    int64 `json:"default_stdout_bytes"`
	DefaultStderrBytes    int64 `json:"default_stderr_bytes"`
	MaxStdoutBytes        int64 `json:"max_stdout_bytes"`
	MaxStderrBytes        int64 `json:"max_stderr_bytes"`
	DefaultStdoutLogBytes int64 `json:"default_stdout_log_bytes"`
	DefaultStderrLogBytes int64 `json:"default_stderr_log_bytes"`
	MaxStdoutLogBytes     int64 `json:"max_stdout_log_bytes"`
	MaxStderrLogBytes     int64 `json:"max_stderr_log_bytes"`
	Concurrency           int   `json:"concurrency"`
	MaxJobsRetained       int   `json:"max_jobs_retained"`
}

type SecurityConfig struct {
	AuthFailureLimit     int `json:"auth_failure_limit"`
	AuthFailureWindowSec int `json:"auth_failure_window_sec"`
}

type ExecutionConfig struct {
	ShellPath                 string   `json:"shell_path"`
	ShellArgs                 []string `json:"shell_args"`
	FixedPath                 string   `json:"fixed_path"`
	AllowAnyCwdForRoot        bool     `json:"allow_any_cwd_for_root"`
	AllowedCwdPrefixesForUser []string `json:"allowed_cwd_prefixes_for_user"`
	RunUser                   string   `json:"run_user"`
	RootHome                  string   `json:"root_home"`
	RunHome                   string   `json:"run_home"`
}

type EnvConfig struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

type StorageConfig struct {
	JobDir           string `json:"job_dir"`
	LogDir           string `json:"log_dir"`
	RetentionDays    int    `json:"retention_days"`
	MaxTotalJobBytes int64  `json:"max_total_job_bytes"`
}

type HelpersConfig struct {
	SudoPath      string `json:"sudo_path"`
	RunChildPath  string `json:"run_child_path"`
	RootChildPath string `json:"root_child_path"`
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func DefaultConfig() Config {
	return Config{
		Listen: "127.0.0.1:7843",
		Limits: LimitsConfig{
			DefaultTimeoutSec:     30,
			MaxTimeoutSec:         300,
			DefaultWaitSec:        25,
			MaxWaitSec:            60,
			DefaultKillGraceSec:   3,
			MaxRequestBytes:       128 << 10,
			MaxCmdBytes:           8 << 10,
			MaxStdinBytes:         64 << 10,
			DefaultStdoutBytes:    256 << 10,
			DefaultStderrBytes:    256 << 10,
			MaxStdoutBytes:        1 << 20,
			MaxStderrBytes:        1 << 20,
			DefaultStdoutLogBytes: 1 << 20,
			DefaultStderrLogBytes: 1 << 20,
			MaxStdoutLogBytes:     16 << 20,
			MaxStderrLogBytes:     16 << 20,
			Concurrency:           1,
			MaxJobsRetained:       1000,
		},
		Security: SecurityConfig{
			AuthFailureLimit:     10,
			AuthFailureWindowSec: 60,
		},
		Execution: ExecutionConfig{
			ShellPath:                 "/bin/bash",
			ShellArgs:                 []string{"--noprofile", "--norc", "-o", "pipefail", "-c"},
			FixedPath:                 "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			AllowAnyCwdForRoot:        true,
			AllowedCwdPrefixesForUser: []string{"/opt", "/srv", "/var/www", "/tmp", "/var/log"},
			RunUser:                   "aiops-run",
			RootHome:                  "/root",
			RunHome:                   "/var/lib/aiops-run",
		},
		Env: EnvConfig{
			Allow: []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"},
			Deny:  []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "BASH_ENV", "ENV", "GIT_SSH_COMMAND", "SSH_AUTH_SOCK", "PYTHONPATH", "NODE_OPTIONS"},
		},
		Storage: StorageConfig{
			JobDir:           "/var/lib/aiops-execd/jobs",
			LogDir:           "/var/log/aiops-execd",
			RetentionDays:    7,
			MaxTotalJobBytes: 100 << 20,
		},
		Helpers: HelpersConfig{
			SudoPath:      "/usr/bin/sudo",
			RunChildPath:  "/usr/local/libexec/aiops-execd-run-child",
			RootChildPath: "/usr/local/libexec/aiops-execd-root-child",
		},
	}
}

func (c Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen is required")
	}
	if len(c.Tokens) == 0 {
		return errors.New("at least one token is required")
	}
	seen := map[string]bool{}
	for _, t := range c.Tokens {
		if t.ID == "" {
			return errors.New("token id is required")
		}
		if seen[t.ID] {
			return fmt.Errorf("duplicate token id %q", t.ID)
		}
		seen[t.ID] = true
		if len(t.SHA256) != 64 {
			return fmt.Errorf("token %q sha256 must be hex sha256", t.ID)
		}
		if _, err := hex.DecodeString(t.SHA256); err != nil {
			return fmt.Errorf("token %q sha256 is invalid: %w", t.ID, err)
		}
	}
	if c.Limits.Concurrency < 1 {
		return errors.New("limits.concurrency must be >= 1")
	}
	if c.Limits.MaxTimeoutSec < 1 || c.Limits.DefaultTimeoutSec < 1 || c.Limits.DefaultTimeoutSec > c.Limits.MaxTimeoutSec {
		return errors.New("invalid timeout limits")
	}
	if c.Limits.MaxWaitSec < 0 || c.Limits.DefaultWaitSec < 0 || c.Limits.DefaultWaitSec > c.Limits.MaxWaitSec {
		return errors.New("invalid wait limits")
	}
	if c.Security.AuthFailureLimit < 0 || c.Security.AuthFailureWindowSec < 0 {
		return errors.New("security auth failure limits must be >= 0")
	}
	if c.Security.AuthFailureLimit > 0 && c.Security.AuthFailureWindowSec == 0 {
		return errors.New("security.auth_failure_window_sec is required when auth_failure_limit is enabled")
	}
	if c.Execution.ShellPath == "" || len(c.Execution.ShellArgs) == 0 {
		return errors.New("execution shell_path and shell_args are required")
	}
	if c.Execution.RunUser == "" {
		return errors.New("execution.run_user is required")
	}
	if c.Storage.JobDir == "" || c.Storage.LogDir == "" {
		return errors.New("storage job_dir and log_dir are required")
	}
	if c.Helpers.SudoPath == "" || c.Helpers.RunChildPath == "" || c.Helpers.RootChildPath == "" {
		return errors.New("helper paths are required")
	}
	return nil
}

func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func durationSeconds(sec int) time.Duration {
	return time.Duration(sec) * time.Second
}
