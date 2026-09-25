# Runner cancellation and idle reservations

The project applies `patches/forgejo-runner-idle-timeout.patch` and
`patches/forgejo-runner-revoked-credential.patch` to the pinned Forgejo Runner
12.13.1 package. The runner image and its one-job launcher both
use that patched binary. The development shell keeps the stock runner for
workflow validation, so entering the shell does not build the patched runner.
Custom workflow images must use `packages.<system>.forgejo-runner` (for example,
`nix build .#forgejo-runner`) or apply both patches. Updating only the
controller or copying only the launcher into a stock image does not fix idle
polling.

## Before assignment

Forgejo 15's runner jobs API returns only waiting and running jobs. A missing
handle cannot distinguish cancellation from completion, filtering, or an
incomplete response. Its public REST API does not provide the terminal state
of a reservation by handle. The controller therefore never deletes a Running
Pod based on this list, a runner's reported idle status, or Pod age alone.

Instead, targeted one-job polling has a five-minute idle budget, starting when
the poller starts. `FORGEJO_RUNNER_IDLE_TIMEOUT` can override it with a positive
Go duration such as `10m` in the shared settings ConfigMap. The deadline applies
only to obtaining a task; it is not a workflow execution timeout.

After the budget expires, a successful empty `FetchSingleTask` response causes
the runner to exit with `ErrNoTaskReceived`. The controller observes the terminal
Pod and performs its existing UID-checked cleanup: Pod first, registration
second, temporary Secret last. The next waiting job can then use the slot.
This also recycles reservations that are still waiting on dependencies; they
remain eligible for a later reservation.

A task in the response always takes precedence over the deadline. No timer
cancels the fetch or the running task. If the server assigned a task but its
response was lost, the runner preserves the request key and retries until it
can recover that task or receive a successful empty response. Transport errors,
timeouts, malformed responses, and generic authentication failures do not prove
that a runner is idle. Consequently, a continuing API outage can extend the
idle budget: safe recovery cannot have an unconditional wall-clock bound when
assignment is ambiguous. Existing per-request fetch timeouts still apply.

## During execution

The runner already cancels the local task when `UpdateTask` returns cancelled
or failed state. The patch also cancels it when `UpdateTask` or `UpdateLog`
returns Forgejo's explicit `unauthenticated: unregistered runner` RPC error.
That response means the one-job credential can no longer report or recover its
task. Generic HTTP authentication errors and transient API errors retain the
existing retry behavior.

Final logs and status are attempted through the existing reporter. An explicit
unregistered-runner response stops final-report retries, since that credential
cannot complete them. Local workload cancellation uses the runner's existing
task cancellation path. Once the runner exits, container termination and
controller cleanup release the slot. The controller does not revoke a live
reservation to force this process, and the patch does not establish which
component removed a registration in a past incident.

## Monitoring and validation

Queue observations continue at full capacity. The aggregate
`forgejo_ephemeral_runner_reservations_unmatched` gauge reports active
reservations missing from the observed job list. Use it with queue-observation
health and sustained-duration alerts; it is not proof of cancellation. See
[metrics.md](metrics.md).

The patch carries runner unit tests for empty polling, assignment at the idle
deadline, recovery of an ambiguous fetch with the same request key, transient
errors, explicit revocation during polling/reporting, invalid configuration,
and final-report retry termination. Controller tests cover observation at full
capacity and preservation of live Pods across missing/incomplete lists, API
errors, and fresh controller state. Existing UID-precondition and leader-election
tests continue to apply.

CI builds the patched runner and runs the Nixpkgs package's unit tests with
`nix build .#forgejo-runner --no-link`. This can be expensive and is separate
from `just check`. The disposable Kind E2E harness remains opt-in; mock tests
do not replace a controlled cancellation test against a real Forgejo instance.

## Upstream references and licensing

- [Forgejo 15.0.5 jobs API](https://codeberg.org/forgejo/forgejo/src/tag/v15.0.5/routers/api/v1/shared/runners.go)
- [Forgejo 15.0.5 task acquisition and reporting](https://codeberg.org/forgejo/forgejo/src/tag/v15.0.5/routers/api/actions/runner/runner.go)
- [Forgejo 15.0.5 runner authentication](https://codeberg.org/forgejo/forgejo/src/tag/v15.0.5/routers/api/actions/runner/interceptor.go)
- [Runner 12.13.1 single-task poller](https://code.forgejo.org/forgejo/runner/src/tag/v12.13.1/internal/app/poll/single.go)
- [Runner 12.13.1 reporter](https://code.forgejo.org/forgejo/runner/src/tag/v12.13.1/internal/pkg/report/reporter.go)

The patches preserve the upstream licensing: changes to the single-task poller
and its new tests are GPL-3.0-or-later; the client helper, reporter changes, and
reporter tests are MIT. These patch portions are exceptions to the controller
repository's Apache-2.0 license. The built Forgejo Runner remains GPL-3.0-or-later.
The exact upstream revision and patches are public so the modified runner source
can be reconstructed. Review patch applicability and regression tests whenever
the pinned upstream runner changes.
