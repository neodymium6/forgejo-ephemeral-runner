#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=e2e/lib.sh
source "${script_dir}/lib.sh"

e2e_remove_image_cache
printf '%s\n' 'Removed the E2E image cache.'
