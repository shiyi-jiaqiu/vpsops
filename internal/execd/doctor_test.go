package execd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckListenWarnsOnPublicAddress(t *testing.T) {
	check := checkListen("0.0.0.0:7843")
	if check.Status != doctorWarn {
		t.Fatalf("expected warn for public listen, got %#v", check)
	}
}

func TestCheckListenWarnsOnEmptyHost(t *testing.T) {
	check := checkListen(":7843")
	if check.Status != doctorWarn {
		t.Fatalf("expected warn for empty host listen, got %#v", check)
	}
}

func TestCheckTokenShapeRequiresRootWarningOnly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tokens = []TokenConfig{{ID: "ai-run", SHA256: strings.Repeat("a", 64)}}
	check := checkTokenShape(cfg)
	if check.Status != doctorWarn {
		t.Fatalf("expected warn without root token, got %#v", check)
	}
}

func TestCheckExecutablePathRejectsWritableHelper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0777); err != nil {
		t.Fatal(err)
	}
	check := checkExecutablePath("helper", path, false)
	if check.Status != doctorFail {
		t.Fatalf("expected writable executable to fail, got %#v", check)
	}
}

func TestRunDoctorReportsConfigLoadFailure(t *testing.T) {
	var out bytes.Buffer
	code := RunDoctor(filepath.Join(t.TempDir(), "missing.json"), DoctorOptions{}, &out)
	if code == 0 {
		t.Fatal("expected doctor to fail for missing config")
	}
	if !strings.Contains(out.String(), "[FAIL] config:") {
		t.Fatalf("unexpected doctor output: %s", out.String())
	}
}
