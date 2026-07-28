# Forgejo Ephemeral Runner for Kubernetes

An experimental, small-footprint pattern that gives each Forgejo Actions job a
fresh Kubernetes Pod without implementing the Forgejo Actions protocol.

The project combines Forgejo Runner's official `--ephemeral` registration and
`one-job` command with a narrowly privileged reaper sidecar. A Deployment keeps
one idle runner available. After one job completes, the sidecar deletes its own
Pod and Kubernetes creates a clean replacement.

## Lifecycle

```text
Deployment (one replica)
  -> init container registers an ephemeral Forgejo Runner
  -> runner container waits for and executes exactly one job
  -> runner writes a completion marker
  -> reaper sidecar deletes its own Pod
  -> Deployment creates a fresh Pod
```

Forgejo 15 or newer is required for server-enforced ephemeral runners.

## Security properties

- The registration token is mounted only into the init container.
- The workflow container has no Kubernetes service account token.
- Only the reaper receives a projected service account token.
- Reaper RBAC is namespace-scoped to `get` and `delete` Pods.
- No host paths, privileged containers, or container-runtime sockets are used.
- The workflow runs as `host` from Forgejo's perspective, but that host is the
  disposable runner container, not the Kubernetes node.
- Runner state, workspace, home, temporary files, and the writable image layer
  disappear with the Pod.

A workflow can still read its ephemeral runner credential and can modify any
files available inside its own container. Forgejo's server-enforced ephemeral
mode prevents that credential from claiming another job. Workflows also retain
whatever network access the cluster policy permits.

See [docs/security.md](docs/security.md) for the complete threat model and
limitations.

## Development

Enter the pinned development environment and run all checks:

```sh
nix develop
just check
```

Build the two OCI archives on Linux:

```sh
nix build .#runner-image -o result-runner
nix build .#reaper-image -o result-reaper
```

The runner image contains Forgejo Runner, Nix, Git, OpenSSH, Node.js, and the
lifecycle scripts. The reaper image contains only the Go reaper binary.

## Deployment

`deploy/base` is an environment-neutral Kustomize base. Before deployment, an
overlay must:

1. replace both development image references with immutable registry digests;
2. replace `https://forgejo.example.com` with the intended Forgejo URL;
3. set the required runner label;
4. provide a Secret named `forgejo-runner-registration` with a `token` key;
5. review resource limits and network access for the target cluster.

Do not commit the plaintext registration token. Use the secret-management
system of the deployment repository.

Render and validate the base locally:

```sh
kustomize build deploy/base | kubeconform -strict -summary
```

## Project status

This is pre-release software. In particular, a Pod that disappears before it
receives a job can leave an offline ephemeral runner entry in Forgejo. Forgejo
removes an ephemeral runner after its assigned job completes or times out, but
an unassigned runner may remain until an administrator removes it. Operational
cleanup and failure-injection tests are required before production use.
The current dynamic-registration flow also depends on Forgejo Runner's deprecated
`register` command and must be adapted if upstream removes it.

A license will be selected before the first public release. Until then, no
license is granted beyond applicable law.
