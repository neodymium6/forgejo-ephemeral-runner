{
  description = "One-job Forgejo runners recycled as Kubernetes Pods";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          version = "0.2.2";
          controller = pkgs.buildGoModule {
            pname = "forgejo-ephemeral-runner-controller";
            inherit version;
            src = pkgs.lib.cleanSource ./.;
            vendorHash = null;
            subPackages = [ "cmd/controller" ];
            ldflags = [
              "-s"
              "-w"
            ];
          };
          e2eProxy = pkgs.buildGoModule {
            pname = "forgejo-ephemeral-runner-e2e-proxy";
            inherit version;
            src = pkgs.lib.cleanSource ./.;
            vendorHash = null;
            subPackages = [ "cmd/e2e-proxy" ];
            ldflags = [
              "-s"
              "-w"
            ];
          };
          runOneJobScript = pkgs.writeShellApplication {
            name = "forgejo-ephemeral-one-job";
            runtimeInputs = [
              pkgs.coreutils
              pkgs.forgejo-runner
            ];
            text = builtins.readFile ./scripts/run-one-job.sh;
          };
          runnerUID = 65532;
          runnerGID = 65532;
          runnerUser = "runner";
          runnerNss = pkgs.dockerTools.fakeNss.override {
            extraPasswdLines = [
              "${runnerUser}:x:${toString runnerUID}:${toString runnerGID}:Forgejo runner:/home/runner:/bin/sh"
            ];
            extraGroupLines = [ "${runnerUser}:x:${toString runnerGID}:" ];
          };
          runnerContents = [
            pkgs.bash
            runnerNss
            pkgs.dockerTools.usrBinEnv
            pkgs.cacert
            pkgs.coreutils
            pkgs.findutils
            pkgs.forgejo-runner
            pkgs.git
            pkgs.gnugrep
            pkgs.gnused
            pkgs.jq
            pkgs.nix
            pkgs.nodejs_24
            pkgs.openssh
            runOneJobScript
          ];
          linuxImages = pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
            runner-image = pkgs.dockerTools.buildLayeredImageWithNixDb {
              name = "forgejo-ephemeral-runner";
              tag = version;
              uid = runnerUID;
              gid = runnerGID;
              uname = runnerUser;
              gname = runnerUser;
              contents = runnerContents;
              fakeRootCommands = ''
                mkdir -p home/runner workspace tmp
                chown -R ${toString runnerUID}:${toString runnerGID} nix home/runner workspace tmp
                chmod 1777 tmp
              '';
              config = {
                User = "${toString runnerUID}:${toString runnerGID}";
                WorkingDir = "/workspace";
                Env = [
                  "PATH=${pkgs.lib.makeBinPath runnerContents}"
                  "HOME=/home/runner"
                  "USER=${runnerUser}"
                  "LOGNAME=${runnerUser}"
                  "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
                  "NIX_SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
                ];
              };
            };
            controller-image = pkgs.dockerTools.buildLayeredImage {
              name = "forgejo-ephemeral-runner-controller";
              tag = version;
              contents = [
                controller
                pkgs.cacert
              ];
              config = {
                User = "65532:65532";
                Entrypoint = [ "${controller}/bin/controller" ];
                Env = [ "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt" ];
              };
            };
            e2e-proxy-image = pkgs.dockerTools.buildLayeredImage {
              name = "forgejo-ephemeral-runner-e2e-proxy";
              tag = version;
              contents = [
                e2eProxy
                pkgs.cacert
              ];
              config = {
                User = "65532:65532";
                Entrypoint = [ "${e2eProxy}/bin/e2e-proxy" ];
                Env = [ "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt" ];
              };
            };
          };
        in
        {
          default = controller;
          inherit controller;
          release-tools = pkgs.symlinkJoin {
            name = "forgejo-ephemeral-runner-release-tools";
            paths = [
              pkgs.coreutils
              pkgs.curl
              pkgs.gnugrep
              pkgs.gnused
              pkgs.jq
            ];
          };
        }
        // linuxImages
      );

      checks = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          go-test =
            pkgs.runCommand "forgejo-ephemeral-runner-go-test"
              {
                nativeBuildInputs = [ pkgs.go_1_26 ];
              }
              ''
                export HOME="$TMPDIR/home"
                export GOCACHE="$TMPDIR/go-cache"
                export CGO_ENABLED=0
                mkdir -p "$HOME" "$GOCACHE"
                cp -R ${pkgs.lib.cleanSource ./.} source
                chmod -R u+w source
                cd source
                go test ./...
                touch "$out"
              '';
        }
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          default = pkgs.mkShell {
            packages =
              with pkgs;
              [
                curl
                forgejo-runner
                git
                kind
                go_1_26
                golangci-lint
                gopls
                gotools
                jq
                just
                kubeconform
                kubectl
                kustomize
                nixfmt-tree
                shellcheck
                skopeo
                yamllint
              ]
              ++ pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.podman ];
            shellHook = ''
              export GOTOOLCHAIN=local
            '';
          };
        }
      );

      formatter = forAllSystems (system: (import nixpkgs { inherit system; }).nixfmt-tree);
    };
}
