# Repository Layout

`vpsops` is intentionally small. Keep the operational surface simple and keep
privileged code easy to audit.

## Top-level directories

- `.github/workflows/`: CI and release automation. This layer builds and publishes release assets; it does not hold live VPS credentials.
- `bin/`: local operator CLI entrypoints. `bin/vpsops` is the human-facing wrapper around `bin/aiops`.
- `cmd/aiops-execd/`: Go `main` package for the remote exec daemon.
- `internal/execd/`: daemon implementation. This is private Go code and should not be imported by external modules.
- `deploy/`: target-host templates for config, systemd, Caddy, and sudoers.
- `scripts/`: local operator automation, release packaging, bootstrap, smoke tests, and release deployment.
- `docs/`: durable project documentation.

## Go package structure

Current package boundary:

- `cmd/aiops-execd`: process startup, flags, signal handling.
- `internal/execd`: config, auth, HTTP handlers, job store, child execution, doctor checks, and validation.

The single `internal/execd` package is acceptable while the daemon remains small. Split it only when there is a clear dependency boundary, for example:

- `internal/execd/server`: HTTP routes and request lifecycle.
- `internal/execd/runner`: sudo/helper process execution.
- `internal/execd/store`: job persistence and cleanup.
- `internal/execd/config`: schema/defaults/validation.

Avoid premature package splitting if it forces exported APIs that are only used internally.

## Test placement

Go unit tests live beside the source files as `*_test.go`. That is the standard Go layout and keeps white-box tests close to package internals without exporting production-only symbols.

Use separate top-level test locations only for black-box or end-to-end checks that should not access package internals. The current local integration check is `scripts/smoke-local.sh`.

## Operational rule

Do not add another control surface unless it removes an existing one. The intended chain remains:

```text
vpsops CLI -> HTTPS /v1/run -> SSH rescue/bootstrap
```
