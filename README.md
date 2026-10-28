# xtmcl — XTM Composer Launchpad

[![test](https://github.com/mariot/xtmcl/actions/workflows/test.yml/badge.svg)](https://github.com/mariot/xtmcl/actions/workflows/test.yml)
[![release](https://github.com/mariot/xtmcl/actions/workflows/release.yml/badge.svg)](https://github.com/mariot/xtmcl/actions/workflows/release.yml)

`xtmcl` drives [XTM Composer](https://github.com/FiligranHQ/xtm-composer) from the command line, **without OpenAEV or
OpenCTI**. You deploy, start, stop and remove connectors, collectors and injectors on Docker or Kubernetes. Composer
handles them exactly as it would for a real platform.

```console
$ xtmcl up
$ xtmcl deploy nginx:alpine --name "Web Demo" --env GREETING=hello --secret API_TOKEN=s3cr3t
Connector Web Demo declared (nginx:alpine, deployed as web-demo)
Waiting for the Composer to report Web Demo started...
Connector Web Demo started
$ xtmcl ps
NAME      IMAGE         REQUESTED  STATUS   RESTARTS  STARTED AT                      DEPLOYED AS
Web Demo  nginx:alpine  start      started  0         2026-10-28T13:55:55.885336345Z  web-demo
```

`REQUESTED` is what you asked for. `STATUS`, `RESTARTS` and `STARTED AT` are what Composer reports.

Use it to:

- **test a Composer build or a PR**: point `xtmcl up --image` at your local image and run the whole lifecycle;
- **reproduce a Composer bug** on a laptop, without installing a platform;
- **run a connector the way the integration manager deploys it**, with the same settings, secrets and orchestrator,
  on Docker or on a local [kind](https://kind.sigs.k8s.io/) cluster.

## Contents

- [How it works](#how-it-works)
- [Install](#install)
- [Quick start](#quick-start)
- [Recipes](#recipes)
- [Command reference](#command-reference)
- [State, networking and security](#state-networking-and-security)
- [Troubleshooting](#troubleshooting)
- [Limitations](#limitations)
- [Development](#development)

## How it works

```
xtmcl ──control API──▶ local platform ◀──OpenAEV API── XTM Composer ──▶ Docker / Kubernetes
                       (emulates the    (container)                        │
                        OpenAEV API) ◀───────────── OPENAEV_URL ──────────┘ deployed connectors
                             │
                             └──▶ --upstream (optional): Mimikyu or a real OpenAEV
```

1. `xtmcl up` starts a **local platform** in the background. It emulates the 7 OpenAEV endpoints Composer calls:
   version, registration, ping, connector instances, status, health and logs. `up` then starts **Composer** in a
   container, registered on that platform.
2. `xtmcl deploy` declares a connector on the platform. On its next cycle, every 5 seconds, Composer deploys it,
   reports its status, health and logs, and redeploys it when its image or settings change.
3. `--secret` settings are encrypted the way OpenAEV encrypts them: RSA-OAEP plus AES-256-GCM, with Composer's
   credentials key. Composer decrypts them before deploying, so the connector receives the clear value.
4. Composer gives every connector `OPENAEV_URL`, which points at the local platform. With `up --upstream`, the
   connectors' own API calls (registration, uploads…) are forwarded to another platform, so a deployed collector
   really works. That platform can be [Mimikyu](https://github.com/mariot/Mimikyu), a fake OpenAEV, or a real
   OpenAEV.

## Install

### Release binary

Prebuilt binaries for Linux, macOS and Windows (amd64 and arm64) are attached to each
[release](https://github.com/mariot/xtmcl/releases):

```bash
VERSION=$(curl -fsSL https://api.github.com/repos/mariot/xtmcl/releases/latest | sed -n 's/.*"tag_name": "\(.*\)".*/\1/p')
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSL "https://github.com/mariot/xtmcl/releases/download/${VERSION}/xtmcl_${VERSION#v}_${OS}_${ARCH}.tar.gz" | tar -xz xtmcl
sudo mv xtmcl /usr/local/bin/
xtmcl --version
```

Each release has a `checksums.txt` and a build provenance attestation, which you can check with
`gh attestation verify <archive> --repo mariot/xtmcl`.

### With Go

```bash
go install github.com/mariot/xtmcl@latest
```

### From source

```bash
git clone https://github.com/mariot/xtmcl && cd xtmcl
go build -o xtmcl . && sudo mv xtmcl /usr/local/bin/
```

### Requirements

- Docker: Docker Desktop, Colima, or Docker Engine on Linux.
- For Kubernetes: [kind](https://kind.sigs.k8s.io/) and kubectl, or any cluster whose API server a container can reach.
- Go 1.26 or later, only to build from source.

## Quick start

```bash
# 1) Start the local platform and Composer, in Docker mode
xtmcl up

# 2) Declare a connector: Composer pulls, deploys and starts it
xtmcl deploy nginx:alpine --name "Web Demo" --env GREETING=hello --secret API_TOKEN=s3cr3t

# 3) Look at it
xtmcl ps
xtmcl logs "Web Demo" -f

# 4) Drive it
xtmcl stop "Web Demo"
xtmcl start "Web Demo"
xtmcl deploy nginx:alpine --name "Web Demo" --env GREETING=bonjour   # changed settings: Composer redeploys
xtmcl rm "Web Demo"

# 5) Stop everything. The declared connectors are kept and come back on the next up.
xtmcl down
```

`deploy` waits until Composer reports the connector as started. Use `--wait=false` to return at once.

## Recipes

### Test a Composer build

```bash
# in an xtm-composer checkout
docker build -t xtm-composer:dev .
xtmcl up --image xtm-composer:dev
```

The Composer container is created with `docker create`, so a local image works without a registry. To run the
whole lifecycle against it, from an `xtmcl` checkout:

```bash
COMPOSER_IMAGE=xtm-composer:dev ./scripts/e2e.sh
```

To go back to the released Composer, run `xtmcl up --image filigran/xtm-composer:latest --pull`.

### Run a real collector against Mimikyu

```bash
# in a Mimikyu checkout: Mimikyu listens on localhost:5001
docker compose up -d

xtmcl up --upstream http://localhost:5001
xtmcl deploy openaev/collector-mitre-attack:rolling --name "MITRE ATT&CK" \
  --env COLLECTOR_ID=$(uuidgen | tr '[:upper:]' '[:lower:]') --env COLLECTOR_LOG_LEVEL=info \
  --secret OPENAEV_TOKEN=any-token
xtmcl logs "MITRE ATT&CK" -f
```

Through the local platform, the collector registers in Mimikyu and pushes its kill chain phases and attack patterns
there. Use `--upstream https://openaev.example.com` to send them to a real OpenAEV, with a real token.

### Kubernetes

```bash
xtmcl up --orchestrator kubernetes                                   # creates the kind cluster xtm-composer if needed
xtmcl up --orchestrator kubernetes --kind-cluster other              # another kind cluster
xtmcl up --orchestrator kubernetes --kubeconfig ~/.kube/lab.yaml     # any other cluster
kubectl get deployments -l opencti-manager                           # what Composer deployed
```

With kind, the Composer container joins the `kind` Docker network and deploys to the `default` namespace. Composer
deploys with `imagePullPolicy: IfNotPresent`, so a locally built image works once it's loaded into the cluster:

```bash
kind load docker-image my-collector:dev --name xtm-composer
xtmcl deploy my-collector:dev --name "My collector"
```

### Settings from a file, secrets from the environment

```bash
xtmcl deploy openaev/collector-x:rolling --env-file collector.env --secret OPENAEV_TOKEN
```

`--env-file` reads `KEY=VALUE` lines and skips blank lines and `#` comments. `--secret KEY` with no value reads
`KEY` from your environment, which keeps it out of your shell history.

### Scripting

```bash
xtmcl ps --json | jq -r '.[] | "\(.name) \(.current_status)"'
```

## Command reference

| Command | Description |
|---|---|
| `xtmcl up` | Start the local platform and Composer, or restart them with new settings. Settings are remembered for the next `up`. |
| `xtmcl deploy <image>` | Declare a connector, or update the one with the same name. Composer redeploys it when its image or settings change. |
| `xtmcl ps` (`ls`) | List the connectors with their requested and reported status, restarts and start time. |
| `xtmcl logs <name>` | Show the connector logs reported by Composer. |
| `xtmcl start <name>` / `stop <name>` | Ask Composer to start or stop a connector. |
| `xtmcl rm <name>` | Remove a connector. Composer removes its deployment. |
| `xtmcl status` | Show the local platform, the Composer container, its registration and its last ping. |
| `xtmcl composer-logs` | Show the logs of Composer itself. |
| `xtmcl down` | Stop Composer and the platform, and remove the deployments. |

Run `xtmcl <command> --help` for the details of each command.

### `up` flags

| Flag | Default | Description |
|---|---|---|
| `--image` | `filigran/xtm-composer:latest` | Composer image. |
| `--pull` | | Pull the Composer image before starting it. |
| `--orchestrator` | `docker` | Where Composer deploys connectors: `docker` or `kubernetes`. |
| `--kind-cluster` | `xtm-composer` | kind cluster for Kubernetes, created if missing. |
| `--kubeconfig` | | Kubeconfig of another cluster, used instead of kind. A container must be able to reach its API server. |
| `--upstream` | | Platform that receives the connectors' own API calls, such as Mimikyu. `none` unsets it. |
| `--network` | | Docker network to attach the connectors to. `none` unsets it. |
| `--port` | `47380` | Port of the local platform. |
| `--listen` | detected | Address the local platform listens on. |
| `--platform-host` | detected | Address containers use to reach this machine. |
| `--timeout` | `1m30s` | How long to wait for Composer to register. |

### `deploy` flags

| Flag | Description |
|---|---|
| `--name` | Connector name. The default comes from the image name. |
| `-e`, `--env KEY=VALUE` | A setting. Repeatable. |
| `--env-file FILE` | A file of `KEY=VALUE` settings. Repeatable. |
| `--secret KEY=VALUE` or `--secret KEY` | A secret setting, sent encrypted. `KEY` alone reads the value from your environment. Repeatable. |
| `--stopped` | Declare the connector without starting it. |
| `--wait` | Wait until Composer reports the connector as started. On by default. |
| `--timeout` | How long to wait. The default is `3m`. |

### Other flags

- `ps --json`: print JSON.
- `logs -f`, `composer-logs -f`: keep printing new logs.
- `down --keep-connectors`: leave the deployments running.
- `down --purge`: also forget the declared connectors, the credentials key and the settings.
- `down --delete-cluster`: also delete the kind cluster.

## State, networking and security

`xtmcl` keeps its state in `$XTMCL_HOME`. When that isn't set, it uses `$XDG_STATE_HOME/xtmcl`, then
`~/.local/state/xtmcl`. The directory has mode 0700 and holds:

| File | Content |
|---|---|
| `env.json` | Settings, manager ID and API tokens. |
| `key.pem` | Composer's RSA 4096 credentials key. |
| `state.json` | Declared connectors. Secrets stay encrypted. |
| `server.log` | Log of the local platform. |

Set `XTMCL_HOME` to keep several independent environments, for example `XTMCL_HOME=$(mktemp -d) xtmcl up` for a
throwaway one. Two environments can't run at the same time unless their `--port` values differ.

The containers xtmcl creates are named and labelled so they're easy to find:

- the Composer container is `xtmcl-<manager id>`, labelled `xtmcl.manager`;
- connectors carry Composer's own labels: `opencti-manager`, `opencti-connector-id`, `opencti-platform`.

On security:

- The control API (used by `xtmcl`) and the Composer API each require their own random bearer token.
- The local platform never listens on every interface:
  - with Docker Desktop and Colima, it listens on `127.0.0.1`, and containers reach it through
    `host.docker.internal`;
  - with Docker Engine on Linux, it listens only on the Docker bridge address.
- Secrets are never stored or printed in clear: `ps` and the control API only show their names.

## Troubleshooting

**Composer doesn't register.** Run `xtmcl status` and `xtmcl composer-logs`. A wrong URL or a network problem shows
up there as a connection error to the platform. If containers can't reach this machine at the detected address,
set it with `--platform-host`.

**`up` says the platform server didn't start.** Read the `server.log` file named in the error. If it says
`address already in use`, another environment or program is using port `47380`. Run `xtmcl down` in that
environment, or start this one with `--port`.

**A connector stays `stopped` or keeps restarting.** Run `xtmcl logs <name>` for its own logs and `xtmcl ps` for its
restart count. The deployment itself is a normal container or Deployment, so `docker logs` and `kubectl describe`
work as usual.

**A local connector image isn't found in Docker mode.** Composer always pulls images in Docker mode, so the image
must be in a registry. Push it to a registry, or use the Kubernetes mode with `kind load docker-image`.

**`kind load docker-image` fails with a "content digest not found" error.** That happens with a multi-platform image
pulled from a registry and re-tagged. Build the image locally instead, or pull it with `--platform` set to your
architecture.

## Limitations

- Only the OpenAEV flavour of the Composer API is emulated. The OpenCTI flavour (GraphQL) isn't, yet.
- In Docker mode, Composer always pulls the connector image. See [Troubleshooting](#troubleshooting).
- Connector names must give distinct deployment names, as Composer requires. A deployment name is the connector
  name lowercased, with every non-alphanumeric character replaced by `-`.

## Development

```bash
go test ./...                                   # unit tests
./scripts/lint.sh                               # golangci-lint
go build -o xtmcl . && ./scripts/e2e.sh         # end-to-end, with a real Composer (needs Docker)
```

The end-to-end script takes `BIN` (the binary to test) and `COMPOSER_IMAGE` (the Composer image) from the
environment.

The project layout:

- `main.go`: the entrypoint.
- `cmd/`: the Cobra commands.
  - `up.go`: environment setup, networking and the Composer container.
  - `manage.go`: `down`, `status` and `composer-logs`.
  - `connectors.go`: the connector commands.
  - `serve.go`: the background platform server.
  - `client.go`: the control API client.
- `platform/`: the emulated OpenAEV API, its store and the secret encryption.
- `scripts/e2e.sh`: the end-to-end scenario.

### CI and releases

On every pull request and push to `main`, [`test.yml`](.github/workflows/test.yml) runs:

- golangci-lint;
- `go vet` and `go test -race`;
- the end-to-end test against the released Composer, on the runner's native Docker;
- a GoReleaser snapshot build of every release target.

To publish a release, push a `v*` tag:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

[`release.yml`](.github/workflows/release.yml) then runs the tests and builds the binaries with
[GoReleaser](https://goreleaser.com). It publishes them with checksums and a changelog, and attests their build
provenance.

## Related projects

- [XTM Composer](https://github.com/FiligranHQ/xtm-composer): the integration manager this tool drives.
- [gh-xtm-launchpad](https://github.com/mariot/gh-xtm-launchpad): build and run collectors and connectors locally.
- [Mimikyu](https://github.com/mariot/Mimikyu): a fake OpenAEV platform for collectors and injectors.
