#!/usr/bin/env bash

e2e_repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
e2e_cluster_name="forgejo-ephemeral-runner-e2e"
e2e_state_dir="${e2e_repository_root}/.e2e"
e2e_cache_dir="${E2E_CACHE_DIR:-${e2e_repository_root}/.cache/e2e}"
e2e_kubeconfig="${e2e_state_dir}/kubeconfig"
e2e_provider_file="${e2e_state_dir}/provider"
e2e_podman_home="${e2e_state_dir}/podman-home"
e2e_podman_policy="${e2e_podman_home}/.config/containers/policy.json"
e2e_minimum_free_kib=$((10 * 1024 * 1024))
e2e_forgejo_source_image="codeberg.org/forgejo/forgejo@sha256:eda2e378442d2f18cfa563994f8ad66e71f04ac9c3bb4259cc57bdd641890f5c"
e2e_forgejo_source_digest="${e2e_forgejo_source_image##*@}"
e2e_forgejo_local_image="docker.io/library/forgejo-e2e:15.0.5"
e2e_forgejo_archive="${e2e_cache_dir}/forgejo-${e2e_forgejo_source_digest#sha256:}.tar"
e2e_forgejo_archive_checksum="${e2e_forgejo_archive}.sha256"

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

e2e_available_disk_kib() {
  df -Pk "${e2e_repository_root}" | awk 'NR == 2 { print $4 }'
}

e2e_assert_free_disk() {
  local available_kib
  available_kib="$(e2e_available_disk_kib)"
  if [[ ! "${available_kib}" =~ ^[0-9]+$ ]]; then
    e2e_die "could not determine free disk space for E2E"
    return 1
  fi
  if ((available_kib < e2e_minimum_free_kib)); then
    e2e_die "E2E requires at least 10 GiB free; found $((available_kib / 1024 / 1024)) GiB"
    return 1
  fi
}

e2e_prepare_podman_home() {
  mkdir -p "$(dirname "${e2e_podman_policy}")"
  printf '%s\n' \
    '{' \
    '  "default": [{"type": "insecureAcceptAnything"}]' \
    '}' >"${e2e_podman_policy}"
}

e2e_archive_checksum() {
  sha256sum "$1" | awk '{ print $1 }'
}

e2e_archive_reference() {
  tar -xOf "$1" index.json |
    jq -er \
      'select((.manifests | length) == 1) | .manifests[0].annotations["org.opencontainers.image.ref.name"]'
}

e2e_prepare_forgejo_cache() {
  local actual_checksum archive_reference expected_checksum partial_archive partial_checksum
  mkdir -p "${e2e_cache_dir}"

  if [[ -f "${e2e_forgejo_archive}" ]]; then
    [[ -f "${e2e_forgejo_archive_checksum}" ]] ||
      e2e_die "cached Forgejo image checksum is missing" || return
    expected_checksum="$(<"${e2e_forgejo_archive_checksum}")"
    [[ "${expected_checksum}" =~ ^[0-9a-f]{64}$ ]] ||
      e2e_die "cached Forgejo image checksum is invalid" || return
    actual_checksum="$(e2e_archive_checksum "${e2e_forgejo_archive}")" || return
    [[ "${actual_checksum}" == "${expected_checksum}" ]] ||
      e2e_die "cached Forgejo image checksum does not match" || return
    skopeo inspect "oci-archive:${e2e_forgejo_archive}" >/dev/null ||
      e2e_die "cached Forgejo image is unreadable" || return
    archive_reference="$(e2e_archive_reference "${e2e_forgejo_archive}")" ||
      e2e_die "cached Forgejo image reference is unreadable" || return
    [[ "${archive_reference}" == "${e2e_forgejo_local_image}" ]] ||
      e2e_die "cached Forgejo image has unexpected reference ${archive_reference}" || return
    printf 'Using cached Forgejo image %s.\n' "${e2e_forgejo_archive}"
    return
  fi

  partial_archive="${e2e_forgejo_archive}.partial"
  partial_checksum="${e2e_forgejo_archive_checksum}.partial"
  rm -f -- "${partial_archive}" "${partial_checksum}"
  printf 'Caching Forgejo image %s.\n' "${e2e_forgejo_source_image}"
  if ! skopeo \
    --insecure-policy \
    copy \
    --preserve-digests \
    --retry-times 5 \
    "docker://${e2e_forgejo_source_image}" \
    "oci-archive:${partial_archive}:${e2e_forgejo_local_image}"; then
    rm -f -- "${partial_archive}" "${partial_checksum}"
    return 1
  fi

  skopeo inspect "oci-archive:${partial_archive}" >/dev/null || {
    rm -f -- "${partial_archive}" "${partial_checksum}"
    e2e_die "downloaded Forgejo image is unreadable"
    return
  }
  archive_reference="$(e2e_archive_reference "${partial_archive}")" || {
    rm -f -- "${partial_archive}" "${partial_checksum}"
    e2e_die "downloaded Forgejo image reference is unreadable"
    return
  }
  if [[ "${archive_reference}" != "${e2e_forgejo_local_image}" ]]; then
    rm -f -- "${partial_archive}" "${partial_checksum}"
    e2e_die "downloaded Forgejo image has unexpected reference ${archive_reference}"
    return
  fi
  e2e_archive_checksum "${partial_archive}" >"${partial_checksum}"
  mv -- "${partial_archive}" "${e2e_forgejo_archive}"
  mv -- "${partial_checksum}" "${e2e_forgejo_archive_checksum}"
}

e2e_remove_image_cache() {
  if [[ "${e2e_cache_dir}" != "${e2e_repository_root}/.cache/e2e" ]]; then
    e2e_die "refusing to remove non-default E2E cache: ${e2e_cache_dir}"
    return
  fi
  rm -rf -- "${e2e_cache_dir}"
  rmdir -- "${e2e_repository_root}/.cache" 2>/dev/null || true
}

e2e_remove_podman_home() {
  if [[ "${e2e_podman_home}" != "${e2e_state_dir}/podman-home" ]]; then
    e2e_die "refusing to remove unexpected Podman home: ${e2e_podman_home}"
    return 1
  fi
  if [[ -d "${e2e_podman_home}/.local/share/containers/storage" ]]; then
    HOME="${e2e_podman_home}" podman image rm --all --force >/dev/null
  fi
  rm -rf -- "${e2e_podman_home}"
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
    "${e2e_state_dir}/e2e-proxy-image" \
    "${e2e_state_dir}/curl.conf" \
    "${e2e_state_dir}/port-forward.log" \
    "${e2e_state_dir}/runner-image" \
    "${e2e_state_dir}/workflow-request.json"
  if [[ "${provider}" == podman ]]; then
    e2e_remove_podman_home
  fi
  rmdir -- "${e2e_state_dir}" 2>/dev/null || true
}
