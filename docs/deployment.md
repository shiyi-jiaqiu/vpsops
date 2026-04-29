# Deployment

## Model

GitHub Actions owns build and release packaging. Local `vpsops` owns live rollout.

This avoids storing root-capable VPS operation tokens in GitHub secrets. If a future workflow deploys directly from GitHub Actions, it must use tightly scoped environment protection and dedicated deploy-only tokens.

## Release

Create and push an explicit tag:

```bash
git tag v0.1.5
git push origin main v0.1.5
```

The `test-and-release` workflow publishes:

```text
vpsops-execd_linux_amd64.tar.gz
vpsops-execd_linux_arm64.tar.gz
checksums.txt
```

## Deploy To VPS Hosts

Deploy a published release from the local control machine:

```bash
./scripts/deploy-release.sh --version v0.1.5 --hosts jp,la,sg,gcp
```

Verify live hosts without changing binaries:

```bash
./scripts/deploy-release.sh --version v0.1.5 --verify-only
```

The deploy script performs a serial rollout:

- Waits for release `checksums.txt`.
- Starts one host update at a time through `vpsops`.
- Downloads the matching release asset on the target host.
- Verifies the release checksum before extraction.
- Keeps daemon state and log directories owned by `aiopsd`.
- Stores transient upgrade scripts, logs, and backups in a root-owned work directory.
- Backs up current daemon/helper/sudoers files.
- Installs the daemon and both helper paths.
- Rewrites sudoers with current helper SHA256 digests.
- Runs static doctor and sudo/helper probe.
- Restarts `aiops-execd`.
- Verifies service state, helper probe, a real `vpsops` command, and cross-host binary hash consistency.

## Rollback Behavior

If the remote install fails after current files are backed up, the remote upgrade script restores:

- `/usr/local/bin/aiops-execd`
- `/usr/local/libexec/aiops-execd-run-child`
- `/usr/local/libexec/aiops-execd-root-child`
- `/etc/sudoers.d/aiops-execd`

It then validates sudoers and restarts `aiops-execd`.

## Bootstrap

Use `scripts/bootstrap-release-host.sh` only for initial host setup or rebuilds. It requires an explicit tag, validates the proxy patch inputs, and downloads verified release assets directly; it does not pipe mutable remote scripts into `sudo bash`.

## Manual Install

For a single host where `vpsops` is not yet available, install a tagged release from a checked-out tag:

```bash
git clone --branch v0.1.5 --depth 1 https://github.com/shiyi-jiaqiu/vpsops.git
cd vpsops
sudo VERSION=v0.1.5 ./scripts/install-release.sh
```

`VERSION=latest` is rejected by all install/bootstrap/deploy entrypoints.
