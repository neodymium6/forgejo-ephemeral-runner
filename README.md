# Forgejo Ephemeral Runner for Kubernetes

A small, early-stage controller that gives each Forgejo Actions job
a fresh Kubernetes Pod without implementing the Forgejo Actions protocol.

The controller polls Forgejo 15's jobs API, creates a server-enforced ephemeral
runner for each matching waiting job, and starts Forgejo Runner with a small
[lifecycle patch](docs/runner-lifecycle.md) using `one-job --handle`. It scales
runner Pods to zero when no job is waiting
and supports a configurable concurrency limit.

## Lifecycle

```text
two-replica controller Deployment
  -> competes for a Kubernetes Lease; only the holder reconciles
  -> lists label-matching waiting jobs in the allowed repositories
  -> keeps zero runner Pods when no job is waiting
  -> assigns free fixed slots up to MAX_CONCURRENT
  -> creates one ephemeral Forgejo runner in the selected repository scope
  -> stores its scope, UUID, one-job token, and target handle in a temporary Secret
  -> creates one unprivileged runner Pod
  -> runs exactly the selected job with one-job --handle
  -> removes the Pod, Forgejo registration, and temporary Secret
```

`MAX_CONCURRENT` defaults to `1` and accepts values from `1` through `10`.
Stalled `Pending` and `Unknown` Pods are recovered after configurable, positive
timeouts; the base defaults to 30 minutes and 5 minutes respectively. The runner
also stops idle polling after five minutes once Forgejo confirms an
empty task response. This recovers reservations cancelled before assignment
without imposing a five-minute limit on running jobs. Ambiguous fetch failures
continue retrying with the same request key until assignment is resolved.
The current development target is Forgejo 15. The disposable test fixture pins
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
- Workflow processes and single-user Nix run as the dedicated non-root UID and
  GID 65532.
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

## Observability

Every controller replica provides a fixed, low-cardinality Prometheus endpoint
at `:9090/metrics` by default. Metrics describe leadership, runner capacity,
queue observations, reconciliation health, bounded mutation operations, and
cleanup reasons. They do not label repository names, job identifiers, runner
identifiers, URLs, tokens, or Kubernetes object UIDs.

The base declares the container port but creates no monitoring Service and its
default-deny ingress policy remains in force. Create Prometheus discovery,
the narrow ingress allow rule, Grafana dashboards, and alerts in the private
infrastructure overlay. See [docs/metrics.md](docs/metrics.md) for the schema,
PromQL examples, and the privacy boundary.

## Development

Enter the pinned development environment and run all checks:

```sh
nix develop
just check
```

The Forgejo workflow in `.forgejo/workflows/ci.yaml` runs `just check` on pushes
to `main` and on manual dispatch. The GitHub workflow in
`.github/workflows/ci.yaml` runs the same checks on pushes to `main`, pull
requests, and manual dispatch. They verify formatting, unit and race tests, the
launcher and E2E helper tests, static analysis, manifest rendering, and Nix
flake evaluation. Forgejo CI first verifies the non-root runner identity,
private Nix store access, inability to create `/homeless-shelter`, and two
consecutive Nix builds.

Both CI workflows also build and test the patched runner with
`nix build .#forgejo-runner --no-link`. This is a separate, potentially expensive
check; it is not part of `just check`. Custom workflow images must include this
patched binary to receive the idle-wait fix.

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

Pushing a protected semantic-version tag such as `v0.2.0` runs the release
workflow at that Git remote. Forgejo publishes both images to its OCI registry
and creates a Forgejo Release. GitHub publishes both images to GHCR and creates
a GitHub Release. Both paths refuse to overwrite different image content and
attach an image manifest with immutable digests. See
[docs/releasing.md](docs/releasing.md) for setup and release procedures.

Release creation does not update a Kubernetes deployment. An operator must
review the published manifest and explicitly pin its digests in a private
deployment overlay.

## Deployment

`deploy/base` is an environment-neutral Kustomize base. Before deployment, an
overlay must:

1. replace both development image references with immutable registry digests;
2. replace `https://forgejo.example.com` with the intended Forgejo URL;
3. set an explicit repository allowlist or the explicit `*` wildcard;
4. set a deterministic runner name and required runner labels;
5. set `MAX_CONCURRENT` from `1` through `10`;
6. review `RUNNER_STARTUP_TIMEOUT` and `RUNNER_UNKNOWN_TIMEOUT`;
7. review `METRICS_LISTEN_ADDRESS` and add narrowly scoped monitoring access
   if used;
8. provide a Secret named `forgejo-runner-controller` with an `api-token` key;
9. review resource limits and network access for the target cluster.

The allowlist is a newline-delimited string. YAML's `|` block scalar keeps the
lines in one ConfigMap value:

```yaml
FORGEJO_REPOSITORY_ALLOWLIST: |
  example/first
  example/second
```

Each exact entry uses a repository-scoped runner endpoint. One API token is
shared by the controller across every allowed repository and normally needs
`write:repository`. The repository owner and token restrictions remain an
independent upper bound if the controller is compromised.

For trusted personal installations, an operator may instead opt in to every
repository eligible for the token's user runner scope:

```yaml
FORGEJO_REPOSITORY_ALLOWLIST: "*"
```

The wildcard must be the only entry, uses Forgejo's user-scoped runner
endpoint, and requires `write:user`. It is not merely a glob over the explicit
list. In both modes, a job must also request one of `FORGEJO_RUNNER_LABELS`
through the standard workflow `runs-on` field. Labels select a runner; the
explicit list is the authorization policy. With `*`, the label intentionally
becomes the practical repository opt-in.

`FORGEJO_REPOSITORY_ALLOWLIST` replaces `FORGEJO_RUNNER_SCOPE`. Before upgrading
an existing deployment, wait for all runner Pods and temporary credentials to
drain; older temporary Secrets do not contain the scope metadata required by
the new cleanup path.


Account ownership or administration checks still apply in addition to the
token scope. Prefer a dedicated account when Forgejo permits it. On Forgejo 15,
repository-scoped runner APIs for a personal repository require the repository
owner; an administrative collaborator is insufficient. Such a repository
therefore needs an owner-issued `write:repository` token. Moving the repository
to an organization is the practical path to separating the runner identity
from a personal owner. See the security document before choosing an account
and token type.

The base reserves ten deterministic Pod, Secret, and runner names so that
read/delete RBAC can remain name-restricted. If their base names or the Lease
name change, update the corresponding `resourceNames` in the Role in the same
overlay. Do not lower `MAX_CONCURRENT` while a higher-numbered slot is active;
wait for those jobs and their cleanup to finish first.

Likewise, wait until all runner Pods and temporary credentials have drained
before removing an allowlist entry or switching between exact entries and `*`.
The controller fails closed rather than discarding state for a removed scope.

Do not commit the plaintext API token. Use the secret-management system of the
private deployment repository. The public base intentionally contains no
Secret value or environment-specific endpoint.

Render and validate the base locally:

```sh
kustomize build deploy/base | kubeconform -strict -summary
```

## Project status

This is early-stage software at version `0.4.0`. The controller uses
Forgejo's current runner API and does not call the deprecated
`forgejo-runner register` command or create a `.runner` file. The default CI
workflow runs the non-destructive `just check` suite; the disposable Kind E2E
harness has passed on a local Podman development host, but remains an explicit
operator test and is not a release gate.

Creating a Forgejo runner and recording its ID in Kubernetes cannot be one
atomic transaction. On restart, the active controller lists runners at its
configured allowlist scopes and safely removes only ephemeral registrations
bearing its deterministic managed identity. Unit tests exercise this recovery
path, and a successful local run of the disposable Forgejo E2E harness
exercised the
corresponding failure-injection scenario.

The controller project source is licensed under the
[Apache License 2.0](LICENSE). The upstream runner patch retains the
[upstream licenses](docs/runner-lifecycle.md#upstream-references-and-licensing).
