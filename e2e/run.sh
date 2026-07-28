#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=e2e/lib.sh
source "${script_dir}/lib.sh"

forgejo_namespace="forgejo-e2e"
runner_namespace="forgejo-ephemeral-runner"
forgejo_port="${E2E_FORGEJO_PORT:-30080}"
forgejo_url="http://127.0.0.1:${forgejo_port}"
api_user="e2e-admin"
repository="e2e-repository"
port_forward_pid=""
e2e_completed=false

e2e_stop_port_forward() {
  if [[ -n "${port_forward_pid}" ]] && kill -0 "${port_forward_pid}" 2>/dev/null; then
    kill "${port_forward_pid}" 2>/dev/null || true
    wait "${port_forward_pid}" 2>/dev/null || true
  fi
}

e2e_on_exit() {
  local status=$?
  e2e_stop_port_forward
  if [[ "${e2e_completed}" != true && -n "${provider:-}" ]] &&
    e2e_cluster_exists "${provider}"; then
    printf '%s\n' \
      'E2E failed; the dedicated cluster was retained for inspection.' \
      "Run 'just e2e-clean' when finished." >&2
  fi
  return "${status}"
}
trap e2e_on_exit EXIT

e2e_wait_for() {
  local description="$1"
  local timeout_seconds="$2"
  shift 2
  local deadline=$((SECONDS + timeout_seconds))
  until "$@"; do
    if ((SECONDS >= deadline)); then
      e2e_die "timed out waiting for ${description}"
      return 1
    fi
    sleep 2
  done
}

e2e_forgejo_ready() {
  curl --fail --silent --show-error "${forgejo_url}/api/healthz" >/dev/null
}

e2e_api() {
  local path="$1"
  shift
  curl --config "${e2e_curl_config}" "$@" "${forgejo_url}${path}"
}

e2e_runner_pod_count() {
  e2e_kubectl get pods \
    --namespace "${runner_namespace}" \
    --selector app.kubernetes.io/component=job \
    --output name | wc -l | tr -d '[:space:]'
}

e2e_runner_pod_present() {
  [[ "$(e2e_runner_pod_count)" == 1 ]]
}

e2e_runner_pod_count_is() {
  local expected="$1"
  [[ "$(e2e_runner_pod_count)" == "${expected}" ]]
}

e2e_successful_run_count_at_least() {
  local expected="$1"
  local response
  response="$(e2e_api "/api/v1/repos/${api_user}/${repository}/actions/runs?limit=10")" || return
  local successful
  successful="$(jq '
    [
      .workflow_runs[] |
      select(
        (.status == "success") or
        (.status == "completed" and .conclusion == "success")
      )
    ] | length
  ' <<<"${response}")" || return
  ((successful >= expected))
}

e2e_dispatch_workflow() {
  e2e_api "/api/v1/repos/${api_user}/${repository}/actions/workflows/e2e.yaml/dispatches" \
    --request POST \
    --data-binary '{"ref":"main"}' \
    >/dev/null
}

e2e_resources_cleaned() {
  [[ "$(e2e_runner_pod_count)" == 0 ]] || return
  [[ "$(e2e_kubectl get secrets \
    --namespace "${runner_namespace}" \
    --selector app.kubernetes.io/managed-by=forgejo-ephemeral-runner \
    --output name | wc -l | tr -d '[:space:]')" == 0 ]] || return

  local response
  response="$(e2e_api "/api/v1/repos/${api_user}/${repository}/actions/runners?visible=false")" ||
    return
  jq -e '[.[] | select(.name | startswith("e2e-ephemeral-"))] | length == 0' \
    <<<"${response}" >/dev/null
}

case "${forgejo_port}" in
'' | *[!0-9]*) e2e_die "E2E_FORGEJO_PORT must be an integer"; exit 1 ;;
esac
if ((forgejo_port < 1024 || forgejo_port > 65535)); then
  e2e_die "E2E_FORGEJO_PORT must be between 1024 and 65535"
  exit 1
fi

e2e_require_command base64 curl df jq kind kubectl kustomize nix
e2e_assert_free_disk
provider="$(e2e_select_provider)"
if e2e_cluster_exists "${provider}"; then
  e2e_die "dedicated E2E cluster already exists; run 'just e2e-clean' first"
  exit 1
fi

umask 077
mkdir -p "${e2e_state_dir}"
printf '%s\n' "${provider}" >"${e2e_provider_file}"
if [[ "${provider}" == podman ]]; then
  e2e_prepare_podman_home
fi

printf 'Creating dedicated Kind cluster %s with %s.\n' \
  "${e2e_cluster_name}" "${provider}"
e2e_kind "${provider}" create cluster \
  --name "${e2e_cluster_name}" \
  --kubeconfig "${e2e_kubeconfig}" \
  --wait 120s

e2e_kubectl apply --kustomize "${script_dir}/manifests/forgejo"
e2e_kubectl rollout status \
  --namespace "${forgejo_namespace}" \
  deployment/forgejo \
  --timeout 900s

e2e_kubectl --namespace "${forgejo_namespace}" port-forward \
  service/forgejo "${forgejo_port}:3000" \
  >"${e2e_state_dir}/port-forward.log" 2>&1 &
port_forward_pid=$!
e2e_wait_for 'Forgejo health endpoint' 60 e2e_forgejo_ready

admin_password="$(head -c 24 /dev/urandom | base64 | tr -d '\n')"
e2e_kubectl exec \
  --namespace "${forgejo_namespace}" \
  deployment/forgejo \
  -- su-exec git forgejo admin user create \
  --username "${api_user}" \
  --password "${admin_password}" \
  --email e2e-admin@example.invalid \
  --admin \
  --must-change-password=false

api_token="$(e2e_kubectl exec \
  --namespace "${forgejo_namespace}" \
  deployment/forgejo \
  -- su-exec git forgejo admin user generate-access-token \
  --username "${api_user}" \
  --token-name ephemeral-runner-e2e \
  --scopes write:repository,write:user \
  --raw | tail -n 1)"
if [[ ! "${api_token}" =~ ^[A-Za-z0-9_-]+$ ]]; then
  e2e_die 'Forgejo returned an unexpected API token format'
  exit 1
fi

e2e_token_file="${e2e_state_dir}/api-token"
e2e_curl_config="${e2e_state_dir}/curl.conf"
printf '%s' "${api_token}" >"${e2e_token_file}"
printf '%s\n' \
  'silent' \
  'show-error' \
  'fail-with-body' \
  'header = "Accept: application/json"' \
  'header = "Content-Type: application/json"' \
  "header = \"Authorization: token ${api_token}\"" >"${e2e_curl_config}"
unset api_token admin_password

e2e_api '/api/v1/user/repos' \
  --request POST \
  --data-binary \
  "{\"name\":\"${repository}\",\"private\":true,\"auto_init\":true,\"default_branch\":\"main\"}" \
  >/dev/null

workflow_content="$(base64 \
  "${script_dir}/fixtures/repository/.forgejo/workflows/e2e.yaml" | tr -d '\n')"
jq --null-input \
  --arg content "${workflow_content}" \
  '{content: $content, message: "Add E2E workflow"}' \
  >"${e2e_state_dir}/workflow-request.json"
unset workflow_content
e2e_api "/api/v1/repos/${api_user}/${repository}/contents/.forgejo/workflows/e2e.yaml" \
  --request POST \
  --data-binary "@${e2e_state_dir}/workflow-request.json" \
  >/dev/null

printf '%s\n' 'Building and loading controller and runner images.'
nix build .#runner-image --out-link "${e2e_state_dir}/runner-image"
nix build .#controller-image --out-link "${e2e_state_dir}/controller-image"
e2e_kind "${provider}" load image-archive \
  --name "${e2e_cluster_name}" "${e2e_state_dir}/runner-image"
e2e_kind "${provider}" load image-archive \
  --name "${e2e_cluster_name}" "${e2e_state_dir}/controller-image"

e2e_kubectl apply --filename "${e2e_repository_root}/deploy/base/namespace.yaml"
e2e_kubectl create secret generic forgejo-runner-controller \
  --namespace "${runner_namespace}" \
  --from-file "api-token=${e2e_token_file}" \
  --dry-run=client \
  --output yaml | e2e_kubectl apply --filename -
e2e_kubectl apply --kustomize "${script_dir}/manifests/controller"
e2e_kubectl rollout status \
  --namespace "${runner_namespace}" \
  deployment/forgejo-ephemeral-runner-controller \
  --timeout 120s

if [[ "$(e2e_runner_pod_count)" != 0 ]]; then
  e2e_die 'runner did not scale to zero before workflow dispatch'
  exit 1
fi

printf '%s\n' 'Dispatching the smoke workflow.'
e2e_dispatch_workflow

e2e_wait_for 'one runner Pod' 120 e2e_runner_pod_present
e2e_wait_for 'one successful Forgejo Actions run' 180 e2e_successful_run_count_at_least 1
e2e_wait_for 'runner Pod, credential, and registration cleanup' 120 e2e_resources_cleaned

printf '%s\n' 'E2E smoke lifecycle succeeded.'

printf '%s\n' 'Dispatching two concurrent workflows.'
e2e_dispatch_workflow
e2e_dispatch_workflow
e2e_wait_for 'two concurrent runner Pods' 120 e2e_runner_pod_count_is 2
e2e_wait_for 'three successful Forgejo Actions runs' 240 e2e_successful_run_count_at_least 3
e2e_wait_for 'concurrent runner cleanup' 120 e2e_resources_cleaned

printf '%s\n' 'E2E concurrency lifecycle succeeded.'
e2e_completed=true
e2e_stop_port_forward
port_forward_pid=""

if [[ "${E2E_KEEP_CLUSTER:-false}" == true ]]; then
  printf '%s\n' 'Dedicated cluster retained because E2E_KEEP_CLUSTER=true.'
else
  e2e_delete_cluster "${provider}" "${e2e_cluster_name}"
fi
