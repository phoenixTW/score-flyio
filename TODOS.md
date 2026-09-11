# score-flyio implementation backlog

`score-flyio` is a reusable Score renderer and Fly Machines reconciler. It
must remain application-neutral: service topology, secret providers, domains,
budgets, and environment policy are supplied by the caller.

## Complete

- [x] Model multi-container machine groups with independent group scaling.
- [x] Convert Score containers into deterministic machine plans.
- [x] Preserve legacy single-container TOML generation.
- [x] Validate container names, resources, services, checks, and immutable
      image references.
- [x] Add Machines API create, update, inspect, wait, lifecycle, and delete
      primitives with retries and timeouts.
- [x] Add deterministic plan/diff and idempotent reconciliation with rollback.
- [x] Add `validate`, `plan`, `apply`, `status`, `logs`, `reconcile`, `scale`,
      `suspend`, `resume`, and `destroy` commands.
- [x] Keep state schema versioning, locking, overrides, provisioners, and
      secret redaction.

## Next

- [x] Make the renderer-owned metadata extension fully documented and stable.
- [ ] Add architecture selection and resource-policy validation.
- [ ] Add adoption/import and a compatibility report for existing machines.
- [ ] Add remote-state/CI artifact guidance and deployment correlation IDs.
- [x] Add golden plans for generic service, worker, and sidecar workloads.
- [x] Add idempotency, orphan cleanup, secret-redaction, and scale-to-zero
      integration fixtures.
- [x] Add a local fake Machines API end-to-end profile with zero cloud spend.
- [ ] Add release packaging checks and pin the renderer in caller CI.
- [ ] Add runbooks for deploy, rollback, suspend, resume, rotation, and logs.

## Definition of done

- [ ] A caller can render a multi-container Score workload without hand-written
      `fly.toml`.
- [ ] Repeated apply with the same plan is a no-op.
- [ ] Failed health checks stop rollout and clean up safely.
- [ ] Secret values never appear in plans, logs, or errors.
- [ ] Adoption and orphan handling are explicit and auditable.
