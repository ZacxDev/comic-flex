{
  description = "Comic Flex - GTK3 image slideshow viewer";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        nativeBuildInputs = with pkgs; [
          pkg-config
          wrapGAppsHook3
        ];

        buildInputs = with pkgs; [
          gtk3
          gdk-pixbuf
          glib
          cairo
          pango
          atk
          gobject-introspection
          gsettings-desktop-schemas
          dconf
        ];
      in
      {
        devShells.default = pkgs.mkShell {
          inherit nativeBuildInputs buildInputs;

          packages = with pkgs; [
            go
            gopls
            gotools
          ];

          shellHook = ''
            export CGO_ENABLED=1
            export XDG_DATA_DIRS="${pkgs.gsettings-desktop-schemas}/share/gsettings-schemas/${pkgs.gsettings-desktop-schemas.name}:${pkgs.gtk3}/share/gsettings-schemas/${pkgs.gtk3.name}:$XDG_DATA_DIRS"
            export GIO_EXTRA_MODULES="${pkgs.dconf.lib}/lib/gio/modules"
          '';
        };

        packages.default = pkgs.buildGoModule {
          pname = "comic-flex";
          version = "0.1.0";
          src = ./.;

          vendorHash = null;

          inherit nativeBuildInputs buildInputs;

          CGO_ENABLED = 1;

          meta = with pkgs.lib; {
            description = "GTK3 image slideshow viewer with S3 support";
            license = licenses.mit;
            platforms = platforms.linux;
          };
        };
      }
    );
}
