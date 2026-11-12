{
  description = "mcp-servers development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # CI and agents build with nixpkgs' Go, never a downloaded toolchain,
        # so go.mod must not ask for a newer one.
        goMod = builtins.readFile ./go.mod;
        goModGo = builtins.match ".*\ngo ([0-9.]+)\n.*" goMod;
        minimumGoVersion =
          if goModGo != null then builtins.head goModGo
          else throw "mcp-servers: go.mod has no `go X.Y.Z` line";
        go =
          if pkgs.lib.versionAtLeast pkgs.go.version minimumGoVersion then pkgs.go
          else throw "mcp-servers: go.mod asks for Go ${minimumGoVersion}, but nixpkgs has Go ${pkgs.go.version}; run `nix flake update nixpkgs` or lower go.mod.";
      in
      {
        devShells.default = pkgs.mkShell {
          packages = [
            go
            pkgs.golangci-lint
            pkgs.govulncheck
            pkgs.gnumake
            pkgs.git
            # Images: ko builds each server onto the distroless base, crane
            # inspects and tags registries.
            pkgs.ko
            pkgs.crane
            pkgs.jq
            pkgs.python3
            pkgs.shellcheck
            # gh is deliberately not here: the coder VM's ~/.local/bin/gh
            # wrapper fetches a fresh token per call, and a dev-shell gh
            # would shadow it.
          ];

          # Never download a newer Go toolchain behind the dev shell's back.
          GOTOOLCHAIN = "local";
        };
      });
}
