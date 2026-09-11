# `metadata.fly` reference

`score-flyio` reads an optional renderer-owned `fly` extension from Score
workload metadata. Workloads that declare it are deployed through the Fly
Machines API path; workloads without it use the legacy single-container
`fly.toml` path. The extension shape is application-neutral: topology, secret
providers, domains, and environment policy are supplied by the caller that
authors the workload.

```yaml
apiVersion: score.dev/v1b1
metadata:
  name: demo-app
  fly:
    ingress:
      type: public
      hostname: demo-app.example.internal
    region: ams
    release_command: ["bin/migrate"]
    variables:
      LOG_LEVEL: info
    processes:
      web:
        machine_group: app
        vm: { cpus: 1, memory_mb: 512 }
        scale: { min: 2, max: 4 }
        http_service:
          internal_port: 8080
          ports:
            - port: 443
              handlers: [http]
        checks:
          ready:
            type: http
            port: 8080
            method: get
            path: /readyz
            interval_seconds: 10
            timeout_seconds: 5
      tunnel:
        machine_group: app
      worker:
        vm: { cpus: 2, memory_mb: 1024 }
        scale: { min: 1, max: 1 }
containers:
  web: { image: "registry.example/demo-app/web@sha256:<digest>" }
  tunnel: { image: "registry.example/demo-app/tunnel@sha256:<digest>" }
  worker: { image: "registry.example/demo-app/worker@sha256:<digest>" }
```

## Top-level fields

| Field | Type | Notes |
| --- | --- | --- |
| `region` | string | 2–4 lowercase letters; Fly primary region for all groups. Default `iad`. |
| `ingress` | object | External traffic policy: `type` (`none`, `private`, `public`), optional `hostname` (public only) and `tunnel`. |
| `release_command` | string list | One-off command run on a dedicated machine before rollout; runs once per plan change. |
| `variables` | map | App-level variables applied to every process. |
| `owner` / `slack_channel` / `secret_namespace` | strings | Caller-owned free-form labels; used for secret namespacing when set. |
| `processes` | map | Per-container process configuration; keys must match Score `containers` keys exactly. |

## Process fields

| Field | Type | Notes |
| --- | --- | --- |
| `machine_group` | string | Colocation group. Processes in one group share a machine and must share `vm`, `scale`, and `restart`. Omit to give the process its own group named after it. |
| `profile` | string | Optional label: `service`, `worker`, or `cron`. |
| `image` | string | Overrides the Score container image; must be pinned to a `sha256` digest at deploy time. |
| `command` / `args` | string lists | Override the container entrypoint/arguments. |
| `variables` | map | Process-level variables; merged over app-level `variables`. |
| `vm` | object | `cpus` (>= 1) and `memory_mb` (>= 256, multiple of 256). |
| `scale` | object | `min`/`max` machine count per group; `max >= min`, `min >= 0`. Scale-to-zero is allowed. |
| `http_service` | object | Internal port plus public `ports` (`port`, `handlers`: `http`, `tls`, `http+tls`), `auto_stop` (`stop`/`off`), `auto_start`, `min_machines_running`, `protocol`. Public ports require `ingress.type: public`. |
| `concurrency` | object | `type`, `soft_limit`, `hard_limit` for the machine service. |
| `checks` | map | Health checks: `type` (`http`, `tcp`), `port`, `method`+`path` (http), optional `headers`, `interval_seconds` > 0, `timeout_seconds` > 0 and < interval, `grace_period_seconds`. |
| `restart` | string | `always`, `no`, or `on-failure`. |

Unknown fields are rejected at validation time, so misspelled keys fail fast
with `validate` or `plan --dry-run` instead of being silently ignored.

A camelCase shape (`primaryRegion`, `releaseCommand`, `machineGroup`,
`httpService`, …) is accepted and normalized to the canonical snake_case form.

## Plugin-owned vs caller-owned metadata

On Fly machines themselves, `score-flyio` writes exactly two machine metadata
keys and treats them as its management contract:

| Key | Owner | Meaning |
| --- | --- | --- |
| `flydeploy.group` | plugin | Machine group name; identifies machines the planner manages. |
| `flydeploy.config-hash` | plugin | Hash of desired config; equal hashes make re-apply a no-op. |

Machines without `flydeploy.group` are never modified, scaled, or destroyed by
this tool. Every other value under `metadata.fly` is caller-owned: the plugin
copies it into the plan verbatim and attaches no product-specific meaning.

## Secret handling

Workload variables resolved from resources become runtime secrets: they are
imported into the Fly app (never written to plans, state, logs, or errors) and
appear in plan artifacts only as required secret *names*. Exact-plan artifacts
(`plan --plan-output`) list `required_secrets` and `apply --plan-file` demands
a matching `--secrets-file` restricted to mode `0600`.
