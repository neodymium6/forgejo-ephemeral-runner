# Prometheus metrics

Each controller replica serves Prometheus text-format metrics at `/metrics` on
TCP port 9090 by default. `METRICS_LISTEN_ADDRESS` changes the listen address;
it must contain an empty or literal IP host and a port from 1 through 65535.
For example, `:9090` listens on all Pod interfaces and `127.0.0.1:9090` listens
only inside the container network namespace.

The endpoint has no application-level authentication. The public Kustomize
base declares the container port but intentionally creates no Service,
PodMonitor, or ServiceMonitor. Its default-deny ingress NetworkPolicy also
selects the controller Pods. A private deployment overlay should create the
Prometheus discovery object and permit ingress only from the intended
monitoring namespace and Pods. Do not expose the endpoint through a public
Ingress or LoadBalancer.

## Metric schema

The exporter schema uses only fixed labels, except for the controller build
version. Its payload does not include repository names, job identifiers or
handles, runner IDs or names, Forgejo URLs, tokens, Kubernetes object UIDs, or
controller Pod names. Prometheus may attach Kubernetes discovery labels such as
the target Pod name; the private monitor configuration controls those labels.

| Metric | Type | Meaning |
| --- | --- | --- |
| `forgejo_ephemeral_runner_build_info` | gauge | Build version, always 1 |
| `forgejo_ephemeral_runner_leader` | gauge | 1 for the current Lease holder |
| `forgejo_ephemeral_runner_runner_slots_active` | gauge | Active or deleting runner Pods |
| `forgejo_ephemeral_runner_runner_slots_capacity` | gauge | Configured slot limit |
| `forgejo_ephemeral_runner_waiting_jobs` | gauge | Matching waiting jobs from the latest queue observation |
| `forgejo_ephemeral_runner_queue_observed` | gauge | 1 when the latest reconciliation queried the queue |
| `forgejo_ephemeral_runner_reconciliations_total` | counter | Reconciliations by `success` or `failure` |
| `forgejo_ephemeral_runner_reconcile_duration_seconds` | summary | Reconciliation duration as `_sum` and `_count` |
| `forgejo_ephemeral_runner_operations_total` | counter | Forgejo and Kubernetes mutations by fixed `operation` and `result` |
| `forgejo_ephemeral_runner_cleanup_attempts_total` | counter | Cleanup decisions by fixed `reason` |

The `operation` values are `registration_create`, `registration_delete`,
`credential_create`, `credential_delete`, `pod_create`, and `pod_delete`. The
cleanup reasons are `orphan_pod`, `pending_timeout`, `unknown_timeout`,
`pod_succeeded`, `pod_failed`, and `stale_registration`.

When all slots are occupied, reconciliation does not need to query Forgejo's
job queue. In that state `queue_observed` is 0 and `waiting_jobs` is reset to 0;
the latter must not be interpreted as an empty queue. Metrics are in memory and
their counters reset when a controller replica restarts.

## Prometheus and Grafana

Scrape both controller replicas. Leader-only gauges are cleared when a replica
loses the Lease, while counters remain local to each process. These PromQL
queries are useful starting points:

```promql
# Exactly one active controller is expected.
sum(forgejo_ephemeral_runner_leader)

# Current runner use and configured capacity.
sum(forgejo_ephemeral_runner_runner_slots_active)
max(forgejo_ephemeral_runner_runner_slots_capacity)

# Queue value only when it was observed.
sum(forgejo_ephemeral_runner_waiting_jobs)
sum(forgejo_ephemeral_runner_queue_observed)

# Reconciliation failures across current replicas.
sum(rate(forgejo_ephemeral_runner_reconciliations_total{result="failure"}[5m]))

# Mutation failures by bounded operation name.
sum by (operation) (
  rate(forgejo_ephemeral_runner_operations_total{result="failure"}[5m])
)
```

A basic dashboard can show leader count, active slots versus capacity, queue
observation and waiting jobs, reconciliation failure rate and latency, mutation
failure rate, and cleanup rate by reason. Alerts should at least cover a leader
count other than 1 for a sustained interval and sustained reconciliation or
mutation failures. Keep the Prometheus Service, monitor object, NetworkPolicy
allow rule, dashboard, and alert routing in the private infrastructure
repository because their selectors and namespace names describe the real
deployment.
