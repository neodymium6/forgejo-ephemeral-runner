#!/usr/bin/env bash

set -euo pipefail

fail() {
  echo "error: $*" >&2
  exit 1
}

[[ ${FORGEJO_ACTIONS:-} == true ]] || fail "this check must run inside Forgejo Actions"
[[ $(id -u) -eq 65532 ]] || fail "runner UID must be 65532"
[[ $(id -g) -eq 65532 ]] || fail "runner GID must be 65532"
[[ -x /usr/bin/env ]] || fail "/usr/bin/env is unavailable"
[[ -w /nix/store ]] || fail "/nix/store is not writable by the runner"
[[ -w /nix/var/nix ]] || fail "/nix/var/nix is not writable by the runner"
[[ ! -e /homeless-shelter ]] || fail "/homeless-shelter already exists"

if mkdir /homeless-shelter 2>/dev/null; then
  fail "the runner can create /homeless-shelter"
fi

nix build .#controller --no-link
nix build .#release-tools --no-link
