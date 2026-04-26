package execd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	doctorPass = "PASS"
	doctorWarn = "WARN"
	doctorFail = "FAIL"
)

type DoctorOptions struct {
	ProbeSudo bool
}

type doctorCheck struct {
	Status string
	Name   string
	Detail string
}

func RunDoctor(configPath string, opts DoctorOptions, out io.Writer) int {
	var checks []doctorCheck
	cfg, err := LoadConfig(configPath)
	if err != nil {
		checks = append(checks, doctorCheck{Status: doctorFail, Name: "config", Detail: err.Error()})
		writeDoctorReport(out, checks)
		return 1
	}
	checks = append(checks, doctorCheck{Status: doctorPass, Name: "config", Detail: "loaded " + configPath})
	checks = append(checks, doctorStaticChecks(configPath, cfg)...)
	if opts.ProbeSudo {
		checks = append(checks, doctorProbeChecks(cfg)...)
	} else {
		checks = append(checks, doctorCheck{Status: doctorWarn, Name: "sudo_probe", Detail: "skipped; rerun with -doctor-probe on the target host"})
	}
	writeDoctorReport(out, checks)
	for _, c := range checks {
		if c.Status == doctorFail {
			return 1
		}
	}
	return 0
}

func doctorStaticChecks(configPath string, cfg Config) []doctorCheck {
	checks := []doctorCheck{
		checkListen(cfg.Listen),
		checkTokenShape(cfg),
		checkExistingDir("storage.job_dir", cfg.Storage.JobDir, true),
		checkExistingDir("storage.log_dir", cfg.Storage.LogDir, true),
		checkExecutablePath("helpers.sudo_path", cfg.Helpers.SudoPath, false),
		checkExecutablePath("helpers.run_child_path", cfg.Helpers.RunChildPath, true),
		checkExecutablePath("helpers.root_child_path", cfg.Helpers.RootChildPath, true),
		checkExecutablePath("execution.shell_path", cfg.Execution.ShellPath, false),
		checkRunUser(cfg.Execution.RunUser),
	}
	if configPath != "" {
		checks = append(checks, checkConfigFile(configPath))
	}
	return checks
}

func checkListen(addr string) doctorCheck {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return doctorCheck{Status: doctorWarn, Name: "listen", Detail: "cannot parse listen address: " + err.Error()}
	}
	if host == "" {
		return doctorCheck{Status: doctorWarn, Name: "listen", Detail: addr + " has an empty host and binds all interfaces; use 127.0.0.1:port"}
	}
	if host == "localhost" {
		return doctorCheck{Status: doctorPass, Name: "listen", Detail: addr}
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return doctorCheck{Status: doctorPass, Name: "listen", Detail: addr}
	}
	return doctorCheck{Status: doctorWarn, Name: "listen", Detail: addr + " is not loopback; put this API behind a private network or strong access control"}
}

func checkTokenShape(cfg Config) doctorCheck {
	hasRoot := false
	for _, token := range cfg.Tokens {
		if token.AllowRoot {
			hasRoot = true
			break
		}
	}
	if hasRoot {
		return doctorCheck{Status: doctorPass, Name: "tokens", Detail: fmt.Sprintf("%d configured, root-capable token present", len(cfg.Tokens))}
	}
	return doctorCheck{Status: doctorWarn, Name: "tokens", Detail: fmt.Sprintf("%d configured, no root-capable token", len(cfg.Tokens))}
}

func checkExistingDir(name, path string, mustWrite bool) doctorCheck {
	info, err := os.Stat(path)
	if err != nil {
		return doctorCheck{Status: doctorFail, Name: name, Detail: err.Error()}
	}
	if !info.IsDir() {
		return doctorCheck{Status: doctorFail, Name: name, Detail: path + " is not a directory"}
	}
	if !mustWrite {
		return doctorCheck{Status: doctorPass, Name: name, Detail: path}
	}
	probe := filepath.Join(path, fmt.Sprintf(".doctor-write-test-%d", os.Getpid()))
	if err := os.WriteFile(probe, []byte("ok\n"), 0600); err != nil {
		return doctorCheck{Status: doctorFail, Name: name, Detail: path + " is not writable: " + err.Error()}
	}
	_ = os.Remove(probe)
	return doctorCheck{Status: doctorPass, Name: name, Detail: path + " writable"}
}

func checkExecutablePath(name, path string, requireRootOwned bool) doctorCheck {
	info, err := os.Stat(path)
	if err != nil {
		return doctorCheck{Status: doctorFail, Name: name, Detail: err.Error()}
	}
	if info.IsDir() {
		return doctorCheck{Status: doctorFail, Name: name, Detail: path + " is a directory"}
	}
	if info.Mode()&0111 == 0 {
		return doctorCheck{Status: doctorFail, Name: name, Detail: path + " is not executable"}
	}
	if info.Mode()&0022 != 0 {
		return doctorCheck{Status: doctorFail, Name: name, Detail: path + " is group/world writable"}
	}
	if requireRootOwned {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != 0 {
			return doctorCheck{Status: doctorFail, Name: name, Detail: path + " is not root-owned"}
		}
	}
	return doctorCheck{Status: doctorPass, Name: name, Detail: path}
}

func checkRunUser(runUser string) doctorCheck {
	if runUser == "" {
		return doctorCheck{Status: doctorFail, Name: "execution.run_user", Detail: "empty"}
	}
	cmd := exec.Command("id", "-u", runUser)
	if out, err := cmd.CombinedOutput(); err != nil {
		return doctorCheck{Status: doctorFail, Name: "execution.run_user", Detail: strings.TrimSpace(string(out))}
	}
	return doctorCheck{Status: doctorPass, Name: "execution.run_user", Detail: runUser}
}

func checkConfigFile(path string) doctorCheck {
	info, err := os.Stat(path)
	if err != nil {
		return doctorCheck{Status: doctorFail, Name: "config_file", Detail: err.Error()}
	}
	if info.IsDir() {
		return doctorCheck{Status: doctorFail, Name: "config_file", Detail: path + " is a directory"}
	}
	if info.Mode()&0007 != 0 {
		return doctorCheck{Status: doctorWarn, Name: "config_file", Detail: path + " is readable or writable by other users"}
	}
	return doctorCheck{Status: doctorPass, Name: "config_file", Detail: path}
}

func doctorProbeChecks(cfg Config) []doctorCheck {
	return []doctorCheck{
		probeChildViaSudo("sudo_probe.user_child_fd3", cfg, PrivilegeUser),
		probeChildViaSudo("sudo_probe.root_child_fd3", cfg, PrivilegeRoot),
	}
}

func probeChildViaSudo(name string, cfg Config, privilege string) doctorCheck {
	truePath := firstExistingPath("/usr/bin/true", "/bin/true")
	if truePath == "" {
		return doctorCheck{Status: doctorFail, Name: name, Detail: "cannot find true binary"}
	}
	spec := childSpec{
		Mode:              "argv",
		Argv:              []string{truePath},
		Privilege:         privilege,
		Cwd:               "/",
		Env:               cleanEnv(nil, privilege, cfg),
		TimeoutSec:        2,
		KillGraceSec:      1,
		MaxStdoutLogBytes: 1024,
		MaxStderrLogBytes: 1024,
		Execution:         cfg.Execution,
	}
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return doctorCheck{Status: doctorFail, Name: name, Detail: err.Error()}
	}

	args := []string{"-n", "-C", "4"}
	if privilege == PrivilegeUser {
		args = append(args, "-u", cfg.Execution.RunUser, cfg.Helpers.RunChildPath)
	} else {
		args = append(args, cfg.Helpers.RootChildPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.Helpers.SudoPath, args...)
	cmd.Stdin = bytes.NewReader(specBytes)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	resultR, resultW, err := os.Pipe()
	if err != nil {
		return doctorCheck{Status: doctorFail, Name: name, Detail: err.Error()}
	}
	defer resultR.Close()
	cmd.ExtraFiles = []*os.File{resultW}
	if err := cmd.Start(); err != nil {
		resultW.Close()
		return doctorCheck{Status: doctorFail, Name: name, Detail: err.Error()}
	}
	resultW.Close()
	waitErr := cmd.Wait()
	resultBytes, readErr := io.ReadAll(io.LimitReader(resultR, 64<<10))
	if readErr != nil {
		return doctorCheck{Status: doctorFail, Name: name, Detail: readErr.Error()}
	}
	if len(bytes.TrimSpace(resultBytes)) == 0 {
		detail := "missing child result on fd 3; check sudo -C 4 and Defaults:aiopsd closefrom_override"
		if stderr.Len() > 0 {
			detail += ": " + strings.TrimSpace(stderr.String())
		}
		if waitErr != nil {
			detail += ": " + waitErr.Error()
		}
		return doctorCheck{Status: doctorFail, Name: name, Detail: detail}
	}
	var res childResult
	if err := json.Unmarshal(resultBytes, &res); err != nil {
		return doctorCheck{Status: doctorFail, Name: name, Detail: "invalid child result: " + err.Error()}
	}
	if res.State != StateSucceeded || res.ExitCode != 0 || waitErr != nil {
		detail := fmt.Sprintf("state=%s exit_code=%d", res.State, res.ExitCode)
		if res.Error != "" {
			detail += " error=" + res.Error
		}
		if waitErr != nil {
			detail += " wait=" + waitErr.Error()
		}
		if stderr.Len() > 0 {
			detail += " stderr=" + strings.TrimSpace(stderr.String())
		}
		return doctorCheck{Status: doctorFail, Name: name, Detail: detail}
	}
	return doctorCheck{Status: doctorPass, Name: name, Detail: "helper wrote valid result on fd 3"}
}

func firstExistingPath(paths ...string) string {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func writeDoctorReport(out io.Writer, checks []doctorCheck) {
	for _, c := range checks {
		_, _ = fmt.Fprintf(out, "[%s] %s: %s\n", c.Status, c.Name, c.Detail)
	}
}
