#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf "${test_dir}"' EXIT

mkdir -p "${test_dir}/bin" "${test_dir}/secrets"
printf '%s' 'runner-uuid' > "${test_dir}/secrets/uuid"
printf '%s' 'runner-token' > "${test_dir}/secrets/token"
printf '%s' 'job-handle' > "${test_dir}/secrets/handle"

cat > "${test_dir}/bin/forgejo-runner" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "${FORGEJO_RUNNER_TEST_ARGS:?}"
EOF
chmod +x "${test_dir}/bin/forgejo-runner"

args_file="${test_dir}/args"
PATH="${test_dir}/bin:${PATH}" \
  FORGEJO_RUNNER_TEST_ARGS="${args_file}" \
  FORGEJO_INSTANCE_URL="https://forgejo.example.com" \
  FORGEJO_RUNNER_LABELS="linux-amd64:host,nix" \
  FORGEJO_RUNNER_UUID_FILE="${test_dir}/secrets/uuid" \
  FORGEJO_RUNNER_TOKEN_FILE="${test_dir}/secrets/token" \
  FORGEJO_JOB_HANDLE_FILE="${test_dir}/secrets/handle" \
  bash "${project_dir}/scripts/run-one-job.sh"

mapfile -t args < "${args_file}"
expected=(
  --config /etc/forgejo-runner/config.yaml
  one-job
  --url https://forgejo.example.com
  --uuid runner-uuid
  --token-url "file://${test_dir}/secrets/token"
  --wait
  --handle job-handle
  --label linux-amd64:host
  --label nix
)

if [[ "${args[*]}" != "${expected[*]}" ]]; then
  printf 'unexpected arguments:\n' >&2
  printf '  %q\n' "${args[@]}" >&2
  exit 1
fi

missing_handle_error="${test_dir}/missing-handle-error"
if PATH="${test_dir}/bin:${PATH}" \
  FORGEJO_RUNNER_TEST_ARGS="${test_dir}/args-without-handle" \
  FORGEJO_INSTANCE_URL="https://forgejo.example.com" \
  FORGEJO_RUNNER_LABELS="linux-amd64:host" \
  FORGEJO_RUNNER_UUID_FILE="${test_dir}/secrets/uuid" \
  FORGEJO_RUNNER_TOKEN_FILE="${test_dir}/secrets/token" \
  FORGEJO_JOB_HANDLE_FILE="${test_dir}/secrets/missing" \
  bash "${project_dir}/scripts/run-one-job.sh" 2>"${missing_handle_error}"; then
  echo "runner accepted a missing job handle file" >&2
  exit 1
fi

if ! grep -Fxq -- 'job handle file is missing or empty' "${missing_handle_error}"; then
  echo "runner returned an unexpected missing-handle error" >&2
  exit 1
fi
