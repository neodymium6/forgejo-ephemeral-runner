#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=e2e/lib.sh
source "${script_dir}/lib.sh"

test_dir="$(mktemp -d)"
trap 'rm -rf -- "${test_dir}"' EXIT
mkdir -p "${test_dir}/bin"

fail() {
  printf 'test failure: %s\n' "$*" >&2
  exit 1
}

if e2e_assert_safe_cluster_name production >/dev/null 2>&1; then
  fail 'unsafe cluster name was accepted'
fi
e2e_assert_safe_cluster_name "${e2e_cluster_name}"

cat >"${test_dir}/bin/podman" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s|%s|%s|%s\n' \
  "${HOME}" "${XDG_CONFIG_HOME:-}" "${XDG_DATA_HOME:-}" "$*" >>"${E2E_PODMAN_LOG}"
case "$*" in
info) ;;
"image rm --all --force") ;;
*) exit 1 ;;
esac
EOF
cat >"${test_dir}/bin/skopeo" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ " $* " == *" copy "* ]]; then
  destination="${*: -1}"
  archive="${destination#oci-archive:}"
  archive="${archive%%:*}"
  : >"${archive}"
fi
EOF
cat >"${test_dir}/bin/tar" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '{"manifests":[{"annotations":{"org.opencontainers.image.ref.name":"%s"}}]}\n' \
  "${E2E_ARCHIVE_REF}"
EOF
cat >"${test_dir}/bin/df" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' 'Filesystem 1024-blocks Used Available Capacity Mounted on'
printf 'test 20000000 0 %s 0%% /\n' "${E2E_DF_AVAILABLE_KIB}"
EOF
chmod +x "${test_dir}/bin/df"

chmod +x "${test_dir}/bin/podman"
chmod +x "${test_dir}/bin/skopeo"
chmod +x "${test_dir}/bin/tar"
E2E_PODMAN_LOG="${test_dir}/podman.log"
export E2E_PODMAN_LOG
XDG_CONFIG_HOME="${test_dir}/inherited-config"
XDG_DATA_HOME="${test_dir}/inherited-data"
export XDG_CONFIG_HOME XDG_DATA_HOME

original_path="${PATH}"
PATH="${test_dir}/bin:${PATH}"
E2E_DF_AVAILABLE_KIB="${e2e_minimum_free_kib}"
export E2E_DF_AVAILABLE_KIB
e2e_assert_free_disk
E2E_DF_AVAILABLE_KIB="$((e2e_minimum_free_kib - 1))"
if e2e_assert_free_disk >/dev/null 2>&1; then
  fail 'insufficient disk space was accepted'
fi
unset E2E_DF_AVAILABLE_KIB

export PATH

provider="$(e2e_select_provider)"
[[ "${provider}" == podman ]] || fail "selected ${provider}, expected podman"
[[ "$(<"${E2E_PODMAN_LOG}")" == \
  "${e2e_podman_home}|${e2e_podman_config_home}|${e2e_podman_data_home}|info" ]] ||
  fail 'Podman provider check inherited user HOME or XDG paths'
rm -- "${E2E_PODMAN_LOG}"

E2E_CONTAINER_PROVIDER=invalid
export E2E_CONTAINER_PROVIDER
if e2e_select_provider >/dev/null 2>&1; then
  fail 'invalid provider was accepted'
fi
unset E2E_CONTAINER_PROVIDER

e2e_cache_dir="${test_dir}/cache/e2e"
e2e_forgejo_archive="${e2e_cache_dir}/forgejo-${e2e_forgejo_source_digest#sha256:}.tar"
e2e_forgejo_archive_checksum="${e2e_forgejo_archive}.sha256"
E2E_ARCHIVE_REF="${e2e_forgejo_local_image}"
export E2E_ARCHIVE_REF
e2e_prepare_forgejo_cache
[[ -f "${e2e_forgejo_archive}" ]] || fail 'Forgejo image cache was not created'
[[ -f "${e2e_forgejo_archive_checksum}" ]] || fail 'Forgejo image cache checksum was not created'
e2e_prepare_forgejo_cache
E2E_ARCHIVE_REF=docker.io/library/unexpected:latest
if e2e_prepare_forgejo_cache >/dev/null 2>&1; then
  fail 'Forgejo cache with an unexpected image reference was accepted'
fi
E2E_ARCHIVE_REF="${e2e_forgejo_local_image}"
e2e_prepare_forgejo_cache
printf 'corruption' >>"${e2e_forgejo_archive}"
if e2e_prepare_forgejo_cache >/dev/null 2>&1; then
  fail 'Forgejo cache with an unexpected checksum was accepted'
fi

cat >"${test_dir}/bin/kind" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == 'get clusters' ]]; then
  printf '%s\n' forgejo-ephemeral-runner-e2e unrelated-cluster
  exit 0
fi
printf '%s|%s|%s|%s\n' \
  "${HOME}" "${XDG_CONFIG_HOME:-}" "${XDG_DATA_HOME:-}" "$*" >>"${E2E_KIND_LOG}"
EOF
chmod +x "${test_dir}/bin/kind"
E2E_KIND_LOG="${test_dir}/kind.log"
export E2E_KIND_LOG

e2e_cluster_exists podman || fail 'dedicated cluster was not detected'
e2e_kind podman version
[[ "$(<"${E2E_KIND_LOG}")" == \
  "${e2e_podman_home}|${e2e_podman_config_home}|${e2e_podman_data_home}|version" ]] ||
  fail 'Kind inherited user HOME or XDG paths for Podman'
rm -- "${E2E_KIND_LOG}"

if e2e_delete_cluster podman unrelated-cluster >/dev/null 2>&1; then
  fail 'cleanup accepted an unrelated cluster'
fi
[[ ! -e "${E2E_KIND_LOG}" ]] || fail 'kind was called for an unrelated cluster'

# Test the exact delete invocation without touching repository state files.
test_state_dir="${test_dir}/state"
mkdir -p "${test_state_dir}"
e2e_state_dir="${test_state_dir}"
e2e_kubeconfig="${test_state_dir}/kubeconfig"
e2e_provider_file="${test_state_dir}/provider"
e2e_podman_home="${test_state_dir}/podman-home"
e2e_podman_config_home="${e2e_podman_home}/.config"
e2e_podman_data_home="${e2e_podman_home}/.local/share"
e2e_podman_policy="${e2e_podman_config_home}/containers/policy.json"
e2e_prepare_podman_home
[[ -f "${e2e_podman_policy}" ]] || fail 'Podman policy was not created'
mkdir -p "${e2e_podman_data_home}/containers/storage"
e2e_delete_cluster podman "${e2e_cluster_name}"

[[ ! -e "${e2e_podman_home}" ]] || fail 'Podman home was not removed'
[[ "$(<"${E2E_PODMAN_LOG}")" == \
  "${e2e_podman_home}|${e2e_podman_config_home}|${e2e_podman_data_home}|image rm --all --force" ]] ||
  fail 'Podman cleanup inherited user HOME or XDG paths'
[[ "$(<"${E2E_KIND_LOG}")" == \
  "${e2e_podman_home}|${e2e_podman_config_home}|${e2e_podman_data_home}|delete cluster --name ${e2e_cluster_name}" ]] ||
  fail 'unexpected Kind delete arguments or Podman environment'

cat >"${test_dir}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >"${E2E_KUBECTL_LOG}"
EOF
chmod +x "${test_dir}/bin/kubectl"
E2E_KUBECTL_LOG="${test_dir}/kubectl.log"
export E2E_KUBECTL_LOG
e2e_kubeconfig="${test_dir}/dedicated-kubeconfig"
e2e_kubectl get namespace
[[ "$(<"${E2E_KUBECTL_LOG}")" == \
  "--kubeconfig ${e2e_kubeconfig} get namespace" ]] || fail 'kubectl omitted E2E kubeconfig'

PATH="${original_path}"
export PATH
printf '%s\n' 'E2E safety helper tests passed.'
