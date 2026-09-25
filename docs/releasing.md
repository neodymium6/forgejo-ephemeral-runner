# Releasing

Releases can be published independently through Forgejo and GitHub. Each remote
builds the same pinned source and creates a release with an image manifest,
checksums, and immutable OCI digests. Forgejo publishes under the hosting
instance's OCI registry. GitHub publishes the two images under GHCR and creates
a GitHub Release. No deployment-specific domain, account, or credential is
stored in this repository.

## One-time repository setup

Configure these values in the Forgejo repository's Actions settings:

- Secret `REGISTRY_TOKEN`: a token with package write access for the package
  owner. Do not grant repository administration or runner-management access.
- Variable `REGISTRY_USERNAME`: the account that owns the token, but only when
  it differs from the repository owner. A personal repository whose owner also
  owns the token can omit this variable.

Protect the `v*.*.*` tag pattern. Only reviewed commits on `main` should be
eligible for a release tag. The registry credential is available only to the
tag workflow step that publishes images.

GitHub needs no additional repository secret. Its publication job uses the
short-lived `GITHUB_TOKEN` with `contents: write` and `packages: write`. The
preceding E2E job has only `contents: read` and receives no deployment secrets.

## Prepare a release

1. Choose a stable semantic version in `X.Y.Z` form.
2. Set the same version in `flake.nix`.
3. Run `nix develop --command just check`.
4. Commit and merge the reviewed change to `main`.
5. Confirm that the `CI` workflow succeeds for that commit on each target
   release remote, including the E2E job on GitHub.

The publication script can build and inspect both OCI images without contacting
the registry:

```sh
nix develop --command just release-dry-run 0.2.0
```

The dry-run requires a clean `x86_64-linux` checkout. It verifies image users,
architecture, working directory, and controller entrypoint, then prints each
image configuration digest. The configuration digest covers the runtime
configuration and uncompressed filesystem-layer identities.

Image inspection and publication stage temporary container data under the
ignored `.release-tmp/` directory in the checkout. Allow at least 1 GiB of free
space there for the current runner image. Set `RELEASE_TMPDIR` to an alternative
private directory when the checkout filesystem is too small; the per-run
subdirectory is always removed on exit.

## Publish

Create the tag on the reviewed `main` commit and push only that tag to each
release remote:

```sh
git switch main
git pull --ff-only
git tag -a v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0
git push github v0.2.0
```

The tag triggers `.forgejo/workflows/release.yaml` or
`.github/workflows/release.yaml` on the corresponding remote.

On GitHub, a separate E2E job first runs the disposable Kind harness against
the tagged source. Publication is skipped if E2E fails or is cancelled. This
gate is independent of the earlier CI run on `main`. Forgejo's release workflow
does not run E2E because its unprivileged runner Pod has no container-runtime
service.

The publication job on each remote:

1. checks out the full history without persisting checkout credentials;
2. runs the complete `just check` suite;
3. verifies that the tag version matches `flake.nix` and belongs to `main`;
4. builds and inspects the runner and controller images;
5. publishes `runner:X.Y.Z` and `controller:X.Y.Z` under the corresponding
   Forgejo registry or GHCR package prefix;
6. creates the corresponding Forgejo or GitHub Release with a JSON manifest,
   checksums, and immutable `name@sha256:...` references.

GitHub uses the package names
`ghcr.io/owner/repository-runner:X.Y.Z` and
`ghcr.io/owner/repository-controller:X.Y.Z`. Forgejo retains its existing
`registry/owner/repository/runner:X.Y.Z` and
`registry/owner/repository/controller:X.Y.Z` layout.

The checksum file records only the manifest basename, so the two downloaded
assets can be verified directly from the same directory with `sha256sum -c`.

The workflow places its pinned release action dependencies, including `curl`,
`jq`, and `which`, on the action path from the Nix-built `release-tools`
package. It does not install packages in the disposable runner at runtime.

The script never creates `latest`. A registry may recompress layers while
preserving their uncompressed content, so resumability compares the image
configuration digest rather than the transport manifest digest. If a version
already contains the same image configuration, publication resumes and records
its registry manifest digest. Different content fails without overwriting the
version. The release action also refuses to replace an existing Release.

The base runner uses single-user Nix without a build sandbox under the dedicated
UID and GID 65532. Its private Nix store is writable by that identity, while the
container filesystem root is not. A workflow therefore cannot create Nix's
reserved `/homeless-shelter` build-home path. Each disposable Pod starts with a
fresh writable image layer and does not require cleanup or retry logic between
builds.

Publishing is separate from deployment. Review the generated image manifest,
then update and reconcile the private infrastructure overlay in an explicit
operator change.
