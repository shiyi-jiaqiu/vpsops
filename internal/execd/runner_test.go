package execd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestSudoHelperArgs(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Execution.RunUser = "runner"
	cfg.Helpers.RunChildPath = "/helper/user"
	cfg.Helpers.RootChildPath = "/helper/root"

	userArgs := []string{"-n", "-C", "4", "-u", "runner", "/helper/user"}
	if got := sudoHelperArgs(cfg, PrivilegeUser); !reflect.DeepEqual(got, userArgs) {
		t.Fatalf("unexpected user helper args: %#v", got)
	}
	rootArgs := []string{"-n", "-C", "4", "/helper/root"}
	if got := sudoHelperArgs(cfg, PrivilegeRoot); !reflect.DeepEqual(got, rootArgs) {
		t.Fatalf("unexpected root helper args: %#v", got)
	}
}

func TestDecodeChildResultProtocolFailures(t *testing.T) {
	missing := decodeChildResult(nil, nil)
	if missing.State != StateFailed || missing.ExitCode != -1 || !strings.Contains(missing.Error, "missing child result") {
		t.Fatalf("unexpected missing-result decode: %+v", missing)
	}
	invalid := decodeChildResult([]byte("{"), nil)
	if invalid.State != StateFailed || invalid.ExitCode != -1 || !strings.Contains(invalid.Error, "invalid child result") {
		t.Fatalf("unexpected invalid-result decode: %+v", invalid)
	}
}

func TestRunViaChildWithFakeSudo(t *testing.T) {
	cfg, store, jobID, req := fakeRunnerFixture(t, fakeChildScript(0, true))

	res := runViaChild(context.Background(), cfg, store, jobID, req)
	if res.State != StateSucceeded || res.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Stdout != "child-out" || res.Stderr != "child-err" {
		t.Fatalf("unexpected output: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	stdoutLog, err := store.ReadOutput(jobID, "stdout.log")
	if err != nil {
		t.Fatal(err)
	}
	if string(stdoutLog) != "child-out" {
		t.Fatalf("unexpected stdout log: %q", string(stdoutLog))
	}
}

func TestRunViaChildMissingFd3ResultIsProtocolFailure(t *testing.T) {
	cfg, store, jobID, req := fakeRunnerFixture(t, fakeChildScript(7, false))

	res := runViaChild(context.Background(), cfg, store, jobID, req)
	if res.State != StateFailed || res.ExitCode != 7 || !strings.Contains(res.Error, "missing child result") {
		t.Fatalf("unexpected protocol failure result: %+v", res)
	}
	if res.Stdout != "child-out" {
		t.Fatalf("stdout should still be captured, got %q", res.Stdout)
	}
}

func fakeRunnerFixture(t *testing.T, childContent string) (Config, *JobStore, string, RunRequest) {
	t.Helper()
	base := t.TempDir()
	cfg := DefaultConfig()
	cfg.Storage.JobDir = filepath.Join(base, "jobs")
	cfg.Storage.LogDir = filepath.Join(base, "logs")
	cfg.Helpers.SudoPath = writeExecutable(t, filepath.Join(base, "fake-sudo"), fakeSudoScript())
	cfg.Helpers.RunChildPath = writeExecutable(t, filepath.Join(base, "run-child"), childContent)
	cfg.Helpers.RootChildPath = writeExecutable(t, filepath.Join(base, "root-child"), childContent)
	cfg.Execution.RunUser = "aiops-run"

	store, err := NewJobStore(cfg.Storage.JobDir)
	if err != nil {
		t.Fatal(err)
	}
	req := RunRequest{
		Mode:              "shell",
		Cmd:               "ignored",
		Privilege:         PrivilegeUser,
		Cwd:               "/tmp",
		TimeoutSec:        5,
		KillGraceSec:      1,
		MaxStdoutBytes:    1024,
		MaxStderrBytes:    1024,
		MaxStdoutLogBytes: 1024,
		MaxStderrLogBytes: 1024,
	}
	jobID := "20260429T120000-runner"
	if err := store.Create(jobID, req); err != nil {
		t.Fatal(err)
	}
	return cfg, store, jobID, req
}

func writeExecutable(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeSudoScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-n" ]]; then
  shift
fi
if [[ "${1:-}" == "-C" ]]; then
  shift 2
fi
if [[ "${1:-}" == "-u" ]]; then
  shift 2
fi
exec "$@"
`
}

func fakeChildScript(exitCode int, writeResult bool) string {
	result := ""
	if writeResult {
		result = `printf '{"state":"succeeded","exit_code":0,"timed_out":false,"stdout_log_truncated":false,"stderr_log_truncated":false}\n' >&3`
	}
	return `#!/usr/bin/env bash
set -euo pipefail
cat >/dev/null
printf 'child-out'
printf 'child-err' >&2
` + result + `
exit ` + strconv.Itoa(exitCode) + `
`
}
