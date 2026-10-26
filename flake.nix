{
  description = "Terminal-based tool to monitor OpenMetrics/Prometheus metrics in real-time";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        openmetrics-tui = pkgs.buildGoModule {
          pname = "openmetrics-tui";
          version = self.shortRev or self.dirtyShortRev or "dev";
          src = ./.;

          vendorHash = "sha256-RORd99onHfJbfRaD/q0NclbvTxjOkmKOpH2U12WtWb4=";

          subPackages = [ "." ];
          env.CGO_ENABLED = 0;
          ldflags = [ "-s" "-w" ];

          meta = with pkgs.lib; {
            description = "Terminal-based tool to monitor OpenMetrics/Prometheus metrics in real-time";
            homepage = "https://github.com/michaelvl/openmetrics-tui";
            mainProgram = "openmetrics-tui";
            platforms = platforms.unix;
          };
        };
      in
      {
        packages = {
          default = openmetrics-tui;
          inherit openmetrics-tui;
        };

        apps.default = flake-utils.lib.mkApp { drv = openmetrics-tui; };

        devShells.default = pkgs.mkShell {
          inputsFrom = [ openmetrics-tui ];
          packages = with pkgs; [ gnumake golangci-lint ];
        };
      });
}
