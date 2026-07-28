#!/usr/bin/env bash
set -euo pipefail

config_file="${FORGEJO_RUNNER_CONFIG:-/etc/forgejo-runner/config.yaml}"
uuid_file="${FORGEJO_RUNNER_UUID_FILE:-/run/secrets/forgejo-runner/uuid}"
token_file="${FORGEJO_RUNNER_TOKEN_FILE:-/run/secrets/forgejo-runner/token}"
handle_file="${FORGEJO_JOB_HANDLE_FILE:-/run/secrets/forgejo-runner/handle}"
instance_url="${FORGEJO_INSTANCE_URL:?FORGEJO_INSTANCE_URL is required}"
labels="${FORGEJO_RUNNER_LABELS:?FORGEJO_RUNNER_LABELS is required}"

if [[ ! -s "${uuid_file}" ]]; then
  echo "runner UUID file is missing or empty" >&2
  exit 1
fi
if [[ ! -s "${token_file}" ]]; then
  echo "runner token file is missing or empty" >&2
  exit 1
fi

uuid="$(tr -d '\r\n' < "${uuid_file}")"
if [[ -z "${uuid}" ]]; then
  echo "runner UUID is empty after trimming line endings" >&2
  exit 1
fi

if [[ ! -s "${handle_file}" ]]; then
  echo "job handle file is missing or empty" >&2
  exit 1
fi
handle="$(tr -d '\r\n' < "${handle_file}")"
if [[ -z "${handle}" ]]; then
  echo "job handle is empty after trimming line endings" >&2
  exit 1
fi

args=(
  --config "${config_file}"
  one-job
  --url "${instance_url}"
  --uuid "${uuid}"
  --token-url "file://${token_file}"
  --wait
  --handle "${handle}"
)

IFS=',' read -r -a runner_labels <<< "${labels}"
for label in "${runner_labels[@]}"; do
  label="${label#"${label%%[![:space:]]*}"}"
  label="${label%"${label##*[![:space:]]}"}"
  if [[ -z "${label}" ]]; then
    echo "FORGEJO_RUNNER_LABELS contains an empty label" >&2
    exit 1
  fi
  args+=(--label "${label}")
done

exec forgejo-runner "${args[@]}"
