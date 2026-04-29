package execd

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventLogLineIsStructuredJSON(t *testing.T) {
	line, err := eventLogLine(time.Unix(1, 0).UTC(), "job_finished", map[string]any{
		"job_id": "20260429T120000-event",
		"event":  "ignored",
		"ts":     "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(line, &record); err != nil {
		t.Fatalf("event log line is not json: %s", string(line))
	}
	if record["event"] != "job_finished" || record["job_id"] != "20260429T120000-event" {
		t.Fatalf("unexpected event record: %#v", record)
	}
	if record["ts"] == "ignored" {
		t.Fatalf("caller fields should not override reserved ts: %#v", record)
	}
}

func TestJobEventFieldsDoNotLogCommandsOrIdempotencyValue(t *testing.T) {
	j := &job{id: "20260429T120000-event", tokenID: "ai-run", remote: "203.0.113.10"}
	fields := jobEventFields(j, RunRequest{
		Cmd:            "echo secret",
		Privilege:      PrivilegeUser,
		Cwd:            "/tmp",
		LockKey:        "deploy:app",
		IdempotencyKey: "idem-secret",
	})
	if _, ok := fields["cmd"]; ok {
		t.Fatalf("command should not be part of event fields: %#v", fields)
	}
	if _, ok := fields["idempotency_key"]; ok {
		t.Fatalf("idempotency value should not be part of event fields: %#v", fields)
	}
	if fields["has_idempotency_key"] != true {
		t.Fatalf("expected idempotency presence marker: %#v", fields)
	}
}
