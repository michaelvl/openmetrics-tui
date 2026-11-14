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
          pname = "omtui";
          version = self.shortRev or self.dirtyShortRev or "dev";
          src = ./.;

          vendorHash = "sha256-NI6lhIpL7lelN/ZWdoxG8nusSzfNtVUqRPfKmJLywvM=";

          nativeBuildInputs = [ pkgs.makeWrapper ];

          subPackages = [ "cmd/omtui" "cmd/omtui-dash" ];
          env.CGO_ENABLED = 0;
          ldflags = [ "-s" "-w" ];

          # Managed mode needs victoria-metrics at runtime. --suffix keeps a
          # victoria-metrics already on the user's PATH ahead of the bundled
          # one; --vm-bin overrides both. kubectl is the user's own. omtui has
          # no runtime dependencies and is not wrapped.
          postInstall = ''
            wrapProgram $out/bin/omtui-dash \
              --suffix PATH : ${pkgs.lib.makeBinPath [ pkgs.victoriametrics ]}
          '';

          meta = with pkgs.lib; {
            description = "Terminal-based tool to monitor OpenMetrics/Prometheus metrics in real-time";
            homepage = "https://github.com/michaelvl/openmetrics-tui";
            mainProgram = "omtui";
            platforms = platforms.unix;
          };
        };
      in
      {
        packages = {
          default = openmetrics-tui;
          inherit openmetrics-tui;
          omtui-dash = openmetrics-tui;
        };

        apps = {
          default = flake-utils.lib.mkApp { drv = openmetrics-tui; };
          omtui-dash = flake-utils.lib.mkApp { drv = openmetrics-tui; name = "omtui-dash"; };
        };

        devShells.default = pkgs.mkShell {
          inputsFrom = [ openmetrics-tui ];
          packages = with pkgs; [ gnumake golangci-lint victoriametrics ];
        };
      });
}
