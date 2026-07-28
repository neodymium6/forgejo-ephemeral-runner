# Security model

## Trust boundary

Each workflow is treated as arbitrary code. Its trust boundary is the runner
container and the network reachable from that Pod. The design protects the
Kubernetes node and control plane by withholding privileged mode, host mounts,
container-runtime sockets, and Kubernetes credentials from the runner.

The controller is a separate Pod with a separate service account. A standard
Kubernetes NetworkPolicy denies ingress to both controller and runner Pods.

## Credentials

The long-lived Forgejo API token is available only to the controller. It is
used to list, create, and delete runners at one configured Forgejo scope. The
controller creates a short-lived Kubernetes Secret containing the resulting
ephemeral runner UUID and token.

Only the short-lived Secret is mounted into the runner Pod. A workflow can read
that credential because Forgejo Runner requires it, but Forgejo marks the
runner ephemeral and assigns it at most one job. The controller deletes the
Secret after the Pod disappears. Forgejo invalidates the runner credential
after its job completes or times out.

The API token remains a high-value credential. Use a dedicated Forgejo account,
select the narrowest runner scope, and grant the narrowest token permissions
that Forgejo supports. Compromise of the controller can exercise all runner
management privileges available to that account and token.

Repository and organization Actions secrets are outside this project's control.
Forgejo sends them to eligible workflows. Operators should register runners at
the narrowest practical scope and protect branches that can modify workflows.

## Kubernetes permissions

The controller service account can create Pods and Secrets in its own
namespace. `get` and `delete` are additionally restricted with `resourceNames`
to the one deterministic runner Pod and credential Secret. It cannot read other
Secret data through the Kubernetes API, list objects, watch objects, update
objects, or access another namespace.

The namespace must contain no unrelated workloads or Secrets. Kubernetes RBAC
cannot restrict `create` to one exact object name, so a compromised controller
can create additional Pods or Secrets inside its namespace even though its
normal code uses deterministic names. A dedicated namespace keeps that residual
capability away from unrelated credentials and workloads.

The runner service account has no RBAC binding, and its token is not
automounted. The runner Pod receives no projected Kubernetes token.

## Lifecycle and stale cleanup

The controller maintains one deterministic runner slot. A terminal runner Pod
is deleted first. On the next reconciliation, the controller deletes the
Forgejo registration recorded on the short-lived Secret, then deletes the
Secret and creates a fresh registration.

A controller crash can happen after Forgejo creates a runner but before
Kubernetes records its ID. Before creating a new registration, the controller
therefore lists runners directly owned by its configured scope. It deletes a
stale runner only when all of the following match:

- the deterministic runner name;
- the controller's fixed project description;
- Forgejo's ephemeral flag.

Any name collision that does not meet all three conditions stops reconciliation
instead of deleting the existing runner. Only one controller instance may own
a given scope and runner name.

A runner Pod deletion or node disruption can interrupt an active job. The
controller cleans up the old registration and creates a clean Pod after it
returns. Forgejo job timeout and retry controls remain part of recovery.

## Nix execution

Nix runs as root inside the unprivileged runner container because it must write
to the image's `/nix/store`. All Linux capabilities are dropped and privilege
escalation is disabled. No Nix store is shared between Pods in the base design.
This avoids cross-job cache poisoning at the cost of additional downloads and
build time.

A future shared cache should use a content-addressed binary cache with signing
and explicit trust policy instead of a writable shared `/nix` volume.

## Known limitations

- The project is not an official Forgejo component.
- The controller is single-replica and has no leader election.
- There is always one waiting Pod; scale-to-zero requires a queue-aware
  controller.
- A single runner slot provides concurrency one, not high availability.
- Forgejo 15 token scopes may not isolate runner administration from other
  administrative operations as narrowly as desired.
- Generic NetworkPolicy cannot express Forgejo and binary-cache FQDN allowlists
  with the standard Kubernetes API.
- The base does not restrict egress, so workflows can reach whatever cluster
  routing and external network policy allow.
- Resource exhaustion inside a job can pressure the selected Kubernetes node;
  requests, limits, quotas, and monitoring remain required.
- A crash in the small interval between remote registration and local state
  persistence relies on deterministic stale discovery during restart.
