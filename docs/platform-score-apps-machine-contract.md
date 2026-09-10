# Score-to-Fly Machines contract

This is the frozen deployment contract consumed by the custom `score-flyio`
renderer. The upstream `platform-score-apps` repository is not present in this
workspace, so this copy is kept beside the renderer until that repository can
carry the canonical document.

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

The Angada topology is:

- API group: `app`, `worker`, `cloudflared`.
- Wiki group: `app`, `worker`.
- Review group: the defined review worker topology.
- LiteLLM and Hatchet: separate private apps.

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
