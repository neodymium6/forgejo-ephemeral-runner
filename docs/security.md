# Security model

## Trust boundary

Each workflow is treated as arbitrary code. Its trust boundary is the runner
container and the network reachable from that Pod. The design protects the
Kubernetes node and control plane by withholding privileged mode, host mounts,
container-runtime sockets, and Kubernetes credentials from the runner.

The controller is a separate Deployment with a separate service account. A
standard Kubernetes NetworkPolicy denies ingress to controller and runner Pods.

## Credentials

The long-lived Forgejo API token is available only to the two trusted
controller replicas. Only the replica holding the Kubernetes Lease uses it for
normal reconciliation. It lists jobs and creates, lists, and deletes runners at
one configured Forgejo scope.

For each waiting job, the controller creates a temporary Kubernetes Secret
containing the ephemeral runner UUID, one-job token, and opaque job handle.
Only that Secret is mounted into its runner Pod. A workflow can read the
credential because Forgejo Runner requires it, but Forgejo marks the runner
ephemeral and `one-job --handle` targets one job attempt. The controller deletes
the Secret during cleanup.

The API token remains a high-value credential. Use a dedicated Forgejo account,
select the narrowest runner scope, and grant the narrowest route-level token
scope that Forgejo supports. Forgejo 15 has no runner-only token permission:

| Runner endpoint | Route-level token scope |
| --- | --- |
| user | `write:user` |
| organization | `write:organization` |
| repository | `write:repository` |
| instance | `write:admin` |

The account must also pass the ownership or administration checks for the
selected endpoint. Forgejo documents repository-specific access tokens as
limited to repository and issue scopes and unable to perform repository
administrative operations. Verify that token type against the intended runner
endpoint instead of assuming it is sufficient.

Compromise of either controller replica can exercise every runner-management
operation available to the account and token. Repository and organization
Actions secrets are outside this project's control: Forgejo sends them to
eligible workflows. Protect branches that can modify workflows.

## Kubernetes permissions

The controller service account can create Pods and Secrets in its own
namespace. `get` and `delete` are restricted with `resourceNames` to ten fixed
runner Pod and credential Secret names. It can also create a Lease in that
namespace and can `get` and `update` only the fixed leader-election Lease.

The controller cannot list Pods, Secrets, or Leases, read unrelated Secret
data, or access another namespace. The namespace should contain no unrelated
workloads or Secrets. Kubernetes RBAC cannot restrict `create` to an exact
object name, so a compromised controller can create additional Pods, Secrets,
or Leases inside its namespace. A dedicated namespace contains that residual
capability.

The runner service account has no RBAC binding, and its token is not
automounted. The runner Pod receives no projected Kubernetes token.

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
should fail closed or be recovered by normal cleanup, but end-to-end failure
injection against a disposable Forgejo instance is still required.

## Lifecycle and stale cleanup

A terminal runner Pod is deleted first. On the next reconciliation, the
controller deletes the Forgejo registration recorded in the temporary Secret
and then deletes the Secret. A new registration is created only when another
matching job is waiting.

A controller crash can happen after Forgejo commits runner creation but before
Kubernetes records its ID. The two APIs provide neither a shared transaction
nor an idempotency key. Before creating a registration for a free slot, the
controller therefore lists runners directly owned by its configured scope. It
deletes a stale runner only when all of the following match:

- the deterministic slot name;
- the controller's fixed project description;
- Forgejo's ephemeral flag.

Any name collision that does not meet all three conditions stops reconciliation
instead of deleting the existing runner. Deterministic slot identity makes the
non-atomic failure recoverable, but it does not remove the transaction boundary.
Only one controller Deployment and identity set may own a given scope and
runner-name prefix.

A Pod deletion or node disruption can interrupt an active job. The controller
cleans up its old registration after Kubernetes returns. Forgejo job timeout
and retry controls remain part of recovery.

## Nix execution

Nix runs as root inside the unprivileged runner container because it must write
to the image's `/nix/store`. All Linux capabilities are dropped and privilege
escalation is disabled. No Nix store is shared between Pods in the base design.
This avoids cross-job cache poisoning at the cost of downloads and build time.

A future shared cache should use a content-addressed binary cache with signing
and explicit trust policy instead of a writable shared `/nix` volume.

## Known limitations

- The project is not an official Forgejo component.
- The jobs API is polled rather than watched.
- Concurrency has a fixed maximum of ten slots.
- Controller leader election depends on Kubernetes API availability.
- Controller replicas both possess the long-lived Forgejo API token.
- Forgejo 15 token scopes are broader than runner administration alone.
- Generic NetworkPolicy cannot express Forgejo and binary-cache FQDN allowlists
  with the standard Kubernetes API.
- The base does not restrict egress, so workflows can reach whatever cluster
  routing and external network policy allow.
- Resource exhaustion inside a job can pressure the selected Kubernetes node;
  requests, limits, quotas, and monitoring remain required.
- The remote-registration/local-state transaction boundary is mitigated by
  deterministic recovery rather than eliminated.
- A disposable-Forgejo integration and failure-injection suite is not yet
  included.
