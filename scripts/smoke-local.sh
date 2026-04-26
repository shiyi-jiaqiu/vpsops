#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
PID=""

cleanup() {
  if [[ -n "${PID}" ]]; then
    kill "${PID}" 2>/dev/null || true
    wait "${PID}" 2>/dev/null || true
  fi
  rm -rf "${TMP}"
}
trap cleanup EXIT

hash_token() {
  python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.argv[1].encode()).hexdigest())' "$1"
}

json_get() {
  python3 -c 'import json,sys; obj=json.load(open(sys.argv[1])); print(obj.get(sys.argv[2], ""))' "$1" "$2"
}

assert_json_field() {
  local file="$1"
  local field="$2"
  local expected="$3"
  local actual
  actual="$(json_get "${file}" "${field}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "expected ${field}=${expected}, got ${actual}" >&2
    echo "response:" >&2
    cat "${file}" >&2
    exit 1
  fi
}

BIN="${TMP}/aiops-execd"
go build -trimpath -ldflags="-s -w" -o "${BIN}" "${ROOT}/cmd/aiops-execd"
ln -s "${BIN}" "${TMP}/aiops-execd-run-child"
ln -s "${BIN}" "${TMP}/aiops-execd-root-child"

cat >"${TMP}/fake-sudo" <<'EOS'
#!/usr/bin/env bash
set -Eeuo pipefail
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
EOS
chmod +x "${TMP}/fake-sudo"

RUN_TOKEN="run-token"
ROOT_TOKEN="root-token"
PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"

cat >"${TMP}/config.json" <<EOF
{
  "listen": "127.0.0.1:${PORT}",
  "tokens": [
    {"id": "ai-run", "sha256": "$(hash_token "${RUN_TOKEN}")", "allow_root": false},
    {"id": "ai-root", "sha256": "$(hash_token "${ROOT_TOKEN}")", "allow_root": true}
  ],
  "limits": {
    "default_timeout_sec": 5,
    "max_timeout_sec": 30,
    "default_wait_sec": 5,
    "max_wait_sec": 10,
    "default_kill_grace_sec": 1,
    "max_request_bytes": 131072,
    "max_cmd_bytes": 8192,
    "max_stdin_bytes": 65536,
    "default_stdout_bytes": 262144,
    "default_stderr_bytes": 262144,
    "max_stdout_bytes": 1048576,
    "max_stderr_bytes": 1048576,
    "default_stdout_log_bytes": 1048576,
    "default_stderr_log_bytes": 1048576,
    "max_stdout_log_bytes": 16777216,
    "max_stderr_log_bytes": 16777216,
    "concurrency": 2,
    "max_jobs_retained": 1000
  },
  "execution": {
    "shell_path": "/bin/bash",
    "shell_args": ["--noprofile", "--norc", "-o", "pipefail", "-c"],
    "fixed_path": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "allow_any_cwd_for_root": true,
    "allowed_cwd_prefixes_for_user": ["/tmp"],
    "run_user": "aiops-run",
    "root_home": "/root",
    "run_home": "/tmp"
  },
  "env": {
    "allow": ["HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"],
    "deny": ["LD_PRELOAD", "LD_LIBRARY_PATH", "BASH_ENV", "ENV", "GIT_SSH_COMMAND", "SSH_AUTH_SOCK", "PYTHONPATH", "NODE_OPTIONS"]
  },
  "storage": {
    "job_dir": "${TMP}/jobs",
    "log_dir": "${TMP}/logs",
    "retention_days": 7,
    "max_total_job_bytes": 104857600
  },
  "helpers": {
    "sudo_path": "${TMP}/fake-sudo",
    "run_child_path": "${TMP}/aiops-execd-run-child",
    "root_child_path": "${TMP}/aiops-execd-root-child"
  }
}
EOF

"${BIN}" -config "${TMP}/config.json" >"${TMP}/server.log" 2>&1 &
PID="$!"

for _ in {1..50}; do
  if curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 0.1
done
curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null

run_resp="${TMP}/run.json"
curl -fsS "http://127.0.0.1:${PORT}/v1/run" \
  -H "Authorization: Bearer ${RUN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"mode":"shell","cmd":"printf hello; printf err >&2","privilege":"user","cwd":"/tmp","timeout_sec":5}' \
  >"${run_resp}"
assert_json_field "${run_resp}" state succeeded
assert_json_field "${run_resp}" exit_code 0
job_id="$(json_get "${run_resp}" job_id)"

stdout_tail="$(curl -fsS "http://127.0.0.1:${PORT}/v1/jobs/${job_id}/stdout?tail_bytes=2" -H "Authorization: Bearer ${RUN_TOKEN}")"
if [[ "${stdout_tail}" != "lo" ]]; then
  echo "unexpected stdout tail: ${stdout_tail}" >&2
  exit 1
fi

unauth_code="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/v1/jobs/${job_id}/stdout")"
if [[ "${unauth_code}" != "401" ]]; then
  echo "expected unauthenticated stdout read to return 401, got ${unauth_code}" >&2
  exit 1
fi

root_forbidden_code="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/v1/run" \
  -H "Authorization: Bearer ${RUN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"mode":"shell","cmd":"printf root","privilege":"root","cwd":"/","timeout_sec":5}')"
if [[ "${root_forbidden_code}" != "403" ]]; then
  echo "expected run token root request to return 403, got ${root_forbidden_code}" >&2
  exit 1
fi

root_resp="${TMP}/root.json"
curl -fsS "http://127.0.0.1:${PORT}/v1/run" \
  -H "Authorization: Bearer ${ROOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"mode":"shell","cmd":"printf root-ok","privilege":"root","cwd":"/","timeout_sec":5}' \
  >"${root_resp}"
assert_json_field "${root_resp}" state succeeded

idem_one="${TMP}/idem1.json"
idem_two="${TMP}/idem2.json"
curl -fsS "http://127.0.0.1:${PORT}/v1/run" \
  -H "Authorization: Bearer ${RUN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"mode":"shell","cmd":"printf idem","privilege":"user","cwd":"/tmp","timeout_sec":5,"idempotency_key":"same"}' \
  >"${idem_one}"
curl -fsS "http://127.0.0.1:${PORT}/v1/run" \
  -H "Authorization: Bearer ${RUN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"cmd":"printf idem","cwd":"/tmp","idempotency_key":"same","mode":"shell","privilege":"user","timeout_sec":5}' \
  >"${idem_two}"
if [[ "$(json_get "${idem_one}" job_id)" != "$(json_get "${idem_two}" job_id)" ]]; then
  echo "expected idempotent requests to reuse job_id" >&2
  exit 1
fi

long_resp="${TMP}/long.json"
curl -fsS "http://127.0.0.1:${PORT}/v1/run" \
  -H "Authorization: Bearer ${RUN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"mode":"shell","cmd":"sleep 3; printf done","privilege":"user","cwd":"/tmp","timeout_sec":10,"wait_sec":1,"lock_key":"smoke-lock"}' \
  >"${long_resp}"
assert_json_field "${long_resp}" state running

lock_code="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/v1/run" \
  -H "Authorization: Bearer ${RUN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"mode":"shell","cmd":"printf blocked","privilege":"user","cwd":"/tmp","timeout_sec":5,"lock_key":"smoke-lock"}')"
if [[ "${lock_code}" != "409" ]]; then
  echo "expected conflicting lock request to return 409, got ${lock_code}" >&2
  exit 1
fi

echo "smoke-local: ok"
