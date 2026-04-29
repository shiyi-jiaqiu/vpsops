package execd

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

var eventLogMu sync.Mutex

func logEvent(event string, fields map[string]any) {
	line, err := eventLogLine(time.Now().UTC(), event, fields)
	if err != nil {
		return
	}
	eventLogMu.Lock()
	defer eventLogMu.Unlock()
	_, _ = os.Stderr.Write(append(line, '\n'))
}

func eventLogLine(ts time.Time, event string, fields map[string]any) ([]byte, error) {
	record := map[string]any{
		"ts":    ts,
		"event": event,
	}
	for k, v := range fields {
		if k == "ts" || k == "event" {
			continue
		}
		record[k] = v
	}
	return json.Marshal(record)
}

func runRejectFields(auth AuthInfo, r *http.Request, reason string, extras ...any) map[string]any {
	fields := map[string]any{
		"reason":      reason,
		"remote_addr": clientAddress(r),
		"peer_addr":   peerAddress(r),
		"token_id":    auth.TokenID,
	}
	for idx := 0; idx+1 < len(extras); idx += 2 {
		key, ok := extras[idx].(string)
		if !ok || key == "" {
			continue
		}
		fields[key] = extras[idx+1]
	}
	return fields
}

func jobEventFields(j *job, req RunRequest) map[string]any {
	fields := map[string]any{
		"job_id":      j.id,
		"remote_addr": j.remote,
		"token_id":    j.tokenID,
		"privilege":   req.Privilege,
		"cwd":         req.Cwd,
	}
	if req.LockKey != "" {
		fields["lock_key"] = req.LockKey
	}
	if req.IdempotencyKey != "" {
		fields["has_idempotency_key"] = true
	}
	return fields
}
