# Security model

## Trust boundary

Each workflow is treated as arbitrary code. Its trust boundary is the runner
container and the network reachable from that Pod. The design protects the
Kubernetes node and control plane by withholding privileged mode, host mounts,
container-runtime sockets, and Kubernetes credentials from the runner.

The controller is a separate Deployment with a separate service account. The
base includes a standard Kubernetes NetworkPolicy that denies ingress to
controller and runner Pods when the target cluster's CNI enforces
NetworkPolicy. The manifest alone cannot enable enforcement in the CNI.

## Metrics endpoint

Each controller replica serves unauthenticated Prometheus metrics on TCP port
9090 by default. The fixed schema exposes aggregate leadership, capacity,
queue-observation, reconciliation, mutation, and cleanup state plus the build
version. The exporter payload does not expose repository or job identities,
runner identifiers, Forgejo URLs, credentials, Kubernetes UIDs, or controller
Pod names. Prometheus service discovery may add its own Kubernetes target
labels. Operational counts and software versions are still deployment
information and should not be public.

The base creates no Service or monitor object, and its default-deny ingress
NetworkPolicy selects the controller Pods. A private overlay must explicitly
add both discovery and a narrow ingress allow rule for its Prometheus workload.
Application-level authentication is not a substitute for that network boundary
because the endpoint intentionally implements none. See
[metrics.md](metrics.md) for the schema and deployment guidance.

## Credentials

The long-lived Forgejo API token is available only to the two trusted
controller replicas. Only the replica holding the Kubernetes Lease uses it for
normal reconciliation. One token is reused across every exact repository in the
configured allowlist. The explicit `*` mode instead uses that token at the user
runner scope.

For each waiting job, the controller creates a temporary Kubernetes Secret
containing the ephemeral runner UUID, one-job token, and opaque job handle. The
repository scope is retained as Secret metadata for cleanup after a restart;
it is not projected into the runner container. Only the credential data is
mounted into its runner Pod. A workflow can read the
credential because Forgejo Runner requires it, but Forgejo marks the runner
ephemeral and `one-job --handle` targets one job attempt. The controller deletes
the Secret during cleanup. Before deleting the recorded Forgejo runner, it
verifies that the ID still belongs to the deterministic slot name and matches
the managed description and ephemeral flag; a mismatch fails closed.

The API token remains a high-value credential. Prefer a dedicated Forgejo
account when Forgejo's authorization checks permit it, select the narrowest
repository restrictions, and grant the narrowest route-level token scope that
Forgejo supports. Forgejo 15 has no runner-only token permission. Exact
repository entries normally require `write:repository`; the explicit `*` mode
uses the user runner endpoint and requires `write:user`:

| Runner endpoint | Route-level token scope |
| --- | --- |
| user | `write:user` |
| repository | `write:repository` |

The account must also pass the ownership or administration checks for the
selected endpoint. On Forgejo 15, the repository-scoped runner endpoint for a
personal repository requires the actual repository owner; granting a separate
collaborator administrative access is insufficient. In that case an
owner-issued `write:repository` token is necessary. Moving the repository to an
organization is the practical way to introduce a separate runner identity with
appropriate organization authority. Verify the account and token type against
the intended runner endpoint instead of assuming a collaborator or
fine-grained repository token is sufficient.

Compromise of either controller replica can exercise every runner-management
operation available to the account and token. Repository and organization
Actions secrets are outside this project's control: Forgejo sends them to
eligible workflows. Protect branches that can modify workflows.

## Release credentials

The tag-triggered release workflow receives a registry credential through the
`REGISTRY_TOKEN` Actions secret. The credential should grant package write
access only; it does not need repository administration or runner-management
access. The workflow uses Forgejo's job token separately to create a Release in
the same repository.

GitHub provides a short-lived repository `GITHUB_TOKEN` to the release workflow.
Explicit `contents: write` and `packages: write` permissions allow it to publish
GHCR images and create the matching GitHub Release without a personal access
token. CI has only `contents: read`, receives no release credential, and uploads
no artifacts.

A version tag selects the exact source that receives the registry credential.
Protect the `v*.*.*` tag pattern so unreviewed commits cannot trigger a release.
The workflow does not run for pull requests, does not publish mutable `latest`
tags, refuses to replace an existing version with a different digest, and does
not deploy the resulting images to Kubernetes. The Actions runner and Forgejo
instance remain trusted parts of this release boundary.

## Kubernetes permissions

The controller service account can create Pods and Secrets in its own
namespace. `get` and `delete` are restricted with `resourceNames` to ten fixed
runner Pod and credential Secret names. It can also create a Lease in that
namespace and can `get` and `update` only the fixed leader-election Lease.

The controller cannot list Pods, Secrets, or Leases, read unrelated Secret
data, or access another namespace. It validates the managed-by and slot labels
plus the API-assigned UID on every fixed-name Pod and Secret, and includes that
UID as a deletion precondition so a same-name replacement is not removed. The
namespace should contain no unrelated workloads or Secrets. Kubernetes RBAC
cannot restrict `create` to an exact object name, so a compromised controller
can create additional Pods, Secrets, or Leases inside its namespace. A dedicated
namespace contains that residual capability.

The runner service account has no RBAC binding, and its token is not
automounted. The runner Pod receives no projected Kubernetes token.

Before creating a runner Pod, the controller strictly decodes and validates
the rendered template. Unknown fields are rejected. It permits exactly one
configured runner image invoking the one-job launcher, literal reviewed
environment values, the settings ConfigMap as the only `envFrom` source, the
fixed unbound service account, the reviewed security context, and the five
reviewed ConfigMap, credential Secret, and `emptyDir` volumes and mounts.
Secret-backed `env` and `envFrom` sources, sidecars, init or ephemeral
containers, host namespaces, host ports, privileged execution, added
capabilities, mount propagation, host paths, projected tokens, persistent
volumes, and CSI volumes are rejected. The base namespace also carries labels
requesting Kubernetes Pod Security Baseline enforcement as an admission-time
backstop. Any future cache or volume design must update these checks explicitly.

## Leader election

The base runs two controller replicas. They compete for one Kubernetes Lease;
only its holder runs the reconciliation loop. Lease updates include the current
`resourceVersion`, so a conflicting update does not grant leadership. A replica
uses local observation time to determine expiration rather than trusting a
remote clock value.

The current timing is a 15-second Lease duration, 10-second renew deadline, and
2-second retry/request interval. Losing renewal stops the active reconciliation
context before that replica competes again. Preferred Pod anti-affinity spreads
replicas when capacity permits, but it is not a hard scheduling guarantee.

This improves controller availability; it does not make a running job Pod
highly available. Deleting its Pod or losing its node interrupts that job.

## Queue and fixed slots

The active controller polls Forgejo for matching jobs. Only jobs with
`waiting` status cause runner creation; running jobs are not duplicated. With
no waiting jobs, the runner count converges to zero. Each free slot receives a
specific opaque job handle, and Forgejo Runner starts with
`one-job --handle`.

`MAX_CONCURRENT` is configurable from 1 through 10. Ten deterministic names are
reserved so read/delete RBAC can remain name-restricted. The controller only
reconciles slots below the configured limit. Before lowering the limit, allow
higher-numbered slots to become idle and finish cleanup, or clean them through
the previous configuration.

Polling adds up to one reconciliation interval before a job is observed. A job
can also be canceled or claimed between listing and runner startup. These races
are handled by bounded idle polling in the patched runner. Its deadline is
checked only after a successful empty task response; an assigned task wins over
the deadline, and ambiguous fetch errors preserve the idempotency key. Job-list
absence never causes controller-driven Pod deletion. See
[runner-lifecycle.md](runner-lifecycle.md) for the recovery boundary, including
the limitation during a continuing API outage. The opt-in E2E harness
includes, and has exercised in a successful local run, a failure-injection
scenario for a crash after Forgejo commits a runner registration and before
Kubernetes records it.

## Lifecycle and stale cleanup

A terminal runner Pod is deleted first. On the next reconciliation, the
controller deletes the Forgejo registration recorded in the temporary Secret
and then deletes the Secret. A new registration is created only when another
matching job is waiting.

A controller crash can happen after Forgejo commits runner creation but before
Kubernetes records its ID. The two APIs provide neither a shared transaction
nor an idempotency key. Before creating a registration for a free slot, the
controller therefore lists runners directly owned by its configured scopes. It
deletes a stale runner only when all of the following match:

- the deterministic slot name;
- the controller's fixed project description;
- Forgejo's ephemeral flag.

Any name collision that does not meet all three conditions stops reconciliation
instead of deleting the existing runner. Deterministic slot identity makes the
non-atomic failure recoverable, but it does not remove the transaction boundary.
Only one controller Deployment and identity set may own a given scope and
runner-name prefix. Do not deploy controllers with overlapping allowlists and
matching labels; they can race to claim the same waiting job.

A Pod deletion or node disruption can interrupt an active job. The controller
cleans up its old registration after Kubernetes returns. A Pod that remains
`Pending` for `RUNNER_STARTUP_TIMEOUT` (30 minutes by default), or whose phase
is `Unknown` after `RUNNER_UNKNOWN_TIMEOUT` (5 minutes by default), is deleted
with a UID precondition so the fixed slot can recover. Both ages are measured
from the Pod creation timestamp. Forgejo job timeout and retry controls remain
part of recovery.

## Nix execution

Nix runs in single-user mode as the dedicated UID and GID 65532. The image gives
that identity ownership of its private Nix store and state directory; it does
not grant root, Linux capabilities, or privilege escalation. In particular, a
workflow cannot create Nix's reserved `/homeless-shelter` build-home path at the
filesystem root. No Nix store is shared between Pods in the base design. This
avoids cross-job cache poisoning at the cost of downloads and build time.

A future shared cache should use a content-addressed binary cache with signing
and explicit trust policy instead of a writable shared `/nix` volume.

## Known limitations

- The project is not an official Forgejo component.
- The jobs API is polled rather than watched.
- Exact repositories are polled in allowlist order; a sustained queue can favor
  earlier entries under the global concurrency limit.
- Concurrency has a fixed maximum of ten slots.
- Controller leader election depends on Kubernetes API availability.
- Controller replicas both possess the long-lived Forgejo API token.
- Forgejo 15 token scopes are broader than runner administration alone.
- NetworkPolicy isolation is effective only when the target cluster's CNI
  enforces it.
- Generic NetworkPolicy cannot express Forgejo and binary-cache FQDN allowlists
  with the standard Kubernetes API.
- The base does not restrict egress, so workflows can reach whatever cluster
  routing and external network policy allow.
- Resource exhaustion inside a job can pressure the selected Kubernetes node;
  requests, limits, quotas, and monitoring remain required.
- The remote-registration/local-state transaction boundary is mitigated by
  deterministic recovery rather than eliminated.
