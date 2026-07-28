# End-to-end tests

The end-to-end harness is opt-in. It creates a dedicated Kind cluster named
`forgejo-ephemeral-runner-e2e`; it never uses the current Kubernetes context.
All `kubectl` calls use the kubeconfig generated under the ignored `.e2e/`
directory.

The cluster contains:

- Forgejo 15.0.5 with an ephemeral SQLite database;
- an ephemeral administrator, API token, private repository, and workflow;
- the locally built controller and runner images;
- the environment-neutral deployment base with an E2E-only overlay.

The smoke scenario verifies that no runner Pod exists while the queue is empty,
one targeted runner Pod appears after `workflow_dispatch`, the workflow
succeeds without a Kubernetes service account token, and the runner Pod,
credential Secret, and Forgejo registration are removed afterward. A second
scenario dispatches two workflows, verifies that two isolated runner Pods exist
at the same time under `MAX_CONCURRENT=2`, and verifies complete cleanup after
both workflows succeed. A final scenario deletes the active controller Pod,
waits for a different replica to acquire the Lease, and verifies that a queued
workflow still runs and cleans up while the Deployment returns to two ready
replicas.

## Requirements

Use a Linux or macOS development host with a working Docker or Podman service.
The Nix development shell supplies Kind, kubectl, curl, jq, and the remaining
test tools. Approximately 4 GiB of memory is recommended. At least 10 GiB of free disk
space is required, and the preflight check rejects hosts below that threshold.
The first Forgejo image pull may take several minutes on a slow connection.
The harness downloads the exact pinned source digest, stores it as a checksummed
OCI archive under the ignored `.cache/e2e/` directory, and loads it into each
disposable Kind cluster before deployment. Test data, credentials, and cluster
state are still removed after each successful run; only the public Forgejo
image layers persist.

Check prerequisites without creating a cluster:

```sh
nix develop
just e2e-preflight
```

Run the destructive-to-the-disposable-environment test explicitly:

```sh
just e2e
```

A successful run removes the dedicated cluster and generated credentials. A
failed run retains the cluster for inspection and prints the cleanup command:

```sh
just e2e-clean
```

`just e2e-clean` keeps the image cache. Remove that cache explicitly when disk
space is needed or when diagnosing image acquisition:

```sh
just e2e-cache-clean
```

Set `E2E_KEEP_CLUSTER=true` to retain a successful cluster. Set
`E2E_CONTAINER_PROVIDER` to `docker` or `podman` to override automatic provider
selection. `E2E_FORGEJO_PORT` may select a different unprivileged localhost
port when the default `30080` is occupied.

For Podman, the harness gives Kind subprocesses an isolated home directory at
`.e2e/podman-home/`. Podman reads its generated signature policy from the
standard per-user location inside that directory and keeps its E2E image storage
there. The harness does not modify the real user home or system containers
configuration. The Kind node image remains digest-pinned.

The harness refuses to delete any Kind cluster whose name differs from the
fixed E2E name. It does not connect to an existing Forgejo instance, a live
Kubernetes cluster, or an operator deployment repository. Runtime credentials,
logs, image links, payloads, and kubeconfig data remain under `.e2e/` and are
not tracked by Git.
