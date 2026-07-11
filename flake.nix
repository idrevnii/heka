{
  description = "heka — minimal passthrough AI gateway with API key rotation";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.05";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        heka = pkgs.buildGoModule {
          pname = "heka";
          version = "0.1.0";
          src = self;
          # Dependencies are vendored (vendor/), no fixed-output hash to maintain.
          vendorHash = null;
          subPackages = [ "cmd/heka" ];
          ldflags = [ "-s" "-w" ];
          meta = {
            description = "Minimal passthrough AI gateway with API key rotation";
            mainProgram = "heka";
            license = nixpkgs.lib.licenses.mit;
          };
        };
        default = heka;
      });

      overlays.default = final: prev: {
        heka = self.packages.${final.stdenv.hostPlatform.system}.heka;
      };

      nixosModules.default = { pkgs, lib, ... }: {
        imports = [ ./nix/module.nix ];
        services.heka.package =
          lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.heka;
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls ]; };
      });
    };
}
