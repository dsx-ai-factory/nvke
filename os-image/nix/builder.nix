# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
{
  modulesPath,
  pkgs,
  lib,
  ...
}:
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
{
  imports = [
    "${modulesPath}/profiles/minimal.nix"
    # modulesPath points into the locked nixpkgs source, not this repository.
    # This upstream module supplies the VM launcher and virtualisation options.
    "${modulesPath}/virtualisation/qemu-vm.nix"
  ];

  system.stateVersion = "26.05";
  networking.hostName = "nke-package-builder";
  networking.useDHCP = true;

  virtualisation = {
    diskSize = 32768;
    memorySize = 4096;
    cores = 4;
    graphics = false;
    useBootLoader = false;
    # Assemble the store filesystem with host tools, without booting a build VM.
    useNixStoreImage = true;
    writableStore = true;
    writableStoreUseTmpfs = false;
    qemu.options = [
      ''-fw_cfg name=opt/io.systemd.credentials/ssh.authorized_keys.root,file="$NKE_BUILDER_SSH_KEY"''
      ''-fw_cfg name=opt/io.systemd.credentials/nke.ssh.host-key,file="$NKE_BUILDER_HOST_KEY"''
    ];
  };
  boot.kernelModules = [ "qemu_fw_cfg" ];

  # systemd imports the public key supplied by the launcher at boot.
  services.openssh = {
    enable = true;
    generateHostKeys = false;
    hostKeys = [
      {
        type = "ed25519";
        path = "/run/credentials/sshd.service/nke.ssh.host-key";
      }
    ];
    settings = {
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
      PermitRootLogin = "prohibit-password";
    };
  };
  # Import the host-provisioned private key into sshd's runtime-only credentials.
  # No private key is embedded in the Nix store or trusted over the network.
  systemd.services.sshd.serviceConfig.LoadCredential = [ "nke.ssh.host-key" ];
  # The upstream f^ rule only initializes this file. Reused disks must accept
  # the public key supplied for this boot, including a switch to the managed runner.
  systemd.tmpfiles.settings."ssh-root-provision"."/root/.ssh/authorized_keys" = lib.mkForce {
    "f+^" = {
      user = "root";
      group = ":root";
      mode = ":600";
      argument = "ssh.authorized_keys.root";
    };
  };

  # Execute downloaded Linux tools such as kubeadm when they use an FHS loader.
  programs.nix-ld.enable = true;
  # build-guest.sh points CC at this wrapper, which Go honors for cgo builds.
  environment.systemPackages = with pkgs; [
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
  ];
}
