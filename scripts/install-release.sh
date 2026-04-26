#!/usr/bin/env bash
set -euo pipefail

REPO="${REPO:-shiyi-jiaqiu/vpsops}"
VERSION="${VERSION:-latest}"
PREFIX="${PREFIX:-/usr/local}"
LIBEXEC_DIR="${LIBEXEC_DIR:-/usr/local/libexec}"

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 2 ;;
esac

asset="vpsops-execd_linux_${arch}.tar.gz"
if [[ "${VERSION}" == "latest" ]]; then
  base="https://github.com/${REPO}/releases/latest/download"
else
  base="https://github.com/${REPO}/releases/download/${VERSION}"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt"
(
  cd "${tmp}"
  grep " ${asset}$" checksums.txt | sha256sum -c -
  tar -xzf "${asset}"
)

sudo_cmd=()
if [[ "${EUID}" -ne 0 ]]; then
  sudo_cmd=(sudo)
fi

"${sudo_cmd[@]}" mkdir -p "${PREFIX}/bin" "${LIBEXEC_DIR}"
"${sudo_cmd[@]}" install -o root -g root -m 0755 "${tmp}/aiops-execd" "${PREFIX}/bin/aiops-execd"
"${sudo_cmd[@]}" install -o root -g root -m 0755 "${tmp}/aiops-execd" "${LIBEXEC_DIR}/aiops-execd-run-child"
"${sudo_cmd[@]}" install -o root -g root -m 0755 "${tmp}/aiops-execd" "${LIBEXEC_DIR}/aiops-execd-root-child"

echo "installed aiops-execd for linux/${arch}"
echo "next: create /etc/aiops-execd/config.json, install sudoers, then enable deploy/aiops-execd.service"
