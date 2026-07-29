# Releasing

Releases are published to the OCI registry and Releases service of the Forgejo
instance that hosts this repository. The workflow derives the HTTPS registry
host and `owner/repository` package prefix from Forgejo's runtime context, so no
deployment-specific domain or account is stored in this repository.

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

## Prepare a release

1. Choose a stable semantic version in `X.Y.Z` form.
2. Set the same version in `flake.nix`.
3. Run `nix develop --command just check`.
4. Commit and merge the reviewed change to `main`.
5. Confirm that the Forgejo `CI` workflow succeeds for that commit.

The publication script can build and inspect both OCI images without contacting
the registry:

```sh
nix develop --command just release-dry-run 0.2.0
```

The dry-run requires a clean `x86_64-linux` checkout. It verifies image users,
architecture, working directory, and controller entrypoint, then prints the
locally calculated image digests.

## Publish

Create the tag on the reviewed `main` commit and push only that tag:

```sh
git switch main
git pull --ff-only
git tag -a v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0
```

`.forgejo/workflows/release.yaml` then:

1. checks out the full history without persisting checkout credentials;
2. runs the complete `just check` suite;
3. verifies that the tag version matches `flake.nix` and belongs to `main`;
4. builds and inspects the runner and controller images;
5. publishes `runner:X.Y.Z` and `controller:X.Y.Z` under this repository's
   Forgejo OCI package prefix;
6. creates a Forgejo Release with a JSON manifest, checksums, and immutable
   `name@sha256:...` references.

The script never creates `latest`. If a version already exists with the same
digest, publication is resumable. If the existing digest differs, it fails
without overwriting the version. The release action also refuses to replace an
existing Release.

The base runner uses single-user Nix without a build sandbox under the dedicated
UID and GID 65532. Its private Nix store is writable by that identity, while the
container filesystem root is not. A workflow therefore cannot create Nix's
reserved `/homeless-shelter` build-home path. Each disposable Pod starts with a
fresh writable image layer and does not require cleanup or retry logic between
builds.

Publishing is separate from deployment. Review the generated image manifest,
then update and reconcile the private infrastructure overlay in an explicit
operator change.
