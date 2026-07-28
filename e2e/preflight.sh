#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=e2e/lib.sh
source "${script_dir}/lib.sh"

e2e_require_command df kind kubectl kustomize
e2e_assert_free_disk
provider="$(e2e_select_provider)"

printf 'E2E cluster: %s\n' "${e2e_cluster_name}"
printf 'Container provider: %s\n' "${provider}"
printf 'Kubeconfig: %s\n' "${e2e_kubeconfig}"

if e2e_cluster_exists "${provider}"; then
  printf '%s\n' \
    "The dedicated E2E cluster already exists; run 'just e2e-clean' before a fresh test."
else
  printf '%s\n' 'No dedicated E2E cluster exists.'
fi

case "${provider}" in
podman)
  printf '%s\n' 'Kind will use its experimental Podman provider.'
  ;;
docker) ;;
esac
