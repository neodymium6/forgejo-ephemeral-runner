#!/usr/bin/env bash

set -euo pipefail
umask 077

usage() {
  echo "usage: $0 [--dry-run] VERSION" >&2
}

fail() {
  echo "error: $*" >&2
  exit 1
}

dry_run=false
if [[ ${1:-} == "--dry-run" ]]; then
  dry_run=true
  shift
fi

if [[ $# -ne 1 ]]; then
  usage
  exit 2
fi

version=$1
if [[ ! $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  fail "VERSION must be a stable semantic version such as 0.2.0"
fi

if [[ $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then
  fail "image publication currently requires x86_64 Linux"
fi

for command in git grep install jq mktemp nix sha256sum skopeo uname; do
  command -v "$command" >/dev/null || fail "required command not found: $command"
done

flake_version=$(nix eval --raw .#packages.x86_64-linux.controller.version)
if [[ $version != "$flake_version" ]]; then
  fail "requested version $version does not match flake version $flake_version"
fi

if [[ -n $(git status --porcelain) ]]; then
  fail "the Git worktree must be clean"
fi

work_root=${RELEASE_TMPDIR:-"$PWD/.release-tmp"}
install -d -m 0700 "$work_root"
work_dir=$(mktemp -d "$work_root/publish.XXXXXXXX")
cleanup() {
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

registries_conf="$work_dir/registries.conf"
printf 'unqualified-search-registries = []\n' >"$registries_conf"
export CONTAINERS_REGISTRIES_CONF="$registries_conf"

skopeo_tmp_dir="$work_dir/skopeo"
install -d "$skopeo_tmp_dir"
skopeo_run() {
  command skopeo --tmpdir "$skopeo_tmp_dir" "$@"
}

controller_archive=$(nix build .#controller-image --no-link --print-out-paths)
runner_archive=$(nix build .#runner-image --no-link --print-out-paths)

inspect_archive() {
  local archive=$1
  local expected_user=$2
  local expected_workdir=$3
  local expected_entrypoint_suffix=$4
  local config

  config=$(skopeo_run inspect --insecure-policy --config "docker-archive:$archive")
  [[ $(jq -r '.architecture' <<<"$config") == amd64 ]] || fail "$archive is not amd64"
  [[ $(jq -r '.os' <<<"$config") == linux ]] || fail "$archive is not a Linux image"
  [[ $(jq -r '.config.User' <<<"$config") == "$expected_user" ]] ||
    fail "$archive has an unexpected user"

  if [[ -n $expected_workdir ]]; then
    [[ $(jq -r '.config.WorkingDir' <<<"$config") == "$expected_workdir" ]] ||
      fail "$archive has an unexpected working directory"
  fi

  if [[ -n $expected_entrypoint_suffix ]]; then
    local entrypoint
    entrypoint=$(jq -r '.config.Entrypoint[0] // ""' <<<"$config")
    [[ $entrypoint == *"$expected_entrypoint_suffix" ]] ||
      fail "$archive has an unexpected entrypoint"
  fi
}

inspect_archive "$runner_archive" "65532:65532" "/workspace" ""
inspect_archive "$controller_archive" "65532:65532" "" "/bin/controller"

archive_config_digest() {
  local archive=$1

  skopeo_run inspect --insecure-policy --raw "docker-archive:$archive" |
    jq -er '.config.digest | select(test("^sha256:[0-9a-f]{64}$"))'
}

runner_config_digest=$(archive_config_digest "$runner_archive")
controller_config_digest=$(archive_config_digest "$controller_archive")

if $dry_run; then
  printf 'runner config %s\ncontroller config %s\n' "$runner_config_digest" "$controller_config_digest"
  exit 0
fi

tag="v$version"
tag_commit=$(git rev-parse "refs/tags/$tag^{commit}" 2>/dev/null) || fail "tag $tag does not exist"
head_commit=$(git rev-parse HEAD)
[[ $tag_commit == "$head_commit" ]] || fail "tag $tag does not point to HEAD"

git show-ref --verify --quiet refs/remotes/origin/main ||
  fail "refs/remotes/origin/main is unavailable; use a full checkout"
git merge-base --is-ancestor HEAD refs/remotes/origin/main ||
  fail "tagged commit is not contained in origin/main"

registry=${REGISTRY_HOST:-}
server_url=${FORGEJO_SERVER_URL:-}
repository=${REGISTRY_REPOSITORY:-${FORGEJO_REPOSITORY:-}}
repository_owner=${REGISTRY_REPOSITORY_OWNER:-${FORGEJO_REPOSITORY_OWNER:-}}
registry_token=${REGISTRY_TOKEN:-}
registry_username=${REGISTRY_USERNAME:-$repository_owner}
image_prefix=${REGISTRY_IMAGE_PREFIX:-$repository}
image_separator=${REGISTRY_IMAGE_SEPARATOR:-/}
github_package_owner=${REGISTRY_GITHUB_PACKAGE_OWNER:-}

if [[ -z $registry ]]; then
  [[ $server_url =~ ^https://([^/]+)/?$ ]] ||
    fail "REGISTRY_HOST or an HTTPS FORGEJO_SERVER_URL origin is required"
  registry=${BASH_REMATCH[1]}
fi
[[ $registry =~ ^[A-Za-z0-9.-]+(:[0-9]+)?$ ]] ||
  fail "REGISTRY_HOST must be a registry host without a scheme or path"
[[ $repository =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] ||
  fail "REGISTRY_REPOSITORY must have the form owner/repository"
[[ -n $repository_owner && -n $registry_username ]] ||
  fail "repository owner and registry username must be set"
[[ $image_prefix =~ ^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$ ]] ||
  fail "REGISTRY_IMAGE_PREFIX contains unsupported characters"
[[ $image_separator == / || $image_separator == - ]] ||
  fail "REGISTRY_IMAGE_SEPARATOR must be / or -"
[[ -n $registry_token ]] || fail "REGISTRY_TOKEN is required"
if [[ -n $github_package_owner ]]; then
  [[ $registry == ghcr.io ]] ||
    fail "REGISTRY_GITHUB_PACKAGE_OWNER is supported only for ghcr.io"
  [[ $image_prefix == "$github_package_owner/"* ]] ||
    fail "REGISTRY_IMAGE_PREFIX must start with the GitHub package owner"
  [[ -n ${GH_TOKEN:-} ]] ||
    fail "GH_TOKEN is required to check GitHub package existence"
  command -v gh >/dev/null || fail "required command not found: gh"
fi

auth_file="$work_dir/auth.json"
printf '%s' "$registry_token" |
  skopeo_run login --authfile "$auth_file" --username "$registry_username" --password-stdin "$registry" >/dev/null
unset REGISTRY_TOKEN registry_token

github_package_exists() {
  local package_name=$1
  local owner_type
  local endpoint
  local response_file="$work_dir/github-package-$package_name.response"

  owner_type=$(gh api "users/$github_package_owner" --jq .type) ||
    fail "cannot determine the GitHub package owner type"
  case $owner_type in
    User)
      endpoint="users/$github_package_owner/packages/container/$package_name"
      ;;
    Organization)
      endpoint="orgs/$github_package_owner/packages/container/$package_name"
      ;;
    *)
      fail "unsupported GitHub package owner type: $owner_type"
      ;;
  esac

  if gh api --include "$endpoint" >"$response_file" 2>&1; then
    return 0
  fi
  if grep -Eq "^HTTP/[0-9.]+ 404 " "$response_file"; then
    return 1
  fi
  fail "cannot verify whether GitHub package $package_name exists"
}

publish_image() {
  local name=$1
  local expected_config_digest=$2
  local archive=$3
  local source="docker-archive:$archive"
  local repository_ref="$registry/$image_prefix$image_separator$name"
  local destination="docker://$repository_ref:$version"
  local tags
  local remote_config_digest
  local remote_digest
  local package_name
  local list_tags_error="$work_dir/list-tags-$name.error"

  if ! tags=$(
    skopeo_run list-tags --authfile "$auth_file" "docker://$repository_ref" 2>"$list_tags_error"
  ); then
    if [[ -z $github_package_owner ]]; then
      fail "cannot list tags for $repository_ref"
    fi
    package_name=${repository_ref#"$registry/$github_package_owner/"}
    [[ $package_name != "$repository_ref" && $package_name != */* ]] ||
      fail "cannot derive the GitHub package name from $repository_ref"
    if github_package_exists "$package_name"; then
      fail "cannot list tags for existing GitHub package $package_name"
    fi
    tags='{"Tags":[]}'
  fi
  if jq -e --arg tag "$version" '.Tags | index($tag) != null' <<<"$tags" >/dev/null; then
    remote_config_digest=$(
      skopeo_run inspect --authfile "$auth_file" --raw "$destination" |
        jq -er '.config.digest | select(test("^sha256:[0-9a-f]{64}$"))'
    )
    [[ $remote_config_digest == "$expected_config_digest" ]] ||
      fail "$repository_ref:$version already exists with different image content"
  else
    skopeo_run copy \
      --insecure-policy \
      --retry-times 3 \
      --authfile "$auth_file" \
      "$source" \
      "$destination" >/dev/null
    remote_config_digest=$(
      skopeo_run inspect --authfile "$auth_file" --raw "$destination" |
        jq -er '.config.digest | select(test("^sha256:[0-9a-f]{64}$"))'
    )
    [[ $remote_config_digest == "$expected_config_digest" ]] ||
      fail "$repository_ref:$version content changed during publication"
  fi

  remote_digest=$(
    skopeo_run inspect --authfile "$auth_file" "$destination" |
      jq -er '.Digest | select(test("^sha256:[0-9a-f]{64}$"))'
  )

  printf '%s@%s' "$repository_ref" "$remote_digest"
}

runner_ref=$(publish_image runner "$runner_config_digest" "$runner_archive")
controller_ref=$(publish_image controller "$controller_config_digest" "$controller_archive")

release_dir=dist/release
install -d "$release_dir"
manifest="$release_dir/forgejo-ephemeral-runner-images-$version.json"
jq -n \
  --arg version "$version" \
  --arg source_commit "$head_commit" \
  --arg runner "$runner_ref" \
  --arg controller "$controller_ref" \
  '{
    version: $version,
    source_commit: $source_commit,
    images: {
      runner: $runner,
      controller: $controller
    }
  }' >"$manifest"
(
  cd "$release_dir"
  sha256sum "${manifest##*/}" >"${manifest##*/}.sha256"
)

cat >dist/RELEASE_NOTES.md <<EOF
## OCI images

- Runner: \`$runner_ref\`
- Controller: \`$controller_ref\`

The attached JSON manifest records the source commit and immutable image
digests. Deployment remains an explicit operator action.
EOF

printf 'published %s\npublished %s\n' "$runner_ref" "$controller_ref"
