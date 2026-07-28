#!/usr/bin/env bash
set -euo pipefail

config_file="${FORGEJO_RUNNER_CONFIG:-/etc/forgejo-runner/config.yaml}"
state_dir="${FORGEJO_RUNNER_STATE_DIR:-/var/lib/forgejo-runner}"
completion_file="${COMPLETION_FILE:-/var/run/forgejo-ephemeral-runner/completed}"
runner_file="${state_dir}/.runner"

if [[ ! -s "${runner_file}" ]]; then
  echo "runner registration file is missing or empty" >&2
  exit 1
fi

install -d -m 0755 "$(dirname "${completion_file}")"
rm -f "${completion_file}"

mark_complete() {
  status=$?
  trap - EXIT
  printf '%s\n' "${status}" > "${completion_file}"
  exit "${status}"
}
trap mark_complete EXIT

cd "${state_dir}" || exit 1
forgejo-runner --config "${config_file}" one-job --wait
