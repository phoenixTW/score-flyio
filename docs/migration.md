# Migrating callers to score-flyio

## Pin a release

Install or reference a released version tag — never a branch, `@latest`,
`@master`, or a commit hash:

```sh
go install github.com/phoenixTW/score-flyio@v0.1.2
```

Multi-container support (the Fly Machines API path) shipped in `v0.1.2`.
Releases are cut automatically from `main` once CI passes; every release
publishes cross-platform binaries and checksums.

## Single-container workloads: no change needed

The legacy flow keeps working: `generate` writes `fly_<workload>.toml`,
`--secrets-file` exports runtime secrets, and `generate --deploy` shells out
to the `fly` CLI. Workloads without `metadata.fly` stay on this path.

## Multi-container workloads

A workload with more than one container can no longer be deployed through the
TOML path — `generate` fails with a clear error instead of producing an
invalid manifest. Migrate in three steps:

1. Declare the generic `metadata.fly` extension on the workload describing
   process groups, colocation, scaling, services, and checks. See the
   [reference](metadata-fly.md) and the [multi-container sample](../samples/multi_containers/score.yaml).
2. Pin every container image to a `sha256` digest — mutable tags are rejected
   before any deploy step.
3. Deploy through the Machines API path:

```sh
score-flyio init --fly-app-prefix my-app-
score-flyio validate score.yaml
score-flyio plan score.yaml --dry-run
score-flyio apply score.yaml
```

`generate --deploy` also routes `metadata.fly` workloads to the Machines API
path. No hand-authored or generated `fly.toml` is involved.

## CLI mapping

| Legacy | Machines path |
| --- | --- |
| `generate` (TOML output) | `plan --dry-run` / `validate` (JSON plan) |
| `generate --deploy` (calls `fly` CLI) | `apply` / `generate --deploy` (Machines API) |
| — | `status`, `logs`, `scale`, `suspend`, `resume`, `reconcile`, `destroy` |

## Behavior changes to expect

- Apply is idempotent: re-running an unchanged plan is a no-op (per-group
  config hash), and failed health checks stop the rollout and roll back.
  On rollback, machines created during the failed apply are deleted; the Fly
  app itself is retained (empty and safe to re-apply or `destroy`).
- One-off release commands (`metadata.fly.release_command`) run exactly once
  per plan change, tracked in local state.
- Runtime secrets never appear in plans, logs, or errors; exact-plan applies
  require a `0600` secrets file matching the plan's required secret names.
- Local state lives in `.score-flyio/state.yaml` per environment — treat it
  like Terraform state (back it up, keep it per environment).

## Offline and CI verification

Tests and CI pipelines can exercise the full CLI against a local fake Machines
API with zero cloud spend by pointing the client at any HTTP base URL:

```sh
export FLY_API_TOKEN=test-token
export FLY_API_BASE_URL=http://localhost:9999/v1
```

`FLY_API_BASE_URL` (v0.2.0+) is optional; unset it for the public Fly API.
