package execd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAuditPreviewRedactsCommonSecrets(t *testing.T) {
	req := RunRequest{
		Mode: "shell",
		Cmd:  `curl -H "Authorization: Bearer abcdefghijklmnop" "https://x/?token=secret123" --password hunter2 --api-key=k_12345678`,
	}
	preview := auditPreview(req)
	if strings.Contains(preview, "abcdefghijklmnop") || strings.Contains(preview, "secret123") || strings.Contains(preview, "hunter2") || strings.Contains(preview, "k_12345678") {
		t.Fatalf("audit preview leaked secret: %s", preview)
	}
	if !strings.Contains(preview, "[REDACTED]") {
		t.Fatalf("expected redaction marker in preview: %s", preview)
	}
}

func TestNewAuditEntryUsesHashAndRedactedPreview(t *testing.T) {
	req := RunRequest{Mode: "shell", Cmd: "echo token=secret123", Privilege: PrivilegeRoot, Cwd: "/"}
	j := &job{id: "20260429T120000-audit", remote: "203.0.113.10", tokenID: "ai-root"}
	res := RunResult{State: StateSucceeded, ExitCode: 0, DurationMS: 12}

	entry := newAuditEntry(time.Unix(1, 0).UTC(), j, req, res)
	if entry.CommandHash == "" {
		t.Fatal("expected command hash")
	}
	if strings.Contains(entry.CommandHint, "secret123") {
		t.Fatalf("audit preview leaked secret: %+v", entry)
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"cmd_preview"`) {
		t.Fatalf("audit entry should retain redacted preview field: %s", string(b))
	}
}
