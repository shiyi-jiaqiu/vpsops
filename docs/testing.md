# Testing

## Standard local check

Run this before pushing:

```bash
go test ./...
go vet ./...
python3 -m py_compile bin/aiops
python3 scripts/test-aiops-cli.py
bash -n scripts/*.sh bin/vpsops
./scripts/smoke-local.sh
```

## What each layer covers

- `go test ./...`: unit and package-level behavior, including auth, validation, job store, child execution, doctor checks, and HTTP handler edge cases.
- `go vet ./...`: common Go correctness checks.
- `python3 -m py_compile bin/aiops`: local CLI syntax check.
- `python3 scripts/test-aiops-cli.py`: local CLI behavior checks for host config, compact/default agent JSON output, full agent output, raw overrides, batch step reporting, fleet summaries, self-restart scheduling, control keys, and retry handling.
- `bash -n scripts/*.sh bin/vpsops`: shell syntax check for operator scripts and wrappers.
- `scripts/smoke-local.sh`: end-to-end local daemon test using a temporary server and fake sudo shim.

## Test layout policy

Keep Go unit tests beside production files as `*_test.go`. This is Go convention and allows focused tests without widening production APIs.

Use separate script or black-box tests when a test needs a full process, release artifact, or live `vpsops` host. Examples:

- Local daemon integration: `scripts/smoke-local.sh`
- Release deployment verification: `scripts/deploy-release.sh --version vX.Y.Z --verify-only`

## Release verification

Tag pushes run GitHub Actions:

```text
go test ./...
./scripts/smoke-local.sh
python3 -m py_compile bin/aiops
python3 scripts/test-aiops-cli.py
./scripts/package-release.sh
```

The release deployment script verifies live hosts after rollout by checking:

- `aiops-execd.service` is active.
- Installed daemon/helper hashes match across all target hosts.
- `sudo -u aiopsd aiops-execd -doctor -doctor-probe` passes.
- A real `vpsops` command returns successfully.
