# vpsops

`vpsops` is a lightweight remote exec gateway for AI-driven VPS operations. The server binary is `aiops-execd`; the local operator CLI is `bin/vpsops`.

`aiops-execd` accepts structured HTTP requests containing shell or argv commands, executes them with timeout and process-group cleanup, and returns structured JSON results.

This is not a sandbox. It is a controlled management channel for trusted AI tooling.

## Design

- Main service runs as `aiopsd`.
- User-mode commands run through `sudo -n -C 4 -u aiops-run /usr/local/libexec/aiops-execd-run-child`.
- Root-mode commands require a root-capable token and run through `sudo -n -C 4 /usr/local/libexec/aiops-execd-root-child`.
- The service listens on `127.0.0.1:7843` by default. Put Caddy, Tailscale, Cloudflare Access, or an SSH tunnel in front of it.
- Every command gets a `job_id` and writes `input.json`, `metadata.json`, `result.json`, `stdout.log`, and `stderr.log` under `/var/lib/aiops-execd/jobs`.
- Completed commands append an audit JSON line to `/var/log/aiops-execd/audit.log`. The command preview is best-effort redacted and the full command is represented by `cmd_hash`. This is local troubleshooting evidence, not tamper-proof audit, because root commands can modify local files.
- Daemon operational events are emitted as JSON lines on stderr without command text.
- Job directories are cleaned according to `retention_days`, `max_jobs_retained`, and `max_total_job_bytes`.
- Failed authentication attempts are throttled per TCP peer by the `security.auth_failure_*` settings.

## Documentation

- Repository layout: [`docs/repo-layout.md`](docs/repo-layout.md)
- Testing policy: [`docs/testing.md`](docs/testing.md)
- Release and deployment: [`docs/deployment.md`](docs/deployment.md)

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
git tag v0.1.5
git push origin v0.1.5
```

The workflow uploads:

```text
vpsops-execd_linux_amd64.tar.gz
vpsops-execd_linux_arm64.tar.gz
checksums.txt
```

Install a tagged release binary on a VPS from a checked-out tag:

```bash
git clone --branch v0.1.5 --depth 1 https://github.com/shiyi-jiaqiu/vpsops.git
cd vpsops
sudo VERSION=v0.1.5 ./scripts/install-release.sh
```

`install-release.sh` requires an explicit `VERSION` tag; `latest` is intentionally rejected. Do not pipe a mutable remote installer directly into `sudo bash`. For full host bootstrap, pass an explicit release tag:

```bash
sudo ./scripts/bootstrap-release-host.sh \
  --domain exec.example.com \
  --path /hidden-aiops-path \
  --run-token "$AIOPS_RUN_TOKEN" \
  --root-token "$AIOPS_ROOT_TOKEN" \
  --proxy caddy \
  --config-path /etc/caddy/Caddyfile \
  --marker '# aiops-execd marker' \
  --version v0.1.5
```

The install script only installs the binary under `/usr/local/bin/aiops-execd` and the two helper copies under `/usr/local/libexec/`. You still need to create users, config, sudoers, and the systemd unit.

For existing configured hosts, use the local rollout script:

```bash
./scripts/deploy-release.sh --version v0.1.5 --hosts jp,la,sg,gcp
./scripts/deploy-release.sh --version v0.1.5 --verify-only
```

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

Error responses include a stable machine-readable `code` while preserving the human-readable `error` field:

```json
{"error":"executor is busy","code":"executor_busy","retry_after_sec":1}
```

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

Use `vpsops` as the AI/operator CLI. It reads `.env` from this repo root, hides the HTTPS/token details, and calls the existing `/v1/run` API. The default command output is compact stable agent JSON; successful commands put `ok`, `summary`, `host`, and `schema` first, omit default-empty diagnostics, and summarize large streams with previews to reduce agent tokens. Failures keep decision fields plus `state`, `exit_code`, `stderr` or `stderr_preview`, truncation flags, and `error`.

Output profiles:

- Default / `--output agent`: compact `aiops.cli.result.v2` for agents.
- `--view decision`: shortest decision envelope for agents that only need continue/stop state.
- `--view brief`: alias for the default compact agent view.
- `--view full`: alias for `--output agent-full`.
- `--view raw`: alias for `--raw`.
- `--output agent-full`: full agent envelope, including success diagnostics.
- `--json`: raw daemon result JSON.
- `--raw`: remote stdout/stderr bytes for scripts or direct human inspection.

```bash
cp .env.example .env
vpsops hosts
vpsops jp health
vpsops jp -- hostname
vpsops jp --view decision -- systemctl is-active caddy
vpsops jp --raw -- hostname
vpsops jp --user -- id -un
vpsops jp batch --cmd 'hostname' --cmd 'uptime' --cmd 'df -h /'
vpsops fleet --hosts jp,la,sg,gcp --parallel 2 -- hostname
vpsops fleet-plan --hosts jp,la --precheck 'sshd -t' --apply 'systemctl reload ssh' --postcheck 'systemctl is-active ssh'
vpsops la docker ps
vpsops sg service status aiops-execd
vpsops jp service restart aiops-execd
vpsops jp doctor
vpsops jp ops ssh-surface
vpsops jp job cancel <job_id>
vpsops gcp file read /etc/hostname
```

Configured host names in this workspace:

- `jp`
- `la`
- `sg`
- `gcp`

The command form stays intentionally close to SSH:

```bash
vpsops <host> -- <shell command>
```

For multiple independent inspection commands, prefer one batch job instead of several separate HTTP calls:

```bash
vpsops <host> batch \
  --cmd 'hostname' \
  --cmd 'uptime' \
  --cmd 'free -h' \
  --cmd 'df -h /'
```

`batch` runs commands sequentially inside one remote job. By default it continues after failed steps so diagnostics are not lost, but the final exit code is non-zero if any step failed. Use `--stop-on-error` for deployment/update sequences where later steps must not run after a failure. In default agent JSON mode, `batch` includes per-step status in `steps`.

`fleet` runs one command across multiple configured hosts and emits one `aiops.cli.fleet.v2` object with `summary`, `counts`, `failed_hosts`, optional `first_failure`, and per-host results. Use it for read-only fan-out checks; keep mutable fleet operations guarded with `--lock-key` and conservative `--parallel` values.

`fleet-plan` runs a gated serial plan across hosts: all `--precheck` commands, then `--apply` commands under a shared lock key, then `--postcheck` commands. It stops on the first failed step and emits one `aiops.cli.fleet_plan.v2` object with aggregate counts, skipped host count when applicable, and the first failed phase. Use it for small live changes where the order and failure behavior matter more than parallel speed.

`doctor` runs the daemon deployment self-check on a host through `vpsops`, including the sudo/helper fd 3 probe by default. `ops ssh-surface` is a read-only recipe for checking effective SSH ports, listeners, and UFW SSH rules; in agent mode it returns structured `facts` instead of forcing agents to parse `ss` and `ufw` text.

`job cancel <job_id>` requests cancellation for a queued or running job. The daemon marks the job as `canceled` and cancels its execution context when the runner is active.

`service restart aiops-execd` is special-cased because a direct restart kills the job currently carrying the command. The CLI schedules a delayed `systemd-run` restart; verify it with a new `vpsops <host> health` call after the delay.

By default the local CLI uses `AIOPS_DEFAULT_PRIVILEGE` from `.env`; this workspace currently uses root to match the VPS admin workflow. Add `--user` for the unprivileged execution path. The CLI follows async jobs, emits stable agent JSON, returns the remote `exit_code`, and retries short `executor_busy` responses. New installs default to `limits.concurrency=2`; use `--lock-key` for mutable operations that must not overlap and `--idempotency-key` for retry-safe requests.

Host configuration supports both the original single-host variables and future host-scoped variables:

```dotenv
AIOPS_DEFAULT_HOST=example
AIOPS_HOSTS=example
AIOPS_HOST_EXAMPLE_ALIASES=ex
AIOPS_DEFAULT_PRIVILEGE=root
AIOPS_OUTPUT=agent-json

AIOPS_HOST_EXAMPLE_BASE=https://example.com/hidden-aiops-path
AIOPS_HOST_EXAMPLE_RUN_TOKEN=...
AIOPS_HOST_EXAMPLE_ROOT_TOKEN=...
```

`AIOPS_OUTPUT` accepts `agent-json`, `agent-full`, `json`, or `raw`.

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
