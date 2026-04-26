package execd

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var (
	envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	jobIDRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{8,80}$`)
)

func normalizeRequest(req *RunRequest, cfg Config) error {
	if req.Root != nil {
		if *req.Root {
			if req.Privilege != "" && req.Privilege != PrivilegeRoot {
				return errors.New("root=true conflicts with privilege")
			}
			req.Privilege = PrivilegeRoot
		} else {
			if req.Privilege == PrivilegeRoot {
				return errors.New("root=false conflicts with privilege=root")
			}
			if req.Privilege == "" {
				req.Privilege = PrivilegeUser
			}
		}
	}
	if req.Privilege == "" {
		req.Privilege = PrivilegeUser
	}
	if req.Privilege != PrivilegeUser && req.Privilege != PrivilegeRoot {
		return errors.New("privilege must be user or root")
	}
	if req.Mode == "" {
		req.Mode = "shell"
	}
	if req.Mode != "shell" && req.Mode != "argv" {
		return errors.New("mode must be shell or argv")
	}
	if req.Mode == "shell" {
		if strings.TrimSpace(req.Cmd) == "" {
			return errors.New("cmd is required for shell mode")
		}
		if int64(len(req.Cmd)) > cfg.Limits.MaxCmdBytes {
			return errors.New("cmd is too large")
		}
	} else {
		if len(req.Argv) == 0 || req.Argv[0] == "" {
			return errors.New("argv[0] is required for argv mode")
		}
		total := 0
		for _, arg := range req.Argv {
			total += len(arg)
		}
		if int64(total) > cfg.Limits.MaxCmdBytes {
			return errors.New("argv is too large")
		}
	}
	if int64(len(req.Stdin)) > cfg.Limits.MaxStdinBytes {
		return errors.New("stdin is too large")
	}
	if req.TimeoutSec == 0 {
		req.TimeoutSec = cfg.Limits.DefaultTimeoutSec
	}
	if req.TimeoutSec < 1 || req.TimeoutSec > cfg.Limits.MaxTimeoutSec {
		return fmt.Errorf("timeout_sec must be 1..%d", cfg.Limits.MaxTimeoutSec)
	}
	if req.WaitSec == 0 {
		req.WaitSec = cfg.Limits.DefaultWaitSec
	}
	if req.WaitSec < 0 || req.WaitSec > cfg.Limits.MaxWaitSec {
		return fmt.Errorf("wait_sec must be 0..%d", cfg.Limits.MaxWaitSec)
	}
	if req.KillGraceSec == 0 {
		req.KillGraceSec = cfg.Limits.DefaultKillGraceSec
	}
	if req.KillGraceSec < 1 || req.KillGraceSec > 30 {
		return errors.New("kill_grace_sec must be 1..30")
	}
	if req.MaxStdoutBytes == 0 {
		req.MaxStdoutBytes = cfg.Limits.DefaultStdoutBytes
	}
	if req.MaxStderrBytes == 0 {
		req.MaxStderrBytes = cfg.Limits.DefaultStderrBytes
	}
	if req.MaxStdoutLogBytes == 0 {
		req.MaxStdoutLogBytes = cfg.Limits.DefaultStdoutLogBytes
	}
	if req.MaxStderrLogBytes == 0 {
		req.MaxStderrLogBytes = cfg.Limits.DefaultStderrLogBytes
	}
	if req.MaxStdoutBytes < 0 || req.MaxStdoutBytes > cfg.Limits.MaxStdoutBytes {
		return errors.New("max_stdout_bytes is out of range")
	}
	if req.MaxStderrBytes < 0 || req.MaxStderrBytes > cfg.Limits.MaxStderrBytes {
		return errors.New("max_stderr_bytes is out of range")
	}
	if req.MaxStdoutLogBytes < req.MaxStdoutBytes || req.MaxStdoutLogBytes > cfg.Limits.MaxStdoutLogBytes {
		return errors.New("max_stdout_log_bytes is out of range")
	}
	if req.MaxStderrLogBytes < req.MaxStderrBytes || req.MaxStderrLogBytes > cfg.Limits.MaxStderrLogBytes {
		return errors.New("max_stderr_log_bytes is out of range")
	}
	cwd, err := normalizeCwd(req.Cwd, req.Privilege, cfg.Execution)
	if err != nil {
		return err
	}
	req.Cwd = cwd
	if err := validateEnv(req.Env, cfg.Env); err != nil {
		return err
	}
	return nil
}

func normalizeCwd(cwd, privilege string, cfg ExecutionConfig) (string, error) {
	if cwd == "" {
		if privilege == PrivilegeRoot {
			cwd = "/"
		} else {
			cwd = "/tmp"
		}
	}
	if !filepath.IsAbs(cwd) {
		return "", errors.New("cwd must be absolute")
	}
	cleaned, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return "", errors.New("cwd must exist")
	}
	if privilege == PrivilegeRoot && cfg.AllowAnyCwdForRoot {
		return cleaned, nil
	}
	for _, prefix := range cfg.AllowedCwdPrefixesForUser {
		cleanPrefix := filepath.Clean(prefix)
		if resolvedPrefix, err := filepath.EvalSymlinks(cleanPrefix); err == nil {
			cleanPrefix = resolvedPrefix
		}
		if pathHasPrefix(cleaned, cleanPrefix) {
			return cleaned, nil
		}
	}
	return "", errors.New("cwd is outside allowed prefixes")
}

func pathHasPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func validateEnv(env map[string]string, cfg EnvConfig) error {
	allow := map[string]bool{}
	for _, k := range cfg.Allow {
		allow[k] = true
	}
	for k, v := range env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid env key %q", k)
		}
		if slices.Contains(cfg.Deny, k) {
			return fmt.Errorf("env key %q is denied", k)
		}
		if len(allow) > 0 && !allow[k] {
			return fmt.Errorf("env key %q is not allowed", k)
		}
		if len(v) > 4096 {
			return fmt.Errorf("env value %q is too large", k)
		}
	}
	return nil
}

func cleanEnv(reqEnv map[string]string, privilege string, cfg Config) []string {
	home := cfg.Execution.RunHome
	if privilege == PrivilegeRoot {
		home = cfg.Execution.RootHome
	}
	env := []string{
		"PATH=" + cfg.Execution.FixedPath,
		"LANG=C.UTF-8",
		"HOME=" + home,
	}
	for k, v := range reqEnv {
		env = append(env, k+"="+v)
	}
	return env
}
