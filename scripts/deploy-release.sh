#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  scripts/deploy-release.sh --version vX.Y.Z [--hosts jp,la,sg,gcp]
  scripts/deploy-release.sh --version vX.Y.Z --verify-only [--hosts jp,la,sg,gcp]

Deploys an existing GitHub release to configured VPS hosts through the local
vpsops CLI. GitHub Actions builds the release assets; this script performs the
trusted local rollout and verification using the operator's local .env tokens.

Options:
  --version VERSION      Required explicit release tag, for example v0.1.3.
  --hosts LIST          Comma-separated host aliases. Default: jp,la,sg,gcp.
  --repo OWNER/REPO     GitHub repo. Default: shiyi-jiaqiu/vpsops.
  --verify-only         Do not deploy; only verify current live state.
  --no-wait-release     Skip waiting for checksums.txt to be available.
  --poll-timeout SEC    Release asset wait timeout. Default: 180.
  -h, --help            Show this help.
EOF
}

die() {
  echo "deploy-release: $*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

split_hosts() {
  local raw="$1"
  local item
  HOSTS=()
  IFS=',' read -ra parts <<<"$raw"
  for item in "${parts[@]}"; do
    item="${item//[[:space:]]/}"
    [[ -n "$item" ]] && HOSTS+=("$item")
  done
  [[ ${#HOSTS[@]} -gt 0 ]] || die "host list is empty"
}

VERSION=""
REPO="${REPO:-shiyi-jiaqiu/vpsops}"
HOSTS=(jp la sg gcp)
VERIFY_ONLY=0
WAIT_RELEASE=1
POLL_TIMEOUT=180
POLL_INTERVAL=5
REMOTE_DIR="/var/lib/aiops-execd"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:?}"; shift 2 ;;
    --hosts) split_hosts "${2:?}"; shift 2 ;;
    --repo) REPO="${2:?}"; shift 2 ;;
    --verify-only) VERIFY_ONLY=1; shift ;;
    --no-wait-release) WAIT_RELEASE=0; shift ;;
    --poll-timeout) POLL_TIMEOUT="${2:?}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -n "$VERSION" ]] || die "--version is required"
[[ "$VERSION" =~ ^v[0-9A-Za-z._-]+$ ]] || die "--version must look like v0.1.3"
[[ "$VERSION" != "latest" ]] || die "latest is not allowed; use an explicit tag"
[[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "--repo must look like owner/repo"
[[ "$POLL_TIMEOUT" =~ ^[0-9]+$ ]] || die "--poll-timeout must be an integer"

need_cmd vpsops
need_cmd curl
need_cmd base64
need_cmd awk

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

release_url() {
  printf 'https://github.com/%s/releases/download/%s/%s' "$REPO" "$VERSION" "$1"
}

wait_for_release() {
  local checksums="$TMP_DIR/checksums.txt"
  local deadline=$((SECONDS + POLL_TIMEOUT))
  local url
  url="$(release_url checksums.txt)"

  while true; do
    if curl -fsSL -o "$checksums" "$url"; then
      grep -q ' vpsops-execd_linux_amd64.tar.gz$' "$checksums" ||
        die "release checksums.txt does not include linux/amd64 asset"
      grep -q ' vpsops-execd_linux_arm64.tar.gz$' "$checksums" ||
        die "release checksums.txt does not include linux/arm64 asset"
      echo "release assets available: $REPO $VERSION"
      return
    fi
    ((SECONDS < deadline)) || die "timed out waiting for release assets: $url"
    sleep "$POLL_INTERVAL"
  done
}

write_remote_script() {
  local path="$1"
  cat >"$path" <<EOF
#!/usr/bin/env bash
set -euo pipefail

version="$VERSION"
repo="$REPO"
remote_dir="$REMOTE_DIR"
log="\${remote_dir}/aiops-execd-upgrade-\${version}.log"
install -d -o root -g root -m 0700 "\$remote_dir"
exec > >(tee -a "\$log") 2>&1

rollback_needed=0
backup_dir=""

rollback() {
  local rc="\$1"
  if [[ "\$rollback_needed" == "1" && -n "\$backup_dir" && -d "\$backup_dir" ]]; then
    rollback_needed=0
    set +e
    echo "rollback: restoring previous aiops-execd files" >&2
    cp -a "\$backup_dir/aiops-execd" /usr/local/bin/aiops-execd
    cp -a "\$backup_dir/aiops-execd-run-child" /usr/local/libexec/aiops-execd-run-child
    cp -a "\$backup_dir/aiops-execd-root-child" /usr/local/libexec/aiops-execd-root-child
    cp -a "\$backup_dir/sudoers.aiops-execd" /etc/sudoers.d/aiops-execd
    chmod 0440 /etc/sudoers.d/aiops-execd
    visudo -cf /etc/sudoers.d/aiops-execd
    systemctl restart aiops-execd
    set -e
  fi
  exit "\$rc"
}

trap 'rc=\$?; if (( rc != 0 )); then rollback "\$rc"; fi' EXIT

case "\$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "unsupported architecture: \$(uname -m)" >&2; exit 2 ;;
esac

asset="vpsops-execd_linux_\${arch}.tar.gz"
base="https://github.com/\${repo}/releases/download/\${version}"
tmp="\$(mktemp -d "\${remote_dir}/upgrade.XXXXXX")"
cleanup() { rm -rf "\$tmp"; }
trap 'rc=\$?; cleanup; if (( rc != 0 )); then rollback "\$rc"; fi' EXIT

echo "start: \$(date -Is)"
echo "release: \${repo} \${version} \${asset}"
curl -fsSL --retry 3 --connect-timeout 10 -o "\$tmp/\$asset" "\$base/\$asset"
curl -fsSL --retry 3 --connect-timeout 10 -o "\$tmp/checksums.txt" "\$base/checksums.txt"
(
  cd "\$tmp"
  grep " \${asset}\$" checksums.txt | sha256sum -c -
  tar -xzf "\$asset"
)

backup_dir="\${remote_dir}/backup-\${version}-\$(date +%Y%m%d%H%M%S)"
install -d -o root -g root -m 0700 "\$backup_dir"
cp -a /usr/local/bin/aiops-execd "\$backup_dir/aiops-execd"
cp -a /usr/local/libexec/aiops-execd-run-child "\$backup_dir/aiops-execd-run-child"
cp -a /usr/local/libexec/aiops-execd-root-child "\$backup_dir/aiops-execd-root-child"
cp -a /etc/sudoers.d/aiops-execd "\$backup_dir/sudoers.aiops-execd"
rollback_needed=1

install -o root -g root -m 0755 "\$tmp/aiops-execd" /usr/local/bin/aiops-execd
install -o root -g root -m 0755 "\$tmp/aiops-execd" /usr/local/libexec/aiops-execd-run-child
install -o root -g root -m 0755 "\$tmp/aiops-execd" /usr/local/libexec/aiops-execd-root-child

read -r helper_run_sha _ < <(sha256sum /usr/local/libexec/aiops-execd-run-child)
read -r helper_root_sha _ < <(sha256sum /usr/local/libexec/aiops-execd-root-child)
cat >/etc/sudoers.d/aiops-execd <<SUDOERS
Defaults:aiopsd env_reset
Defaults:aiopsd secure_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
Defaults:aiopsd !setenv
Defaults:aiopsd closefrom_override
aiopsd ALL=(aiops-run) NOPASSWD: sha256:\${helper_run_sha} /usr/local/libexec/aiops-execd-run-child ""
aiopsd ALL=(root) NOPASSWD: sha256:\${helper_root_sha} /usr/local/libexec/aiops-execd-root-child ""
SUDOERS
chmod 0440 /etc/sudoers.d/aiops-execd
visudo -cf /etc/sudoers.d/aiops-execd

/usr/local/bin/aiops-execd -config /etc/aiops-execd/config.json -doctor
sudo -u aiopsd /usr/local/bin/aiops-execd -config /etc/aiops-execd/config.json -doctor -doctor-probe

systemctl restart aiops-execd
sleep 2
systemctl is-active aiops-execd
rollback_needed=0
sha256sum /usr/local/bin/aiops-execd /usr/local/libexec/aiops-execd-run-child /usr/local/libexec/aiops-execd-root-child
echo "done: \$(date -Is)"
EOF
}

unit_suffix() {
  printf '%s' "$VERSION" | tr -c 'A-Za-z0-9' '-'
}

start_deploy() {
  local host="$1"
  local remote_script="${REMOTE_DIR}/upgrade-${VERSION}.sh"
  local unit="aiops-execd-upgrade-$(unit_suffix)"
  local script="$TMP_DIR/remote-upgrade-${host}.sh"
  local encoded
  local remote_cmd

  write_remote_script "$script"
  encoded="$(base64 -w0 "$script")"
  remote_cmd="install -d -o root -g root -m 0700 ${REMOTE_DIR} && printf %s ${encoded} | base64 -d >${remote_script} && chmod 700 ${remote_script} && systemd-run --unit=${unit} --collect /bin/bash ${remote_script}"

  echo "==> ${host}: start deploy ${VERSION}"
  vpsops "$host" --timeout 20 --wait 10 -- "$remote_cmd"
}

verify_host() {
  local host="$1"
  local output
  local hash
  local log_path="${REMOTE_DIR}/aiops-execd-upgrade-${VERSION}.log"

  echo "==> ${host}: verify"
  output="$(vpsops "$host" batch --stop-on-error \
    --cmd "systemctl is-active aiops-execd" \
    --cmd "tail -n 80 ${log_path} 2>/dev/null || true" \
    --cmd "sha256sum /usr/local/bin/aiops-execd /usr/local/libexec/aiops-execd-run-child /usr/local/libexec/aiops-execd-root-child" \
    --cmd "sudo -u aiopsd /usr/local/bin/aiops-execd -config /etc/aiops-execd/config.json -doctor -doctor-probe" \
    --cmd "printf vpsops-ok")"
  printf '%s\n' "$output"
  hash="$(printf '%s\n' "$output" | awk '/\/usr\/local\/bin\/aiops-execd$/ {print $1; exit}')"
  [[ -n "$hash" ]] || die "${host}: could not parse aiops-execd hash"
  LAST_HASH="$hash"
}

if [[ "$WAIT_RELEASE" == "1" ]]; then
  wait_for_release
fi

EXPECTED_HASH=""
for host in "${HOSTS[@]}"; do
  if [[ "$VERIFY_ONLY" != "1" ]]; then
    start_deploy "$host"
    sleep 15
  fi
  LAST_HASH=""
  verify_host "$host"
  if [[ -z "$EXPECTED_HASH" ]]; then
    EXPECTED_HASH="$LAST_HASH"
  elif [[ "$LAST_HASH" != "$EXPECTED_HASH" ]]; then
    die "${host}: binary hash ${LAST_HASH} differs from expected ${EXPECTED_HASH}"
  fi
done

echo "deploy-release: all hosts verified with hash ${EXPECTED_HASH}"
