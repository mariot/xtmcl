# AGENTS Guide

## Project Snapshot
- `xtmcl` (XTM Composer Launchpad): a standalone Go CLI (`module github.com/mariot/xtmcl`), Cobra-based like
  `mariot/gh-xtm-launchpad`. It isn't a gh extension: it never calls GitHub.
- It drives XTM Composer without OpenAEV/OpenCTI: `platform/` emulates the OpenAEV API the Composer polls,
  `cmd/` starts the platform and the Composer container and manages the connectors through a control API.

## Architecture and Data Flow
- `up` (`cmd/up.go`):
  1. Load or create the environment (`cmd/env.go`): manager ID, two tokens, RSA 4096 credentials key, settings.
  2. Find how containers reach this machine (`host-gateway`, `detectPlatformHost`) and pick a listen address
     that stays off the network (`listenAddress`).
  3. For Kubernetes, create the kind cluster if needed and get its internal kubeconfig.
  4. Restart the platform server in the background (`<binary> serve`, `cmd/serve.go`).
  5. Recreate the Composer container, copy its config (`config/production.yaml`), key and kubeconfig with
     `docker cp`, start it, and wait for its registration.
- The Composer polls `GET /api/xtm-composer/{manager}/connector-instances` and reports status, health and logs
  (`platform/server.go`). Field names must match `ConnectorInstances` in xtm-composer
  (`src/api/openaev/connector/mod.rs`).
- The CLI uses `/control/*` (`cmd/client.go`), authenticated by the control token.
- Secrets are encrypted with `platform.Encrypt` (version 2: RSA-OAEP SHA-256 + AES-256-GCM), the format
  `parse_aes_encrypted_value` decrypts in xtm-composer (`src/api/decrypt_value.rs`).
- The Composer sends log snapshots (last 100 lines with Docker, all lines with Kubernetes): `Store.AppendLogs`
  only appends what follows the overlap with the kept lines.

## Developer Workflows
```bash
go test ./...
./scripts/lint.sh
go build -o xtmcl . && ./scripts/e2e.sh                            # needs Docker
COMPOSER_IMAGE=xtm-composer:dev ./scripts/e2e.sh                   # against a local Composer build
XTMCL_HOME=$(mktemp -d) ./xtmcl up                                 # isolated environment
goreleaser check && goreleaser build --snapshot --clean            # release dry run
```
- CI (`.github/workflows/test.yml`): golangci-lint, `go vet` + `go test -race`, `scripts/e2e.sh`, and a GoReleaser
  snapshot build.
- Release is tag-driven: pushing `v*` runs `.github/workflows/release.yml` (GoReleaser, `.goreleaser.yaml`), which
  stamps `cmd.version` through ldflags and attests the archives.

## Project Conventions
- Command logic lives in `cmd/`; the platform (HTTP API, store, crypto) in `platform/`, without other layers.
- Commands return errors (`RunE`); `Execute` prints them. Command output goes through `outf`/`outln` (the
  `output` writer), which tests capture.
- External commands (docker, kind, kubectl) go through `runCommand`; tests replace it with `fakeRunner`
  (`cmd/cmd_test.go`). Waits go through `sleep`, also replaced in tests.
- Generated files reach containers with `docker cp`, not bind mounts (Docker Desktop and Colima don't share
  every host path).
- Never store or print a secret in clear: the store keeps ciphertexts, the control API only lists secret names.
- Keep `README.md` in sync with the flags and the command output.

## Files to Read First
- `platform/server.go` - the emulated OpenAEV API and the control API.
- `cmd/up.go` - environment setup, networking, Composer container.
- `cmd/cmd_test.go`, `platform/server_test.go` - expected behavior.
- `scripts/e2e.sh` - the end-to-end scenario.
