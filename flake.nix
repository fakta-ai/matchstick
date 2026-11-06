{
  description = "Ephemeral GitHub Actions runners: one fresh cloud VM per job";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  # A dev shell only: CI and releases build with Go and GoReleaser (.goreleaser.yaml), not with Nix.
  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go_1_27
            pkgs.golangci-lint
            pkgs.govulncheck
            pkgs.kubernetes-helm
            # verifies a release's signatures (docs/operations.md)
            pkgs.cosign
            # the workflows' tests (.github/tests)
            pkgs.jq
            pkgs.yq-go
          ];
        };
      });
    };
}
