package execd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeCwdPrefixBoundary(t *testing.T) {
	cfg := DefaultConfig()
	root := t.TempDir()
	opt := filepath.Join(root, "opt")
	app := filepath.Join(opt, "app")
	opt2App := filepath.Join(root, "opt2", "app")
	if err := os.MkdirAll(app, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(opt2App, 0755); err != nil {
		t.Fatal(err)
	}
	cfg.Execution.AllowedCwdPrefixesForUser = []string{opt}

	_, err := normalizeCwd(app, PrivilegeUser, cfg.Execution)
	if err != nil {
		t.Fatalf("expected app cwd to pass: %v", err)
	}
	_, err = normalizeCwd(opt2App, PrivilegeUser, cfg.Execution)
	if err == nil {
		t.Fatal("expected sibling prefix to fail validation")
	}
}

func TestNormalizeCwdRejectsSymlinkEscape(t *testing.T) {
	cfg := DefaultConfig()
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	link := filepath.Join(allowed, "link")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg.Execution.AllowedCwdPrefixesForUser = []string{allowed}

	if _, err := normalizeCwd(link, PrivilegeUser, cfg.Execution); err == nil {
		t.Fatal("expected symlink escape to fail validation")
	}
}

func TestNormalizeRequestRejectsDeniedEnv(t *testing.T) {
	cfg := DefaultConfig()
	req := RunRequest{
		Mode:       "shell",
		Cmd:        "echo ok",
		Privilege:  PrivilegeUser,
		Cwd:        "/tmp",
		Env:        map[string]string{"LD_PRELOAD": "/tmp/x.so"},
		TimeoutSec: 1,
	}
	if err := normalizeRequest(&req, cfg); err == nil {
		t.Fatal("expected denied env to fail")
	}
}

func TestNormalizeRequestLegacyRoot(t *testing.T) {
	cfg := DefaultConfig()
	root := true
	req := RunRequest{
		Mode: "shell",
		Cmd:  "id -u",
		Root: &root,
	}
	if err := normalizeRequest(&req, cfg); err != nil {
		t.Fatalf("normalize root request: %v", err)
	}
	if req.Privilege != PrivilegeRoot {
		t.Fatalf("expected root privilege, got %q", req.Privilege)
	}
	if req.Cwd != "/" {
		t.Fatalf("expected root cwd /, got %q", req.Cwd)
	}
}

func TestNormalizeRequestRejectsRootFalsePrivilegeRoot(t *testing.T) {
	cfg := DefaultConfig()
	root := false
	req := RunRequest{
		Mode:      "shell",
		Cmd:       "id -u",
		Root:      &root,
		Privilege: PrivilegeRoot,
	}
	if err := normalizeRequest(&req, cfg); err == nil {
		t.Fatal("expected root=false with privilege=root to fail")
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
  "tokens": [
    {"id": "ai-run", "sha256": "` + strings.Repeat("a", 64) + `", "allow_root": false}
  ],
  "executionn": {"allow_any_cwd_for_root": false}
}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestLoadConfigRejectsMultipleJSONValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"tokens":[{"id":"ai-run","sha256":"` + strings.Repeat("a", 64) + `"}]} {}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("expected multiple JSON values error, got %v", err)
	}
}
