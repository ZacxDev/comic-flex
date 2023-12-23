{ pkgs ? import <nixpkgs> {}, ... }:

pkgs.mkShell {
  buildInputs = with pkgs; [
    go
    gtk3
    pkg-config
    pkgsCross.raspberryPi.gcc 
  ];

  shellHook = ''
    export PKG_CONFIG_PATH=${pkgs.lib.makeSearchPathOutput "lib" "pkgconfig" [ pkgs.gtk3 ]}
  '';
}
