# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# The tools the package build runs with, for the Linux system of `pkgs`. The
# package-builder VM installs them system-wide (builder.nix), and the flake's
# package-build devShell provides the same list.
{ pkgs }:
let
  # Use the target ABI, rather than the builder's Nix-store loader and libc.
  ubuntuCC = pkgs.writeShellScriptBin "nke-ubuntu-cc" ''
    exec ${pkgs.zig_0_15}/bin/zig cc \
      -target ${pkgs.stdenv.hostPlatform.parsed.cpu.name}-linux-gnu.2.39 "$@"
  '';
  checkUbuntuElf = pkgs.writeShellApplication {
    name = "check-ubuntu-elf";
    runtimeInputs = with pkgs; [
      binutils
      gnugrep
      gnused
    ];
    text = builtins.readFile ./check-ubuntu-elf.sh;
  };
in
# build-guest.sh points CC at nke-ubuntu-cc, which Go honors for cgo builds.
with pkgs;
[
  ubuntuCC
  checkUbuntuElf
  binutils
  cacert
  dpkg
  file
  git
  gnumake
  gnupg
  go_1_26
  jq
  openssh
  rsync
]
