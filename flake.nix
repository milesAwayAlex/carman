{
  description = "carman: an event-driven agent system. So far, its memory (cmem)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # carman is several components, of which the memory is the first. Each
      # is a file under nix/packages and an attribute here; nothing below
      # needs to change when one is added.
      components = pkgs: rec {
        cmem = pkgs.callPackage ./nix/packages/cmem.nix { };
        cmem-claude-code-settings = pkgs.callPackage ./nix/packages/cmem-claude-code-settings.nix { inherit cmem; };
        dispatcher = pkgs.callPackage ./nix/packages/dispatcher.nix { inherit cmem; };
        dispatcher-claude-code-settings = pkgs.callPackage ./nix/packages/dispatcher-claude-code-settings.nix { inherit dispatcher; };
        carman = pkgs.callPackage ./nix/packages/carman.nix { inherit cmem; };
        dev-tools = pkgs.callPackage ./nix/packages/dev-tools.nix { };
      };
    in
    {
      # packages.<system>.cmem, and so on per component.
      packages = forAllSystems (pkgs: components pkgs // {
        default = (components pkgs).cmem;
      });

      # For a system configuration: adds pkgs.carman.cmem.
      overlays.default = final: _prev: { carman = components final; };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          # The same tools as packages.<system>.dev-tools, and git.
          # python3 and restic: for the checks of what agent pods run.
          packages = [ (components pkgs).dev-tools pkgs.git pkgs.python3 pkgs.restic ];
        };
      });
    };
}
