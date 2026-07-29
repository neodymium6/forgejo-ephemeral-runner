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

work_dir=$(mktemp -d)
cleanup() {
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

runner_archive=$(nix build .#runner-image --no-link --print-out-paths)
controller_archive=$(nix build .#controller-image --no-link --print-out-paths)

inspect_archive() {
  local archive=$1
  local expected_user=$2
  local expected_workdir=$3
  local expected_entrypoint_suffix=$4
  local config

  config=$(skopeo inspect --insecure-policy --config "docker-archive:$archive")
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

inspect_archive "$runner_archive" "0:0" "/workspace" ""
inspect_archive "$controller_archive" "65532:65532" "" "/bin/controller"

prepare_image() {
  local name=$1
  local archive=$2
  local directory="$work_dir/$name"

  install -d "$directory"
  skopeo copy --insecure-policy "docker-archive:$archive" "dir:$directory" >/dev/null
  jq -r '.Digest' < <(skopeo inspect --insecure-policy "dir:$directory")
}

runner_digest=$(prepare_image runner "$runner_archive")
controller_digest=$(prepare_image controller "$controller_archive")

if $dry_run; then
  printf 'runner %s\ncontroller %s\n' "$runner_digest" "$controller_digest"
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

server_url=${FORGEJO_SERVER_URL:-}
repository=${FORGEJO_REPOSITORY:-}
repository_owner=${FORGEJO_REPOSITORY_OWNER:-}
registry_token=${REGISTRY_TOKEN:-}
registry_username=${REGISTRY_USERNAME:-$repository_owner}

[[ $server_url =~ ^https://([^/]+)/?$ ]] ||
  fail "FORGEJO_SERVER_URL must be an HTTPS origin without a path"
registry=${BASH_REMATCH[1]}
[[ $repository =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] ||
  fail "FORGEJO_REPOSITORY must have the form owner/repository"
[[ -n $repository_owner && -n $registry_username ]] ||
  fail "Forgejo repository owner and registry username must be set"
[[ -n $registry_token ]] || fail "REGISTRY_TOKEN is required"

auth_file="$work_dir/auth.json"
printf '%s' "$registry_token" |
  skopeo login --authfile "$auth_file" --username "$registry_username" --password-stdin "$registry" >/dev/null
unset REGISTRY_TOKEN registry_token

publish_image() {
  local name=$1
  local expected_digest=$2
  local source="dir:$work_dir/$name"
  local repository_ref="$registry/$repository/$name"
  local destination="docker://$repository_ref:$version"
  local tags
  local remote_digest

  tags=$(skopeo list-tags --authfile "$auth_file" "docker://$repository_ref")
  if jq -e --arg tag "$version" '.Tags | index($tag) != null' <<<"$tags" >/dev/null; then
    remote_digest=$(
      skopeo inspect --authfile "$auth_file" "$destination" |
        jq -r '.Digest'
    )
    [[ $remote_digest == "$expected_digest" ]] ||
      fail "$repository_ref:$version already exists with a different digest"
  else
    skopeo copy \
      --insecure-policy \
      --retry-times 3 \
      --authfile "$auth_file" \
      "$source" \
      "$destination" >/dev/null
    remote_digest=$(
      skopeo inspect --authfile "$auth_file" "$destination" |
        jq -r '.Digest'
    )
    [[ $remote_digest == "$expected_digest" ]] ||
      fail "$repository_ref:$version digest changed during publication"
  fi

  printf '%s@%s' "$repository_ref" "$remote_digest"
}

runner_ref=$(publish_image runner "$runner_digest")
controller_ref=$(publish_image controller "$controller_digest")

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
sha256sum "$manifest" >"$manifest.sha256"

cat >dist/RELEASE_NOTES.md <<EOF
## OCI images

- Runner: \`$runner_ref\`
- Controller: \`$controller_ref\`

The attached JSON manifest records the source commit and immutable image
digests. Deployment remains an explicit operator action.
EOF

printf 'published %s\npublished %s\n' "$runner_ref" "$controller_ref"
