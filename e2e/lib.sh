#!/usr/bin/env bash

e2e_repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
e2e_cluster_name="forgejo-ephemeral-runner-e2e"
e2e_state_dir="${e2e_repository_root}/.e2e"
e2e_kubeconfig="${e2e_state_dir}/kubeconfig"
e2e_provider_file="${e2e_state_dir}/provider"
e2e_podman_home="${e2e_state_dir}/podman-home"
e2e_podman_policy="${e2e_podman_home}/.config/containers/policy.json"

e2e_die() {
  printf 'error: %s\n' "$*" >&2
  return 1
}

e2e_require_command() {
  local command_name
  for command_name in "$@"; do
    command -v "${command_name}" >/dev/null 2>&1 ||
      e2e_die "required command not found: ${command_name}" || return
  done
}

e2e_assert_safe_cluster_name() {
  local requested_name="$1"
  if [[ "${requested_name}" != "${e2e_cluster_name}" ]]; then
    e2e_die "refusing to operate on unexpected Kind cluster: ${requested_name}"
  fi
}

e2e_runtime_works() {
  local provider="$1"
  command -v "${provider}" >/dev/null 2>&1 && "${provider}" info >/dev/null 2>&1
}

e2e_select_provider() {
  local requested_provider="${E2E_CONTAINER_PROVIDER:-}"
  if [[ -n "${requested_provider}" ]]; then
    case "${requested_provider}" in
    docker | podman) ;;
    *) e2e_die "E2E_CONTAINER_PROVIDER must be docker or podman" || return ;;
    esac
    e2e_runtime_works "${requested_provider}" ||
      e2e_die "${requested_provider} is unavailable or its service is not running" || return
    printf '%s\n' "${requested_provider}"
    return
  fi

  if e2e_runtime_works podman; then
    printf '%s\n' podman
    return
  fi
  if e2e_runtime_works docker; then
    printf '%s\n' docker
    return
  fi
  e2e_die "neither a working Podman nor Docker installation was found"
}

e2e_prepare_podman_home() {
  mkdir -p "$(dirname "${e2e_podman_policy}")"
  printf '%s\n' \
    '{' \
    '  "default": [{"type": "insecureAcceptAnything"}]' \
    '}' >"${e2e_podman_policy}"
}

e2e_kind() {
  local provider="$1"
  shift
  if [[ "${provider}" == podman ]]; then
    HOME="${e2e_podman_home}" \
      KIND_EXPERIMENTAL_PROVIDER="${provider}" kind "$@"
  else
    KIND_EXPERIMENTAL_PROVIDER="${provider}" kind "$@"
  fi
}

e2e_kubectl() {
  kubectl --kubeconfig "${e2e_kubeconfig}" "$@"
}

e2e_cluster_exists() {
  local provider="$1"
  e2e_kind "${provider}" get clusters 2>/dev/null |
    grep -Fqx -- "${e2e_cluster_name}"
}

e2e_delete_cluster() {
  local provider="$1"
  local requested_name="$2"
  e2e_assert_safe_cluster_name "${requested_name}" || return
  if e2e_cluster_exists "${provider}"; then
    e2e_kind "${provider}" delete cluster --name "${requested_name}"
  fi

  rm -f -- \
    "${e2e_kubeconfig}" \
    "${e2e_provider_file}" \
    "${e2e_state_dir}/api-token" \
    "${e2e_state_dir}/controller-image" \
    "${e2e_state_dir}/curl.conf" \
    "${e2e_state_dir}/port-forward.log" \
    "${e2e_state_dir}/runner-image" \
    "${e2e_state_dir}/workflow-request.json"
  if [[ "${provider}" == podman ]]; then
    if [[ "${e2e_podman_home}" != "${e2e_state_dir}/podman-home" ]]; then
      e2e_die "refusing to remove unexpected Podman home: ${e2e_podman_home}"
      return 1
    fi
    rm -rf -- "${e2e_podman_home}"
  fi
  rmdir -- "${e2e_state_dir}" 2>/dev/null || true
}
