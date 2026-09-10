# Score-to-Fly Machines contract

This is the generic deployment contract consumed by `score-flyio`. Application
repositories may translate their own workload configuration into this contract;
the renderer does not contain application-specific topology or policy.

## Workload and machine groups

- One Score workload maps to one Fly app.
- A machine group defines colocation and scaling.
- Containers in a group share lifecycle, region, VM size, and scale.
- Different machine groups may scale independently.
- Every container has an explicit image, command, environment, and restart
  policy. Defaults are resolved before a plan is emitted.
- Public ingress is opt-in. Private services do not receive a Fly public
  service.
- Images resolve to immutable digests before `plan` or `apply`.

## Planner contract

Conversion is pure and ordered:

```text
Score + resolved resources -> MachinePlan -> validation -> diff -> apply
```

Plans may include secret names and secret references, but never secret values.
The exact plan passed to `apply` is the plan produced by `plan`; applying a
different input requires creating a new plan.

## Compatibility

`init`, `generate`, `generate --deploy`, provisioners, overrides, and secret
file output remain supported. Legacy single-container workloads may still
produce TOML. Multi-container workloads must select the Machines API path and
must never silently fall back to TOML.
