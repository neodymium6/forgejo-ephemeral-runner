# Security model

## Trust boundary

Each workflow is treated as arbitrary code. Its trust boundary is the runner
container and the network reachable from that Pod. The design protects the
Kubernetes node and control plane by withholding privileged mode, host mounts,
container-runtime sockets, and Kubernetes credentials from the runner.

The reaper is a separate container. Kubernetes does not share its projected
service account token with the runner container.

## Credentials

The long-lived Forgejo registration token is available only to the init
container. Registration writes a runner-specific credential to an `emptyDir`.
The runner container can read that credential because Forgejo Runner requires
it, but the Forgejo server marks it ephemeral and assigns it at most one job.

Repository and organization Actions secrets are outside this project's control.
Forgejo sends them to eligible workflows. Operators should register runners at
the narrowest practical scope and protect branches that can modify workflows.

## Kubernetes permissions

The reaper service account can get and delete Pods in its own namespace. The
runner cannot use that service account. A compromised reaper could disrupt
other runner Pods in the same namespace, so the namespace should contain no
unrelated workloads.

Kubernetes RBAC cannot restrict a Role to a dynamically named current Pod. The
separate token mount is therefore an essential control, not an optimization.

## Pod replacement

The runner writes a completion marker only after `forgejo-runner one-job`
returns. The reaper also replaces a Pod if Kubernetes reports that the runner
container restarted or terminated. Writing the marker early only terminates the
current job's Pod and does not grant access to another job.

A Deployment update or node disruption can interrupt an active job. Apply
changes while the runner is idle, and rely on Forgejo's job timeout and retry
controls for recovery.

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
- Dynamic registration currently relies on Forgejo Runner's deprecated `register`
  command and must track upstream replacement APIs.
- There is always one waiting Pod; scale-to-zero requires a queue-aware
  controller.
- Unassigned runners that lose their Pod may leave stale offline entries.
- Generic NetworkPolicy cannot express Forgejo and binary-cache FQDN allowlists
  with the standard Kubernetes API.
- A single replica provides concurrency one, not high availability.
- Resource exhaustion inside a job can pressure the selected Kubernetes node;
  requests, limits, quotas, and monitoring remain required.
