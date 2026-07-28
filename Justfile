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
    yamllint -s deploy

manifests-check:
    kustomize build deploy/base | kubeconform -strict -summary

check: fmt-check unit audit manifests-check
    nix flake check

images:
    nix build .#runner-image .#controller-image

e2e-preflight:
    bash e2e/preflight.sh

e2e-clean:
    bash e2e/cleanup.sh
