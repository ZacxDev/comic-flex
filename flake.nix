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

        # revision is what GET /healthz reports for a nix-built binary.
        #
        # 🔴 It has to be injected. buildGoModule builds from a /nix/store copy of
        # the source, which has NO .git, so `-buildvcs=auto` stamps nothing and
        # main.resolveVersion falls all the way through to "unknown" — while
        # TestVersionIsNotEmpty passes, because "unknown" is not empty. The
        # comment on main.version claimed "an un-edited build still identifies
        # itself"; for `nix build .#default` that was false until this line.
        # (A plain `go build` in a git clone — which is how the Pi builds — does
        # stamp correctly, which is why it went unnoticed.)
        revision = self.shortRev or self.dirtyShortRev or "unknown";

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
          version = revision;
          src = ./.;

          # 🔴 Was `null`, which tells buildGoModule the source vendors its own
          # deps. It does not — there is no vendor/ directory — so the build
          # phase died with "is explicitly required in go.mod, but not marked as
          # explicit in vendor/modules.txt" for every dependency. Between this
          # and the CGO_ENABLED clash below, `nix build .#default` had not
          # produced a binary at all; the "unknown" version it would have
          # reported was the second problem, not the first.
          vendorHash = "sha256-HnXdH+hmLXue7E9oshGxJ43ggW8icktCN0Tbp4MovhM=";

          inherit nativeBuildInputs buildInputs;

          # 🔴 `env.CGO_ENABLED`, not a bare `CGO_ENABLED`. buildGoModule now
          # sets CGO_ENABLED inside `env` itself, and passing the same name as a
          # top-level derivation argument is an EVALUATION error:
          #   "The `env` attribute set cannot contain any attributes passed to
          #    derivation. The following attributes are overlapping: CGO_ENABLED"
          # so `nix build .#default` did not merely produce a binary reporting
          # "unknown" — it did not evaluate at all. (`nix develop` was unaffected,
          # which is why the shell kept working.)
          env.CGO_ENABLED = 1;

          # Stamp the version main.resolveVersion prefers. Without this the store
          # build has no .git to read and /healthz reports "unknown".
          ldflags = [ "-X main.injectedVersion=${revision}" ];

          meta = with pkgs.lib; {
            description = "GTK3 image slideshow viewer with S3 support";
            license = licenses.mit;
            platforms = platforms.linux;
          };
        };
      }
    );
}
