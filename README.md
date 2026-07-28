# Forgejo Ephemeral Runner for Kubernetes

An experimental, small-footprint controller that gives each Forgejo Actions
job a fresh Kubernetes Pod without implementing the Forgejo Actions protocol.

The controller uses Forgejo 15's runner management API to create a
server-enforced ephemeral runner. It then starts the official Forgejo Runner
with `one-job`, watches the Kubernetes Pod, and removes both the Pod and its
one-job credential after completion.

## Lifecycle

```text
controller Deployment
  -> removes a stale registration with its deterministic managed name
  -> creates an ephemeral runner through the Forgejo API
  -> stores only that runner's UUID and one-job token in a temporary Secret
  -> creates one unprivileged runner Pod
  -> observes the Pod until Forgejo Runner exits
  -> deletes the Pod, Forgejo registration, and temporary Secret
  -> creates a fresh registration and Pod
```

The controller currently maintains one idle runner. Forgejo 15 or newer and
Forgejo Runner 12.5 or newer are required for API-created ephemeral runners and
direct `one-job` credentials.

## Security properties

- The long-lived Forgejo API token is mounted only into the controller.
- The runner Pod receives only its server-enforced ephemeral UUID and token.
- The runner Pod has no Kubernetes service account token.
- Controller RBAC is namespace-scoped to `get`, `create`, and `delete` Pods and
  Secrets.
- No host paths, privileged containers, or container-runtime sockets are used.
- The workflow runs as `host` from Forgejo's perspective, but that host is the
  disposable runner container, not the Kubernetes node.
- Runner workspace, home, temporary files, Nix store changes, and writable
  image layer disappear with the Pod.
- Stale cleanup requires the exact deterministic name, project description,
  and ephemeral flag; a colliding unmanaged runner is never deleted.

A workflow can still read its own one-job runner credential and can modify any
files inside its container. Forgejo's server-enforced ephemeral mode prevents
that credential from claiming another job. Workflows retain whatever network
access the cluster policy permits.

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
nix build .#controller-image -o result-controller
```

The runner image contains Forgejo Runner, Nix, Git, OpenSSH, Node.js, and the
`one-job` launcher. The controller image contains the Go controller and CA
certificates.

## Deployment

`deploy/base` is an environment-neutral Kustomize base. Before deployment, an
overlay must:

1. replace both development image references with immutable registry digests;
2. replace `https://forgejo.example.com` with the intended Forgejo URL;
3. select a runner scope;
4. set a deterministic runner name and required runner labels;
5. provide a Secret named `forgejo-runner-controller` with an `api-token` key;
6. review resource limits and network access for the target cluster.

Supported scope values are:

```text
user
instance
organization:<name>
repository:<owner>/<repository>
```

The API token's account must be allowed to create, list, and delete runners at
the selected scope. Use a dedicated account with the narrowest practical
Forgejo ownership and token permissions. Forgejo 15 may require a token with
broader repository administration access than the runner operations alone, so
review the token's effective permissions before deployment.

Do not commit the plaintext API token. Use the secret-management system of the
private deployment repository. The public base intentionally contains no
Secret value or environment-specific endpoint.

Render and validate the base locally:

```sh
kustomize build deploy/base | kubeconform -strict -summary
```

## Project status

This is pre-release software. The controller uses Forgejo's current runner API
and does not call the deprecated `forgejo-runner register` command or create a
`.runner` file.

There is an unavoidable transaction boundary between creating a Forgejo runner
and recording its ID in Kubernetes. On restart, the controller lists runners
at its exact scope and safely removes only ephemeral registrations bearing its
deterministic managed identity. Failure-injection tests against a disposable
Forgejo instance are still required before production use.

A license will be selected before the first public release. Until then, no
license is granted beyond applicable law.
