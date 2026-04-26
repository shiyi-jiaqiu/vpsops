#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${OUT:-${ROOT}/dist}"
VERSION="${VERSION:-dev}"

rm -rf "${OUT}"
mkdir -p "${OUT}"

for arch in amd64 arm64; do
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' EXIT

  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
    go build -trimpath -ldflags="-s -w" \
    -o "${tmp}/aiops-execd" "${ROOT}/cmd/aiops-execd"

  mkdir -p "${tmp}/deploy"
  cp "${ROOT}/README.md" "${tmp}/README.md"
  cp "${ROOT}/deploy/config.example.json" "${tmp}/deploy/config.example.json"
  cp "${ROOT}/deploy/aiops-execd.service" "${tmp}/deploy/aiops-execd.service"
  cp "${ROOT}/deploy/sudoers.aiops-execd" "${tmp}/deploy/sudoers.aiops-execd"

  printf '%s\n' "${VERSION}" > "${tmp}/VERSION"
  tar -C "${tmp}" -czf "${OUT}/vpsops-execd_linux_${arch}.tar.gz" .
  rm -rf "${tmp}"
  trap - EXIT
done

(
  cd "${OUT}"
  sha256sum vpsops-execd_linux_*.tar.gz > checksums.txt
)
