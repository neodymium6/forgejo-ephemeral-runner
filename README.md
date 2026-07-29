# Forgejo Ephemeral Runner for Kubernetes

A pre-release, small-footprint controller that gives each Forgejo Actions job
a fresh Kubernetes Pod without implementing the Forgejo Actions protocol.

The controller polls Forgejo 15's jobs API, creates a server-enforced ephemeral
runner for each matching waiting job, and starts the official Forgejo Runner
with `one-job --handle`. It scales runner Pods to zero when no job is waiting
and supports a configurable concurrency limit.

## Lifecycle

```text
two-replica controller Deployment
  -> competes for a Kubernetes Lease; only the holder reconciles
  -> lists matching waiting jobs at the configured Forgejo scope
  -> keeps zero runner Pods when no job is waiting
  -> assigns free fixed slots up to MAX_CONCURRENT
  -> creates one ephemeral Forgejo runner for the selected job
  -> stores its UUID, one-job token, and target handle in a temporary Secret
  -> creates one unprivileged runner Pod
  -> runs exactly the selected job with one-job --handle
  -> removes the Pod, Forgejo registration, and temporary Secret
```

`MAX_CONCURRENT` defaults to `1` and accepts values from `1` through `10`.
Stalled `Pending` and `Unknown` Pods are recovered after configurable, positive
timeouts; the base defaults to 30 minutes and 5 minutes respectively. The
current development target is Forgejo 15. The disposable test fixture pins
Forgejo 15.0.5, and the pinned Nixpkgs input currently supplies Forgejo Runner
12.13.1. Other versions are not yet part of the tested compatibility surface.

## Security properties

- The long-lived Forgejo API token is mounted only into the controller replicas.
- The Kubernetes service account token is sent only to an HTTPS API unless an
  operator explicitly enables the insecure HTTP test override.
- A runner Pod receives only one ephemeral UUID and token plus its target job
  handle.
- The runner Pod has no Kubernetes service account token.
- Controller access to existing Pods and Secrets is restricted to ten fixed
  slot names.
- Kubernetes Lease leader election permits only one active reconciler under
  normal API-server and `resourceVersion` semantics.
- No host paths, privileged containers, or container-runtime sockets are used.
- The controller strictly rejects unmodeled runner Pod fields and templates that
  change the reviewed image, launcher, environment sources, service account,
  containers, security context, volumes, or mounts.
- The base namespace carries labels requesting Kubernetes Pod Security
  Baseline enforcement.
- The workflow runs as `host` from Forgejo's perspective, but that host is the
  disposable runner container, not the Kubernetes node.
- Runner workspace, home, temporary files, Nix store changes, and writable
  image layer disappear with the Pod.
- Stale cleanup requires the exact deterministic name, project description,
  and ephemeral flag; a colliding unmanaged runner is never deleted.

A workflow can still read its own one-job runner credential and can modify any
files inside its container. Forgejo's server-enforced ephemeral mode and the
target handle limit it to the assigned job. Workflows retain whatever network
access the cluster policy permits.

See [docs/security.md](docs/security.md) for the complete threat model and
limitations.

## Development

Enter the pinned development environment and run all checks:

```sh
nix develop
just check
```

The Forgejo workflow in `.forgejo/workflows/ci.yaml` runs `just check` on pushes
to `main` and on manual dispatch. It verifies formatting, unit and race tests,
the launcher and E2E helper tests, static analysis, manifest rendering, and Nix
flake evaluation.

The full end-to-end harness is deliberately separate from default CI. It needs
a Docker or Podman service to create a fixed, disposable Kind cluster and is
not intended to run inside the unprivileged runner Pod. See
[docs/e2e.md](docs/e2e.md) before invoking it on an isolated development host:

```sh
just e2e
```

Build the two OCI archives on Linux:

```sh
nix build .#runner-image -o result-runner
nix build .#controller-image -o result-controller
```

The runner image contains Forgejo Runner, Nix, Git, OpenSSH, Node.js, and the
`one-job` launcher. It also provides the conventional `/usr/bin/env` path from
Nixpkgs coreutils for Actions that use `#!/usr/bin/env` entrypoints. The
controller image contains the Go controller and CA certificates.

Image publication is not automated yet. Build, publish, inspect, and pin both
images by registry digest before changing a deployment overlay. The development
tag `0.2.0-dev` is not an immutable deployment reference.

## Deployment

`deploy/base` is an environment-neutral Kustomize base. Before deployment, an
overlay must:

1. replace both development image references with immutable registry digests;
2. replace `https://forgejo.example.com` with the intended Forgejo URL;
3. select a runner scope;
4. set a deterministic runner name and required runner labels;
5. set `MAX_CONCURRENT` from `1` through `10`;
6. review `RUNNER_STARTUP_TIMEOUT` and `RUNNER_UNKNOWN_TIMEOUT`;
7. provide a Secret named `forgejo-runner-controller` with an `api-token` key;
8. review resource limits and network access for the target cluster.

Supported scope values are:

```text
user
instance
organization:<name>
repository:<owner>/<repository>
```

The token account must be allowed to list jobs and create, list, and delete
runners at the selected scope. Forgejo 15 exposes route-level token scopes, not
a runner-only permission:

| Runner scope | Required route-level token scope |
| --- | --- |
| `user` | `write:user` |
| `organization:<name>` | `write:organization` |
| `repository:<owner>/<repository>` | `write:repository` |
| `instance` | `write:admin` |

Account ownership or administration checks still apply in addition to the
token scope. Prefer a dedicated account when Forgejo permits it for the
selected scope. On Forgejo 15, repository-scoped runner APIs for a personal
repository require the repository owner; an administrative collaborator is
insufficient. Such a repository therefore needs an owner-issued
`write:repository` token. Moving the repository to an organization is the
practical path to separating the runner identity from a personal owner. See
the security document before choosing an account and token type.

The base reserves ten deterministic Pod, Secret, and runner names so that
read/delete RBAC can remain name-restricted. If their base names or the Lease
name change, update the corresponding `resourceNames` in the Role in the same
overlay. Do not lower `MAX_CONCURRENT` while a higher-numbered slot is active;
wait for those jobs and their cleanup to finish first.

Do not commit the plaintext API token. Use the secret-management system of the
private deployment repository. The public base intentionally contains no
Secret value or environment-specific endpoint.

Render and validate the base locally:

```sh
kustomize build deploy/base | kubeconform -strict -summary
```

## Project status

This is pre-release software at version `0.2.0-dev`. The controller uses
Forgejo's current runner API and does not call the deprecated
`forgejo-runner register` command or create a `.runner` file. The default CI
workflow runs the non-destructive `just check` suite; the disposable Kind E2E
harness has passed on a local Podman development host, but remains an explicit
operator test and is not a release gate.

Creating a Forgejo runner and recording its ID in Kubernetes cannot be one
atomic transaction. On restart, the active controller lists runners at its
exact scope and safely removes only ephemeral registrations bearing its
deterministic managed identity. Unit tests exercise this recovery path, and a
successful local run of the disposable Forgejo E2E harness exercised the
corresponding failure-injection scenario.

The project source is licensed under the
[Apache License 2.0](LICENSE).
