#!/usr/bin/env bash
set -euo pipefail

config_file="${FORGEJO_RUNNER_CONFIG:-/etc/forgejo-runner/config.yaml}"
state_dir="${FORGEJO_RUNNER_STATE_DIR:-/var/lib/forgejo-runner}"
token_file="${FORGEJO_REGISTRATION_TOKEN_FILE:-/run/secrets/forgejo-registration/token}"
instance_url="${FORGEJO_INSTANCE_URL:?FORGEJO_INSTANCE_URL is required}"
labels="${FORGEJO_RUNNER_LABELS:?FORGEJO_RUNNER_LABELS is required}"
pod_name="${POD_NAME:?POD_NAME is required}"
name_prefix="${FORGEJO_RUNNER_NAME_PREFIX:-kubernetes-ephemeral}"
runner_file="${state_dir}/.runner"

if [[ -e "${runner_file}" ]]; then
  echo "refusing to overwrite an existing runner registration" >&2
  exit 1
fi
if [[ ! -s "${token_file}" ]]; then
  echo "registration token file is missing or empty" >&2
  exit 1
fi

install -d -m 0700 "${state_dir}"
registration_token="$(tr -d '\r\n' < "${token_file}")"
if [[ -z "${registration_token}" ]]; then
  echo "registration token is empty after trimming line endings" >&2
  exit 1
fi

cd "${state_dir}" || exit 1
umask 0077
exec forgejo-runner --config "${config_file}" register \
  --no-interactive \
  --ephemeral \
  --instance "${instance_url}" \
  --token "${registration_token}" \
  --name "${name_prefix}-${pod_name}" \
  --labels "${labels}"
