# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
{
  description = "Direct-boot NixOS package-builder VMs for NKE";

  # Follow nixos-unstable intentionally; flake.lock pins the exact revision used.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      hostSystems = [
        "aarch64-darwin"
        "x86_64-linux"
        "aarch64-linux"
      ];
      guests = {
        amd64 = "x86_64-linux";
        arm64 = "aarch64-linux";
      };
      # Stable per-architecture defaults; NKE_BUILDER_SSH_PORT overrides.
      defaultSshPort = arch: if arch == "arm64" then "5422" else "5423";
      mkBuilder =
        hostSystem: arch: guestSystem:
        let
          hostPkgs = import nixpkgs { system = hostSystem; };
          guest = nixpkgs.lib.nixosSystem {
            system = guestSystem;
            modules = [
              ./nix/builder.nix
              {
                system.name = "nke-builder-${arch}";
                virtualisation.host.pkgs = hostPkgs;
              }
            ];
          };
          native =
            hostPkgs.stdenv.hostPlatform.parsed.cpu.name == guest.pkgs.stdenv.hostPlatform.parsed.cpu.name;
          machine = if arch == "arm64" then "virt,gic-version=max" else "q35";
        in
        hostPkgs.writeShellApplication {
          name = "nke-builder-${arch}";
          runtimeInputs = [
            hostPkgs.coreutils
            hostPkgs.gnugrep
          ];
          # Runtime wrapper: attach the key/cache disk and choose native acceleration
          # when available, with software emulation as the fallback.
          text = ''
            export NKE_BUILDER_SSH_KEY
            NKE_BUILDER_SSH_KEY=$(realpath "''${NKE_BUILDER_SSH_KEY:?Set NKE_BUILDER_SSH_KEY to a public SSH key file}")
            test -r "$NKE_BUILDER_SSH_KEY"
            export NKE_BUILDER_HOST_KEY
            NKE_BUILDER_HOST_KEY=$(realpath "''${NKE_BUILDER_HOST_KEY:?Set NKE_BUILDER_HOST_KEY to a private SSH host key file}")
            test -r "$NKE_BUILDER_HOST_KEY"
            state_dir="''${NKE_BUILDER_STATE_DIR:-''${XDG_CACHE_HOME:-$HOME/.cache}/nke/builders/${arch}}"
            mkdir -p "$state_dir"
            export NIX_DISK_IMAGE="''${NIX_DISK_IMAGE:-$state_dir/root.qcow2}"
            accel=tcg
            ${nixpkgs.lib.optionalString native (
              if hostPkgs.stdenv.hostPlatform.isDarwin then
                ''
                  if /usr/sbin/sysctl -n kern.hv_support | grep -q 1; then accel=hvf; fi
                ''
              else
                ''
                  if [[ -r /dev/kvm && -w /dev/kvm ]]; then accel=kvm; fi
                ''
            )}
            accel=''${ACCELERATOR:-$accel}
            case "$accel" in
              tcg) cpu=${if arch == "arm64" then "max,pauth-impdef=on" else "max"} ;;
              kvm|hvf) cpu=host ;;
              *) echo "ACCELERATOR must be kvm, hvf, or tcg" >&2; exit 1 ;;
            esac
            export QEMU_NET_OPTS="hostfwd=tcp:127.0.0.1:''${NKE_BUILDER_SSH_PORT:-${defaultSshPort arch}}-:22"
            exec ${guest.config.system.build.vm}/bin/run-nke-builder-${arch}-vm \
              -machine ${machine},accel="$accel" -cpu "$cpu" "$@"
          '';
        };
      mkPackageApp =
        hostSystem: arch:
        let
          pkgs = import nixpkgs { system = hostSystem; };
          launcher = self.packages.${hostSystem}."builder-${arch}";
          ssh = pkgs.writeShellApplication {
            name = "nke-builder-ssh";
            runtimeInputs = [ pkgs.openssh ];
            text = ''
              exec ssh -F /dev/null -p "$NKE_BUILDER_SSH_PORT" \
                -i "$NKE_BUILDER_STATE_DIR/client-key" \
                -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes \
                -o ConnectTimeout=2 -o StrictHostKeyChecking=yes \
                -o GlobalKnownHostsFile=/dev/null \
                -o HostKeyAlias=nke-package-builder \
                -o "UserKnownHostsFile=$NKE_BUILDER_STATE_DIR/known_hosts" "$@"
            '';
          };
        in
        pkgs.writeShellApplication {
          name = "build-packages-${arch}";
          runtimeInputs = [
            pkgs.coreutils
            pkgs.flock
            ssh
            pkgs.gitMinimal
            pkgs.openssh
            pkgs.rsync
          ];
          text = ''
            arch=${arch}
            launcher=${launcher}/bin/nke-builder-${arch}
            guest_script=${./nix/build-guest.sh}
            default_ssh_port=${defaultSshPort arch}
          ''
          + builtins.readFile ./nix/build-packages.sh;
        };
    in
    {
      packages = nixpkgs.lib.genAttrs hostSystems (
        hostSystem:
        (nixpkgs.lib.mapAttrs' (
          arch: guestSystem:
          nixpkgs.lib.nameValuePair "builder-${arch}" (mkBuilder hostSystem arch guestSystem)
        ) guests)
        // (nixpkgs.lib.mapAttrs' (
          arch: _: nixpkgs.lib.nameValuePair "build-packages-${arch}" (mkPackageApp hostSystem arch)
        ) guests)
      );
    };
}
