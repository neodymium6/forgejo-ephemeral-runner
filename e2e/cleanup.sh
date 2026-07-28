#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=e2e/lib.sh
source "${script_dir}/lib.sh"

e2e_require_command kind

if [[ -f "${e2e_provider_file}" ]]; then
  provider="$(<"${e2e_provider_file}")"
else
  provider="$(e2e_select_provider)"
fi

e2e_delete_cluster "${provider}" "${e2e_cluster_name}"
printf 'Removed dedicated E2E cluster %s.\n' "${e2e_cluster_name}"
