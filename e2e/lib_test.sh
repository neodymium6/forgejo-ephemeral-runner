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
[[ "${1:-}" == info ]]
EOF
chmod +x "${test_dir}/bin/podman"

original_path="${PATH}"
PATH="${test_dir}/bin:${PATH}"
export PATH

provider="$(e2e_select_provider)"
[[ "${provider}" == podman ]] || fail "selected ${provider}, expected podman"

E2E_CONTAINER_PROVIDER=invalid
export E2E_CONTAINER_PROVIDER
if e2e_select_provider >/dev/null 2>&1; then
  fail 'invalid provider was accepted'
fi
unset E2E_CONTAINER_PROVIDER

cat >"${test_dir}/bin/kind" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == 'get clusters' ]]; then
  printf '%s\n' forgejo-ephemeral-runner-e2e unrelated-cluster
  exit 0
fi
printf '%s|%s|%s\n' "${HOME}" "${CONTAINERS_POLICY_JSON:-}" "$*" >>"${E2E_KIND_LOG}"
EOF
chmod +x "${test_dir}/bin/kind"
E2E_KIND_LOG="${test_dir}/kind.log"
export E2E_KIND_LOG

e2e_cluster_exists podman || fail 'dedicated cluster was not detected'
e2e_kind podman version
[[ "$(<"${E2E_KIND_LOG}")" == \
  "${e2e_podman_home}||version" ]] || fail 'Kind omitted the isolated E2E Podman home'
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
e2e_podman_policy="${e2e_podman_home}/.config/containers/policy.json"
e2e_prepare_podman_home
[[ -f "${e2e_podman_policy}" ]] || fail 'Podman policy was not created'
e2e_delete_cluster podman "${e2e_cluster_name}"

[[ ! -e "${e2e_podman_home}" ]] || fail 'Podman home was not removed'
[[ "$(<"${E2E_KIND_LOG}")" == \
  "${e2e_podman_home}||delete cluster --name ${e2e_cluster_name}" ]] ||
  fail 'unexpected Kind delete arguments or Podman policy'

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
