[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

# score-flyio

A [Score](https://score.dev) implementation for [Fly.io](https://fly.io). Describe your workload once in a vendor-neutral Score spec; `score-flyio` converts it into Fly Machines deployment plans — or classic `fly.toml` for single-container apps — and deploys it with resource provisioning, secret handling, and idempotent reconciliation.

Score workloads may carry the renderer-owned `metadata.fly` extension for
multi-container machine groups (colocation, independent scaling, and release
commands). Application-specific configuration belongs in the caller that
produces this extension. Workloads without it use the legacy single-container
`fly.toml` path. See the [`metadata.fly` reference](docs/metadata-fly.md) and
[migration notes](docs/migration.md).

## Quickstart

```sh
go install github.com/phoenixTW/score-flyio@v0.1.2
# or download a binary from https://github.com/phoenixTW/score-flyio/releases

# create a token from the Fly dashboard or API — no CLI required
export FLY_API_TOKEN=<fly-api-token>

score-flyio init --fly-app-prefix my-app-
score-flyio generate score.yaml --deploy
```

Pin a released semver tag — not a branch or `@latest` — so caller pipelines
stay reproducible. `FLY_API_BASE_URL` (v0.2.0+) optionally overrides the
Machines API endpoint for the whole machine deploy path — machine
reconciliation and secret creation — for local fake-API testing.

Legacy single-container flow: `generate` writes `<workload>.toml` + `.env`,
uploads secrets via the Machines API, and creates/deploys the app with the
`fly` CLI. Machine flow (multi-container /
`metadata.fly`): deploys — including runtime secret upload — use only the
Fly Machines API; `generate --deploy` drives it directly and never falls back
to an invalid TOML plan. Caller pipelines do not need `flyctl`/`fly` on the
deploy path.

### Machine deployment commands

| Command | Purpose |
| --- | --- |
| `score-flyio plan score.yaml` | offline diff with JSON output (`--dry-run`) |
| `score-flyio apply score.yaml` | deploy plan; rollback on failure |
| `score-flyio validate score.yaml` | validate the workload and plan |
| `score-flyio status score.yaml` | machines, checks, events, exits |
| `score-flyio logs score.yaml` | stream per-machine logs |
| `score-flyio scale score.yaml --group app --min 1 --max 3` | adjust group bounds (`--apply` to deploy) |
| `score-flyio suspend/resume score.yaml` | scale to zero and back |
| `score-flyio reconcile score.yaml` | re-apply desired state |
| `score-flyio destroy score.yaml --yes` | delete managed machines and app |

`logs` is the only command on the machine path that still uses the `fly` CLI
(streaming compatibility); every other machine-path command is API-only. The
legacy single-container flow still uses the `fly` CLI for app creation and
deploys.

Apply is idempotent (config-hash no-op re-runs), runs one-off release commands exactly once per change, waits for health checks, and rolls back partial failures.

## Feature support

**Supported:** single and multi-container workloads (machine path) · image or Dockerfile builds · `command`/`args` · variables with placeholders · cpu/memory resources · mounted files and Fly volumes · tcp/udp services with Fly Proxy handlers · liveness/readiness http probes as checks · resource provisioning (static, cmd, http) · secret variables and files.

**Not supported:** file mount modes · volume sub-paths and read-only mounts.

## Workload annotations

| Annotation | Value |
| --- | --- |
| `score-flyio.phoenixtw.github.com/service-<port>-handlers` | comma-separated [Fly Proxy handlers](https://fly.io/docs/reference/fly-proxy/#connection-handlers), e.g. `tls,http` |
| `score-flyio.phoenixtw.github.com/service-<port>-http-options` | JSON, e.g. `'{"idle_timeout": 60}'` |
| `score-flyio.phoenixtw.github.com/service-<port>-auto-stop` | `stop` (also enables auto-start) |
| `score-flyio.phoenixtw.github.com/service-<port>-min-running` | minimum running machines, e.g. `"1"` |
| `score-flyio.phoenixtw.github.com/service-<port>-concurrency` | JSON, e.g. `'{"type":"requests","hard_limit":25,"soft_limit":20}'` |

## Library packages

The deployment machinery under `pkg/` is domain-neutral and importable by any tool:

- `pkg/flymachines` — generated Fly Machines API client
- `pkg/flydeploy/machineconfig` — machine plan model, validation, Fly config conversion
- `pkg/flydeploy/planner` — deterministic diff between plans and live machines
- `pkg/flydeploy/deployer` — Machines API primitives
- `pkg/flydeploy/reconcile` — apply with rollback, readiness waits, release commands
- `pkg/state` — project state with schema versioning and file locking

The library owns only `flydeploy.group` / `flydeploy.config-hash` metadata keys;
everything else is caller-supplied. The `internal/` tree contains the generic
Score adapter and CLI.

## Resources and state

Provisioners (`score-flyio provisioners add ...`) support `static` JSON, `cmd` binaries, and `http` endpoints; a built-in Fly Postgres provisioner is included. Command and HTTP provisioners classify their outputs into `values` and `secrets`; secret-backed `${resources.*}` references resolve to runtime secrets that are uploaded to the Machines API per key and never appear in plans, artifacts, logs, or errors. Vendor-specific provisioners belong in separate companion projects outside this renderer. Resource state and provisioner config persist to `.score-flyio/state.yaml` — treat it like Terraform state: keep it per environment, backed up, and access-controlled. See the [Score docs](https://docs.score.dev/docs/) for resource semantics.

Sample Score specs live in [./samples](./samples), including a
[multi-container workload](./samples/multi_containers/score.yaml) with a
service, a colocated sidecar, and an independently scaled worker.

## Releases

Releases are cut with [GoReleaser](https://goreleaser.com) — the toolchain Fly.io uses for `flyctl` — driven by `v*` tags: cross-platform binaries (linux/darwin/windows, amd64/arm64), archives, and checksums publish to the GitHub release. CI runs build, test, and lint on every push and pull request.

## License

Apache 2.0 — see [LICENSE](LICENSE).
