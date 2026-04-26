# vpsops

`vpsops` is a lightweight remote exec gateway for AI-driven VPS operations. The server binary is `aiops-execd`; the local operator CLI is `bin/vpsops`; the local stdio MCP adapter is `bin/vpsops-mcp`.

`aiops-execd` accepts structured HTTP requests containing shell or argv commands, executes them with timeout and process-group cleanup, and returns structured JSON results.

This is not a sandbox. It is a controlled management channel for trusted AI tooling.

## Design

- Main service runs as `aiopsd`.
- User-mode commands run through `sudo -n -C 4 -u aiops-run /usr/local/libexec/aiops-execd-run-child`.
- Root-mode commands require a root-capable token and run through `sudo -n -C 4 /usr/local/libexec/aiops-execd-root-child`.
- The service listens on `127.0.0.1:7843` by default. Put Caddy, Tailscale, Cloudflare Access, or an SSH tunnel in front of it.
- Every command gets a `job_id` and writes `input.json`, `metadata.json`, `result.json`, `stdout.log`, and `stderr.log` under `/var/lib/aiops-execd/jobs`.
- Completed commands append an audit JSON line to `/var/log/aiops-execd/audit.log`. This is local troubleshooting evidence, not tamper-proof audit, because root commands can modify local files.
- Job directories are cleaned according to `retention_days`, `max_jobs_retained`, and `max_total_job_bytes`.
- Failed authentication attempts are throttled per TCP peer by the `security.auth_failure_*` settings.

## Build

```bash
go test ./...
go build -trimpath -ldflags="-s -w" -o aiops-execd ./cmd/aiops-execd
```

Install the same binary under three root-owned names:

```bash
install -o root -g root -m 0755 aiops-execd /usr/local/bin/aiops-execd
install -o root -g root -m 0755 aiops-execd /usr/local/libexec/aiops-execd-run-child
install -o root -g root -m 0755 aiops-execd /usr/local/libexec/aiops-execd-root-child
```

## Release Assets

Do not commit built Linux binaries to git. Tag releases are built by GitHub Actions for:

- `linux/amd64`
- `linux/arm64`

Create a release:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The workflow uploads:

```text
vpsops-execd_linux_amd64.tar.gz
vpsops-execd_linux_arm64.tar.gz
checksums.txt
```

Install the latest release binary on a VPS:

```bash
curl -fsSL https://raw.githubusercontent.com/shiyi-jiaqiu/vpsops/main/scripts/install-release.sh | sudo bash
```

Install a specific tag:

```bash
curl -fsSL https://raw.githubusercontent.com/shiyi-jiaqiu/vpsops/main/scripts/install-release.sh | sudo env VERSION=v0.1.0 bash
```

The install script only installs the binary under `/usr/local/bin/aiops-execd` and the two helper copies under `/usr/local/libexec/`. You still need to create users, config, sudoers, and the systemd unit.

## API

```bash
curl -sS http://127.0.0.1:7843/v1/run \
  -H "Authorization: Bearer ${AIOPS_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"mode":"shell","cmd":"id; hostname","privilege":"user","timeout_sec":10}'
```

Root execution requires a root-capable token:

```bash
curl -sS http://127.0.0.1:7843/v1/run \
  -H "Authorization: Bearer ${AIOPS_ROOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"mode":"shell","cmd":"id -u","privilege":"root","timeout_sec":10}'
```

Responses use HTTP status for API-level success and `exit_code` for command-level success. A command that exits `7` still returns HTTP `200` if it executed normally.

Job lookup endpoints also require `Authorization`. A normal token can read only jobs it created; a root-capable token can read all jobs:

```bash
curl -sS http://127.0.0.1:7843/v1/jobs/JOB_ID \
  -H "Authorization: Bearer ${AIOPS_TOKEN}"

curl -sS http://127.0.0.1:7843/v1/jobs/JOB_ID/stdout \
  -H "Authorization: Bearer ${AIOPS_TOKEN}"
```

Use `tail_bytes` to avoid pulling a full log file:

```bash
curl -sS 'http://127.0.0.1:7843/v1/jobs/JOB_ID/stdout?tail_bytes=65536' \
  -H "Authorization: Bearer ${AIOPS_TOKEN}"
```

## Local CLI

Use the local wrapper when operating a configured host. It reads `.env` from this repo root, hides the HTTPS/token details, and calls the existing `/v1/run` API. `bin/vpsops` is a stable alias for the same CLI; examples below keep `aiops` for compatibility.

```bash
cp .env.example .env
bin/aiops hosts
bin/aiops example health
bin/aiops example -- hostname
bin/aiops example --user -- id -un
bin/aiops example batch --cmd 'hostname' --cmd 'uptime' --cmd 'df -h /'
bin/aiops example docker ps
bin/aiops example service status aiops-execd
bin/aiops example file read /etc/hostname
bin/vpsops example -- hostname
```

The command form is intentionally close to SSH:

```bash
bin/vpsops <host> -- <shell command>
```

For multiple independent inspection commands, prefer one batch job instead of several separate HTTP calls:

```bash
bin/vpsops <host> batch \
  --cmd 'hostname' \
  --cmd 'uptime' \
  --cmd 'free -h' \
  --cmd 'df -h /'
```

`batch` runs commands sequentially inside one remote job. By default it continues after failed steps so diagnostics are not lost, but the final exit code is non-zero if any step failed. Use `--stop-on-error` for deployment/update sequences where later steps must not run after a failure.

By default the local CLI uses `AIOPS_DEFAULT_PRIVILEGE` from `.env`; this workspace currently uses root to match the VPS admin workflow. Add `--user` for the unprivileged execution path. The CLI follows async jobs, prints stdout/stderr directly, returns the remote `exit_code`, and retries short `executor is busy` responses because each small VPS is intentionally single-concurrency.

Host configuration supports both the original single-host variables and future host-scoped variables:

```dotenv
AIOPS_DEFAULT_HOST=example
AIOPS_HOSTS=example
AIOPS_HOST_EXAMPLE_ALIASES=ex
AIOPS_DEFAULT_PRIVILEGE=root

AIOPS_HOST_EXAMPLE_BASE=https://example.com/hidden-aiops-path
AIOPS_HOST_EXAMPLE_RUN_TOKEN=...
AIOPS_HOST_EXAMPLE_ROOT_TOKEN=...
```

## Local MCP Server

`bin/vpsops-mcp` starts a local stdio MCP server for AI clients. It does not expose a remote MCP listener and does not store tokens. The server delegates every operation to `bin/aiops`, so host config, token loading, busy retry, job polling, and result formatting remain centralized.

The MCP intentionally stays small:

- `vps_run` is the universal fallback and can run any shell command through the existing Exec API.
- `vps_batch` runs several commands sequentially in one remote job and should be preferred for multi-step inspection.
- `vps_hosts`, `vps_health`, and `vps_inspect` cover discovery and standard health checks.
- `docker_ps`, `docker_logs`, `service_status`, and `file_read` are high-frequency read-only templates to reduce quoting mistakes.

Example Codex config snippet:

```toml
[mcp_servers.vpsops]
command = "/path/to/vpsops/bin/vpsops-mcp"
args = []
```

The same snippet is stored at `mcp/codex-config.example.toml`. Existing Codex sessions may need to be restarted before the new MCP tool appears.

Minimal JSON-RPC smoke test:

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"smoke","version":"0"},"capabilities":{}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"vps_run","arguments":{"host":"example","cmd":"hostname","timeout_sec":20}}}' \
  | bin/vpsops-mcp
```

## Local Smoke Test

The local smoke test builds the binary, starts a temporary server with a fake `sudo` shim, and verifies HTTP execution, root-token enforcement, job log reads, idempotency, and `lock_key` conflicts. It does not require root and does not install anything:

```bash
./scripts/smoke-local.sh
```

## Deployment Doctor

Run the static deployment self-check after installing config, users, directories, and helper binaries:

```bash
aiops-execd -config /etc/aiops-execd/config.json -doctor
```

On the target host, run the sudo/helper probe from the daemon identity to verify that sudo permits fd 3 to survive and both helpers can return structured child results:

```bash
sudo -u aiopsd /usr/local/bin/aiops-execd \
  -config /etc/aiops-execd/config.json \
  -doctor -doctor-probe
```

The probe runs only `/usr/bin/true` or `/bin/true` through the configured user and root helper paths. It is intended to catch sudoers digest mistakes, missing `closefrom_override`, wrong helper ownership, and unreadable storage/log directories before real AI jobs are sent.

## Deployment Notes

Create users:

```bash
useradd --system --home /var/lib/aiops-execd --shell /usr/sbin/nologin aiopsd
useradd --system --home /var/lib/aiops-run --shell /usr/sbin/nologin aiops-run
mkdir -p /etc/aiops-execd /var/lib/aiops-execd/jobs /var/log/aiops-execd /usr/local/libexec
chown -R aiopsd:aiopsd /var/lib/aiops-execd /var/log/aiops-execd
chmod 700 /var/lib/aiops-execd /var/log/aiops-execd
```

Generate token hashes:

```bash
printf '%s' 'your-token' | sha256sum
```

Install and validate sudoers:

```bash
sha256sum /usr/local/libexec/aiops-execd-run-child /usr/local/libexec/aiops-execd-root-child
visudo -cf /etc/sudoers.d/aiops-execd
```

The sudoers template enables `closefrom_override` for `aiopsd`; the main service invokes `sudo -C 4` so fd 3 stays open for the child JSON result channel.

Use `deploy/aiops-execd.service`, `deploy/config.example.json`, and `deploy/sudoers.aiops-execd` as templates. Do not expose the Go service directly to the public internet.
