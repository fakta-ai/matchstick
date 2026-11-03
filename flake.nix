{
  description = "Ephemeral GitHub Actions runners: one fresh cloud VM per job";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = self.shortRev or self.dirtyShortRev or "dev";
    in
    {
      packages = forAll (
        pkgs:
        let
          matchstick = pkgs.callPackage ./nix/package.nix { inherit version; };
        in
        {
          default = matchstick;
          inherit matchstick;
        }
        // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          image = pkgs.callPackage ./nix/image.nix { inherit matchstick version; };
        }
      );

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go_1_27
            pkgs.golangci-lint
            pkgs.govulncheck
            pkgs.kubernetes-helm
            # the review gate's test (.github/tests/codex.sh)
            pkgs.jq
            pkgs.yq-go
          ];
        };
      });

      checks = forAll (pkgs: {
        # building the package runs go test ./... -short
        matchstick = self.packages.${pkgs.stdenv.hostPlatform.system}.matchstick;
      });
    };
}
