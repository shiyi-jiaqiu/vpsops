package execd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type auditEntry struct {
	TS          time.Time `json:"ts"`
	JobID       string    `json:"job_id"`
	RemoteAddr  string    `json:"remote_addr"`
	TokenID     string    `json:"token_id"`
	Privilege   string    `json:"privilege"`
	Cwd         string    `json:"cwd"`
	CommandHash string    `json:"cmd_hash"`
	CommandHint string    `json:"cmd_preview,omitempty"`
	State       string    `json:"state"`
	ExitCode    int       `json:"exit_code"`
	TimedOut    bool      `json:"timed_out"`
	DurationMS  int64     `json:"duration_ms"`
}

var auditPreviewRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer[[:space:]]+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)(--(?:token|password|passwd|secret|api-key|api_key)(?:=|[[:space:]]+))["']?[^"'\s;&|]+`),
	regexp.MustCompile(`(?i)((?:token|password|passwd|secret|api[_-]?key)(?:=|:[[:space:]]*))["']?[^"'\s;&|]+`),
}

func newAuditEntry(ts time.Time, j *job, req RunRequest, res RunResult) auditEntry {
	return auditEntry{
		TS:          ts,
		JobID:       j.id,
		RemoteAddr:  j.remote,
		TokenID:     j.tokenID,
		Privilege:   req.Privilege,
		Cwd:         req.Cwd,
		CommandHash: hashBytes([]byte(auditCommand(req))),
		CommandHint: auditPreview(req),
		State:       res.State,
		ExitCode:    res.ExitCode,
		TimedOut:    res.TimedOut,
		DurationMS:  res.DurationMS,
	}
}

func writeAuditEntry(logDir string, entry auditEntry) error {
	f, err := os.OpenFile(filepath.Join(logDir, "audit.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(entry)
}

func auditCommand(req RunRequest) string {
	if req.Mode == "argv" {
		if b, err := json.Marshal(req.Argv); err == nil {
			return string(b)
		}
		return strings.Join(req.Argv, "\x00")
	}
	return req.Cmd
}

func auditPreview(req RunRequest) string {
	const maxPreview = 200
	preview := auditCommand(req)
	preview = strings.ReplaceAll(preview, "\n", `\n`)
	preview = strings.ReplaceAll(preview, "\r", `\r`)
	for _, redactor := range auditPreviewRedactors {
		preview = redactor.ReplaceAllString(preview, "${1}[REDACTED]")
	}
	if len(preview) <= maxPreview {
		return preview
	}
	return preview[:maxPreview] + "..."
}
