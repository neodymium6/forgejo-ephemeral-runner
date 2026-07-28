set shell := ["bash", "-euo", "pipefail", "-c"]

_default:
    @just --list

fmt:
    gofmt -w cmd internal
    nix fmt

fmt-check:
    test -z "$(gofmt -l cmd internal)"
    nixfmt --check flake.nix

unit:
    go test -race ./...
    bash scripts/run-one-job_test.sh
    bash e2e/lib_test.sh

audit:
    go vet ./...
    shellcheck scripts/*.sh e2e/*.sh
    yamllint -s deploy e2e/manifests e2e/fixtures

manifests-check:
    kustomize build deploy/base | kubeconform -strict -summary

e2e-manifests-check:
    kustomize build e2e/manifests/forgejo | kubeconform -strict -summary
    kustomize build e2e/manifests/controller | kubeconform -strict -summary

check: fmt-check unit audit manifests-check e2e-manifests-check
    nix flake check

images:
    nix build .#runner-image .#controller-image

e2e-preflight:
    bash e2e/preflight.sh

e2e-clean:
    bash e2e/cleanup.sh
