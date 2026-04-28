#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  bootstrap-release-host.sh \
    --domain DOMAIN \
    --path PATH \
    --run-token TOKEN \
    --root-token TOKEN \
    --proxy caddy|nginx \
    --config-path PATH \
    --marker STRING \
    --version vX.Y.Z

Installs the published aiops-execd release, writes host-specific config/sudoers,
patches the local reverse proxy with a hidden path, and validates the deployment.
EOF
}

domain=""
path_prefix=""
run_token=""
root_token=""
proxy_kind=""
config_path=""
marker=""
version=""
repo="${REPO:-shiyi-jiaqiu/vpsops}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain) domain="${2:?}"; shift 2 ;;
    --path) path_prefix="${2:?}"; shift 2 ;;
    --run-token) run_token="${2:?}"; shift 2 ;;
    --root-token) root_token="${2:?}"; shift 2 ;;
    --proxy) proxy_kind="${2:?}"; shift 2 ;;
    --config-path) config_path="${2:?}"; shift 2 ;;
    --marker) marker="${2:?}"; shift 2 ;;
    --version) version="${2:?}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

for required in domain path_prefix run_token root_token proxy_kind config_path marker; do
  if [[ -z "${!required}" ]]; then
    echo "missing required argument: ${required}" >&2
    usage >&2
    exit 2
  fi
done
if [[ -z "${version}" || "${version}" == "latest" ]]; then
  echo "--version must be an explicit release tag such as v0.1.0" >&2
  usage >&2
  exit 2
fi
if [[ "${version}" != v* ]]; then
  echo "--version must look like a release tag, for example v0.1.0" >&2
  usage >&2
  exit 2
fi

if [[ "${EUID}" -ne 0 ]]; then
  echo "run as root" >&2
  exit 2
fi

export DEBIAN_FRONTEND=noninteractive

mkdir -p /usr/local/bin /usr/local/libexec

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 2 ;;
esac
asset="vpsops-execd_linux_${arch}.tar.gz"
release_base="https://github.com/${repo}/releases/download/${version}"
curl -fsSL -o "${tmp}/${asset}" "${release_base}/${asset}"
curl -fsSL -o "${tmp}/checksums.txt" "${release_base}/checksums.txt"
(
  cd "${tmp}"
  grep -F " ${asset}" checksums.txt | sha256sum -c -
  tar -xzf "${asset}"
)
install -o root -g root -m 0755 "${tmp}/aiops-execd" /usr/local/bin/aiops-execd
install -o root -g root -m 0755 "${tmp}/aiops-execd" /usr/local/libexec/aiops-execd-run-child
install -o root -g root -m 0755 "${tmp}/aiops-execd" /usr/local/libexec/aiops-execd-root-child

id -u aiopsd >/dev/null 2>&1 || useradd --system --home /var/lib/aiops-execd --shell /usr/sbin/nologin aiopsd
id -u aiops-run >/dev/null 2>&1 || useradd --system --home /var/lib/aiops-run --shell /usr/sbin/nologin aiops-run
mkdir -p /etc/aiops-execd /var/lib/aiops-execd/jobs /var/log/aiops-execd /usr/local/libexec /var/lib/aiops-run
chown -R aiopsd:aiopsd /var/lib/aiops-execd /var/log/aiops-execd
chown aiops-run:aiops-run /var/lib/aiops-run
chmod 700 /var/lib/aiops-execd /var/log/aiops-execd /var/lib/aiops-run

run_sha="$(printf '%s' "${run_token}" | sha256sum | awk '{print $1}')"
root_sha="$(printf '%s' "${root_token}" | sha256sum | awk '{print $1}')"
helper_run_sha="$(sha256sum /usr/local/libexec/aiops-execd-run-child | awk '{print $1}')"
helper_root_sha="$(sha256sum /usr/local/libexec/aiops-execd-root-child | awk '{print $1}')"

cat >/etc/aiops-execd/config.json <<EOF
{
  "listen": "127.0.0.1:7843",
  "tokens": [
    {
      "id": "ai-run",
      "sha256": "${run_sha}",
      "allow_root": false
    },
    {
      "id": "ai-root",
      "sha256": "${root_sha}",
      "allow_root": true
    }
  ],
  "limits": {
    "default_timeout_sec": 30,
    "max_timeout_sec": 300,
    "default_wait_sec": 25,
    "max_wait_sec": 60,
    "default_kill_grace_sec": 3,
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
    "concurrency": 1,
    "max_jobs_retained": 1000
  },
  "security": {
    "auth_failure_limit": 10,
    "auth_failure_window_sec": 60
  },
  "execution": {
    "shell_path": "/bin/bash",
    "shell_args": ["--noprofile", "--norc", "-o", "pipefail", "-c"],
    "fixed_path": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "allow_any_cwd_for_root": true,
    "allowed_cwd_prefixes_for_user": ["/opt", "/srv", "/var/www", "/tmp", "/var/log"],
    "run_user": "aiops-run",
    "root_home": "/root",
    "run_home": "/var/lib/aiops-run"
  },
  "env": {
    "allow": ["HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"],
    "deny": ["LD_PRELOAD", "LD_LIBRARY_PATH", "BASH_ENV", "ENV", "GIT_SSH_COMMAND", "SSH_AUTH_SOCK", "PYTHONPATH", "NODE_OPTIONS"]
  },
  "storage": {
    "job_dir": "/var/lib/aiops-execd/jobs",
    "log_dir": "/var/log/aiops-execd",
    "retention_days": 7,
    "max_total_job_bytes": 104857600
  },
  "helpers": {
    "sudo_path": "/usr/bin/sudo",
    "run_child_path": "/usr/local/libexec/aiops-execd-run-child",
    "root_child_path": "/usr/local/libexec/aiops-execd-root-child"
  }
}
EOF
chown root:aiopsd /etc/aiops-execd/config.json
chmod 0640 /etc/aiops-execd/config.json

cat >/etc/sudoers.d/aiops-execd <<EOF
Defaults:aiopsd env_reset
Defaults:aiopsd secure_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
Defaults:aiopsd !setenv
Defaults:aiopsd closefrom_override
aiopsd ALL=(aiops-run) NOPASSWD: sha256:${helper_run_sha} /usr/local/libexec/aiops-execd-run-child ""
aiopsd ALL=(root) NOPASSWD: sha256:${helper_root_sha} /usr/local/libexec/aiops-execd-root-child ""
EOF
chmod 0440 /etc/sudoers.d/aiops-execd
visudo -cf /etc/sudoers.d/aiops-execd

install -o root -g root -m 0644 /dev/stdin /etc/systemd/system/aiops-execd.service <<'EOF'
[Unit]
Description=AIOps Exec API
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=aiopsd
Group=aiopsd
ExecStart=/usr/local/bin/aiops-execd -config /etc/aiops-execd/config.json
Restart=always
RestartSec=2s

CPUQuota=30%
MemoryHigh=256M
TasksMax=128
Nice=10
IOSchedulingClass=idle

PrivateTmp=true
NoNewPrivileges=false

[Install]
WantedBy=multi-user.target
EOF

backup_path="${config_path}.bak-aiops-$(date +%Y%m%d-%H%M%S)"
cp "${config_path}" "${backup_path}"

case "${proxy_kind}" in
  caddy)
    python3 - "$config_path" "$path_prefix" "$marker" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
prefix = sys.argv[2]
marker = sys.argv[3]
text = path.read_text()
lines = text.splitlines(keepends=True)
for idx, line in enumerate(lines):
    if line.lstrip() == marker + "\n" or line.lstrip() == marker:
        indent = line[: len(line) - len(line.lstrip())]
        block = (
            f"{indent}handle_path {prefix}* {{\n"
            f"{indent}\treverse_proxy 127.0.0.1:7843\n"
            f"{indent}}}\n\n"
        )
        if "".join(lines[max(0, idx - 3): idx + 3]).find(f"handle_path {prefix}*") != -1:
            sys.exit(0)
        lines.insert(idx, block)
        break
else:
    raise SystemExit(f"marker not found: {marker}")
text = "".join(lines)
path.write_text(text)
PY
    caddy validate --config /etc/caddy/Caddyfile >/dev/null
    systemctl reload caddy
    ;;
  nginx)
    python3 - "$config_path" "$path_prefix" "$marker" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
prefix = sys.argv[2]
marker = sys.argv[3]
text = path.read_text()
lines = text.splitlines(keepends=True)
matches = [idx for idx, line in enumerate(lines) if line.lstrip() == marker + "\n" or line.lstrip() == marker]
if not matches:
    raise SystemExit(f"marker not found: {marker}")
idx = matches[-1]
line = lines[idx]
indent = line[: len(line) - len(line.lstrip())]
block = (
    f'{indent}location ^~ {prefix}/ {{\n'
    f'{indent}    proxy_pass http://127.0.0.1:7843/;\n'
    f'{indent}    proxy_http_version 1.1;\n'
    f'{indent}    proxy_set_header Host $host;\n'
    f'{indent}    proxy_set_header X-Real-IP $remote_addr;\n'
    f'{indent}    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n'
    f'{indent}    proxy_set_header X-Forwarded-Proto https;\n'
    f'{indent}}}\n\n'
)
if "".join(lines[max(0, idx - 3): idx + 10]).find(f"location ^~ {prefix}/") != -1:
    sys.exit(0)
lines.insert(idx, block)
text = "".join(lines)
path.write_text(text)
PY
    nginx -t >/dev/null
    systemctl reload nginx
    ;;
  *)
    echo "unsupported proxy kind: ${proxy_kind}" >&2
    exit 2
    ;;
esac

cat >/root/aiops-execd-https.env <<EOF
AIOPS_HTTPS_BASE=https://${domain}${path_prefix}
AIOPS_HTTPS_PATH=${path_prefix}
AIOPS_PROXY_BACKUP=${backup_path}
EOF
chmod 0600 /root/aiops-execd-https.env

cat >/root/aiops-execd-test-tokens.env <<EOF
AIOPS_RUN_TOKEN=${run_token}
AIOPS_ROOT_TOKEN=${root_token}
EOF
chmod 0600 /root/aiops-execd-test-tokens.env

systemctl daemon-reload
systemctl enable --now aiops-execd

/usr/local/bin/aiops-execd -config /etc/aiops-execd/config.json -doctor >/tmp/aiops-doctor.txt
sudo -u aiopsd /usr/local/bin/aiops-execd -config /etc/aiops-execd/config.json -doctor -doctor-probe >/tmp/aiops-doctor-probe.txt

echo "bootstrap complete"
echo "base=https://${domain}${path_prefix}"
echo "backup=${backup_path}"
