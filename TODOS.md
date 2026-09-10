# score-flyio custom deployment TODOs

This is the implementation backlog for replacing stock `score-flyio` with the
Progresify deployment implementation. The target is a Score-driven Fly Machines
deployment; `fly.toml` is retained only as an optional legacy compatibility path.

## P0 — deployment blocker

- [x] Replace the single-container guard in `internal/convert/convert.go` with a
      typed multi-container model.
- [x] Extend `internal/appconfig` (or introduce `internal/machineconfig`) to
      represent Fly Machines API configuration:
      `containers[]`, per-container image/command/args/env/files/mounts/restart,
      machine guest resources, services, checks, region, and lifecycle policy.
- [x] Preserve container names and reject duplicate, empty, or invalid names.
- [x] Define and validate colocation semantics: containers in one machine share
      scale and lifecycle; independently scaled processes must be separate groups
      or applications.
- [x] Support different images in one machine (required by API `cloudflared`).
- [x] Add a deterministic mapping from Score containers and
      `metadata.progresify.processes` to machine groups.
- [x] Add a Machines API deployer using the existing generated client in
      `internal/flymachines`:
      create, inspect, update, wait, stop, suspend, restart, delete, and list.
- [x] Implement idempotent reconciliation. Re-running apply with the same plan
      must produce no unnecessary machine replacement.
- [x] Add partial-failure recovery and rollback behavior.
- [x] Wait for machine and health-check readiness; do not report success after
      only receiving a successful API response.
- [x] Make `generate --deploy` use the Machines API when the workload requires
      multi-container support. Never silently fall back to an invalid TOML plan.

## P0 — Angada topology contract

- [x] Add typed support for the `metadata.progresify` contract used by
      `platform-score-apps`: owner, Slack channel, secret namespace, region,
      ingress, release command, variables, processes, VM, scale, HTTP service,
      concurrency, and checks.
- [x] Validate that every declared process has exactly one matching Score
      container and that no container is left unconfigured.
- [x] Support API `app`, `worker`, and `cloudflared` as distinct containers.
- [ ] Support review and wiki app/worker topology with explicit machine-group
      behavior.
- [x] Enforce private ingress for internal services and Cloudflare-only ingress
      for public staging API traffic.
- [ ] Reject production credentials, public Fly services, mutable image tags, or
      staging scale values that violate the thin-staging policy.

## P1 — Score CLI and state compatibility

- [x] Keep compatibility with `init`, `generate`, `--deploy`, `--secrets-file`,
      overrides, and provisioner commands.
- [x] Add explicit `validate`, `plan`, `apply`, `status`, `logs`, `reconcile`,
      `scale`, and `destroy` commands.
- [x] Add `--dry-run` and machine-readable JSON plan output.
- [x] Include renderer version, workload, environment, machine group, and image
      digest in plan output and Fly machine metadata.
- [x] Add state schema versioning and migration from existing
      `.score-flyio/state.yaml`.
- [ ] Add remote state support or a documented CI artifact strategy.
- [x] Add state locking to prevent concurrent CI deployments.
- [x] Redact secrets from state diagnostics, plans, logs, and errors.
- [x] Make deprovisioning safe and explicit; detect orphaned machines before
      deleting anything.

## P1 — images, secrets, and resources

- [x] Require prebuilt, registry-accessible images for Machines API deployment.
- [x] Resolve and record immutable image digests; reject `latest` in release
      environments.
- [ ] Validate architecture (staging and production currently require amd64).
- [x] Keep Score resource provisioners and substitutions working for every
      container, including secret access tracking.
- [ ] Add an Infisical provisioner/adapter with per-environment namespaces.
- [x] Ensure secrets are injected at deploy time and are never serialized into
      generated manifests or logs.
- [ ] Support secret rotation without rebuilding images.
- [ ] Test that staging database, R2, GitHub App, Slack, Daytona, Hatchet, and
      model credentials cannot resolve to production resources.

## P1 — networking and Cloudflare

- [x] Model Cloudflare Tunnel attachment and hostname in deployment values.
- [ ] Validate the staging hostname (`flowbit.work`) and tunnel route before
      apply.
- [ ] Inject the tunnel token from Infisical, never from repository files.
- [x] Add a post-deploy check for Tunnel connector health and API reachability.
- [ ] Verify direct Fly origin access is impossible for public staging API.
- [ ] Verify internal service URLs resolve only over the private network.

## P1 — lifecycle, health, and observability

- [x] Map Score probes to Fly machine/service checks, preserving method, path,
      headers, interval, timeout, and grace period.
- [x] Support per-group VM size, region, minimum, and maximum machine counts.
- [x] Support scale-to-zero and resume for staging workers.
- [x] Support release commands with exactly-once and failure-stop semantics.
- [x] Surface Fly machine events, container exits, failed checks, and deployment
      diffs in CLI output.
- [ ] Add structured deployment logs and correlation IDs.
- [ ] Add metrics configuration for each service where requested.

## P1 — tests and acceptance gates

- [x] Add unit tests for every Score-to-Machine field and invalid combination.
- [ ] Add golden JSON fixtures for API, review, wiki, LiteLLM, and Hatchet.
- [x] Add mocked Machines API contract tests for create/update/reconcile/rollback.
- [ ] Add idempotency, orphan cleanup, secret redaction, and scale-to-zero tests.
- [ ] Add a local E2E profile with fake GitHub, LLM, OCR, Hatchet, Daytona, R2,
      Slack, and Cloudflare services and zero cloud spend.
- [ ] Fix the local review E2E OCR contract: align the pinned OCR version,
      Daytona snapshot, fake binary, fixtures, and documentation (currently
      `v1.11.7` versus `v1.10.0`).
- [ ] Add a release-candidate staging smoke test covering:
      Cloudflare → Tunnel → API → signed webhook → Hatchet → review/wiki worker
      → completion callback.
- [ ] Verify LiteLLM virtual-key restrictions and provider-enforced monthly cap.
- [ ] Verify public/internal router separation and direct-origin denial.

## P2 — migration and operations

- [ ] Build a compatibility report comparing generated Machines config with the
      current `platform-fly-apps` Fly configuration.
- [ ] Add an import/adoption command for existing Fly apps and machines.
- [ ] Document rollback to the previous deployer and a one-command restore path.
- [ ] Publish versioned binaries and pin the custom renderer in each service CI.
- [ ] Add CI checks that reject accidental `fly.toml` regeneration for migrated
      workloads.
- [ ] Add runbooks for deploy, suspend, resume, rotate secrets, inspect logs,
      and recover a failed machine.
- [ ] Keep staging suspended outside release windows while preserving secrets,
      IaC, and the exact topology needed to restore it.

## Definition of done

- [ ] API, review, wiki, LiteLLM, and Hatchet deploy from Score without requiring
      hand-authored `fly.toml` files.
- [ ] API's three-container topology deploys and reconciles successfully.
- [ ] A second identical apply is a no-op.
- [ ] Local fake-backed API E2E completes a review with no cloud provider calls.
- [ ] One controlled staging smoke test proves the release gate and stays within
      the thin-staging resource and LLM budgets.
- [ ] Production direct-origin access remains impossible.
