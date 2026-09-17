# Full disk encryption (FDE)

Host volumes on the FDE image variant are LUKS2-encrypted, with the LUKS key
sealed to that node's own TPM 2.0. This satisfies the host-encryption half of
NFR-11/IAM-7 (data-at-rest on nodes). Kubernetes Secrets/etcd
encryption-at-rest is a separate, controller/agent-side concern and is out of
scope for `os-image/` — see the controller's etcd-encryption workflows for
that half.

This is a plain-shell Packer build, `nke-node-fde.pkr.hcl`.

## How it works

1. **Image build** (`autoinstall-fde.pkrtpl`): the Ubuntu Server installer
   lays down an LVM-on-LUKS2 root volume (`ubuntu-vg`/`ubuntu-lv`), unlocked
   at build time by a shared, documented passphrase (`changeit` by default,
   overridable via `LUKS_PASSPHRASE` — **set this to a real value for any
   build that will run on real hardware; `changeit` is a build-time-only
   placeholder, not a secret**). A temporary keyfile lets the
   post-autoinstall reboot and Packer's SSH provisioning happen unattended;
   `configure-fde.sh` removes that keyfile before the image ships, leaving
   only the passphrase slot.

   That same `LUKS_PASSPHRASE` also gets baked into the shipped disk's GRUB
   config as a `luks.key=<passphrase>` kernel parameter (see "Building and
   testing locally" below for the mechanism) — this is what lets a node's
   very first real boot unlock unattended, before it has anything TPM-bound
   yet. It is a real, unlock-capable secret on the disk for that window, not
   just a build-time convenience: anyone with console/physical access to an
   unbound node could read it out of GRUB and unlock the volume with it.
   `luks-tpm-firstboot.sh` strips `luks.key=` from GRUB automatically once
   TPM binding is verified (see step 3 below), which is what closes that
   window — until that happens, a shared or default `LUKS_PASSPHRASE` is a
   real exposure, not just bad hygiene.
2. **Provisioning** (`configure-fde.sh`): installs `clevis`/`tpm2-tools`, the
   initramfs scripts that make Clevis unlock work reliably (waiting for the
   block device and the TPM to appear, copying `crypttab` into the
   initramfs), a kernel-postinst hook that re-binds after kernel updates, and
   enables `luks-tpm-firstboot.service`. It does **not** bind to a TPM —
   binding is machine-specific and this VM's TPM (if any) is not the target
   node's TPM.
3. **First boot on target hardware** (`luks-tpm-firstboot.sh`, run once by
   `luks-tpm-firstboot.service`): grows the partition/LUKS/PV/LV/filesystem
   to the real disk's full size, binds the LUKS key to the local TPM via
   Clevis, **verifies** the TPM can actually unseal it (`clevis luks pass |
   cryptsetup luksOpen --test-passphrase`) before touching anything
   irreversible, and only then removes the passphrase slot. If verification
   fails, the passphrase slot is kept and the service stays enabled to retry
   on the next boot — this is what stands between a bad bind and a bricked
   node.
4. **Node replacement**: a replacement node boots the same image and runs
   the same first-boot flow against its own TPM. There is no old-TPM/new-TPM
   migration step — each node seals its own key independently.

## No key material off the image

The Gherkin acceptance spec requires that no key material appear in
cloud-init, user-data, or the genesis medium. This design satisfies that:

- The only thing on the autoinstall/cidata HTTP payload is the shared,
  documented build-time passphrase and the storage layout — not a per-node
  secret, and not TPM-derived material.
- The build-time keyfile used to survive the unattended post-install reboot
  lives only on the build VM's own disk and is deleted by `configure-fde.sh`
  before the qcow2 is published.
- The host-side QCOW2 compression and collection paths only replace or copy
  the verified disk bytes and do not serialize the process environment.
- The TPM binding itself happens entirely on-node, driven by the local TPM;
  nothing about it is templated in from outside.

**This is narrower than "no unlock secret exists on the node at all."** The
same build-time `LUKS_PASSPHRASE` is present both as a LUKS passphrase slot
and as a `luks.key=` GRUB parameter on the shipped disk itself (not on any
external medium) until the first successful TPM bind removes both — see the
callout in "How it works" step 1 above. Off-medium and off-node are
different guarantees; this design only makes the first one.

## Policy: TPM-only, no PCR binding, passphrase removed

By default this image binds to the **local TPM key hierarchy only** — no
measured-boot PCR policy — and **removes the passphrase slot** once the bind
is verified. This is a deliberate tradeoff:

- **Pro**: firmware, GRUB, and kernel updates don't change PCR values, so
  there's no re-seal step to build or operate. `luks-tpm-firstboot.sh`'s PCR
  logic is still present (dead by default, `PCR_IDS=""`) if a fork wants to
  opt into PCR binding — see the comment at the top of that script for what
  each PCR measures and how stable it is.
- **Con**: there is **no recovery path**. If the node's TPM fails, is reset,
  or its state otherwise stops satisfying the seal, the volume is
  **permanently unreadable** — moving it to different hardware fails for the
  same reason. Re-imaging is the only remediation.

## Verifying encryption status

On a running node with console/SSH access:

```bash
sudo /usr/local/bin/luks-tpm-verify
```

Exit codes: `0` = all LUKS devices are TPM-bound and currently unsealable by
this TPM; `1` = a device has no TPM binding; `2` = a binding exists but this
TPM cannot unseal it right now (lockout risk if no passphrase slot remains);
`3` = missing TPM device or tooling; `4` = no LUKS devices found. Run this
right after first boot, before trusting TPM-only unlock in production.

Other useful checks:

```bash
# Confirm first-boot binding completed successfully.
cat /root/.luks-tpm-firstboot-done

# First-boot binding log.
cat /var/log/luks-tpm-firstboot.log

# Clevis bindings on the LUKS device.
LUKS_DEV=$(lsblk -ln -o NAME,FSTYPE | grep crypto_LUKS | awk '{print "/dev/" $1}' | head -1)
sudo clevis luks list -d "${LUKS_DEV}"
```

## TPM prerequisites

- TPM 2.0, exposed as `/dev/tpm0` (and ideally `/dev/tpmrm0`) to the OS.
- If no TPM is present at first boot, `luks-tpm-firstboot.sh` logs a warning
  and exits without binding, leaving the service enabled to retry on the
  next boot; the node keeps booting via the passphrase slot in the meantime.

## Building and testing locally

```bash
cd packer
make os-image-fde NKE_AGENT_VERSION=edge
```

then run `../demo-fde.sh` (see its `--help`) to exercise the acceptance
scenarios against the built image under QEMU with `swtpm`. Requires
`qemu-system-x86_64`, `swtpm`, `cloud-localds`, and OVMF
(`/usr/share/OVMF/OVMF_CODE_4M.fd` + `OVMF_VARS_4M.fd`) on `PATH`/in the
default location. No interactive/scripted console typing is needed: the
autoinstall storage layout bakes a build-time `luks.key=<passphrase>` GRUB
parameter, so `files/luks/luks-cmdline-script` unlocks the very first real
boot on its own — the same mechanism production uses via an
iPXE `luks.key=` kernel parameter. `luks-tpm-firstboot.sh` strips
`luks.key=` from GRUB once the TPM bind succeeds, so a later boot against a
different TPM has no fallback and correctly fails to unlock.

The autoinstall layout also bakes `console=tty0 console=ttyS0,115200` into
GRUB, so boot and kernel messages — including
the LUKS unlock prompt on a boot that *doesn't* have `luks.key=` anymore —
reach a serial console (`virsh console`, `-nographic`, real BMC/SOL access)
and not just the VGA framebuffer. Earlier revisions of this variant shipped
with no `console=` parameter at all, which is why an earlier design here
needed `expect`-driven blind console typing; that's no longer necessary.

This exact flow (fresh boot → TPM bind/verify/passphrase-removal → a
different swtpm can't unlock → a later boot on the original swtpm auto-
unlocks) was run end-to-end against Ubuntu 26.04 while building this
variant. One thing worth knowing if you're debugging a failed build:
Ubuntu 26.04 installs `dracut` alongside `initramfs-tools`, and the
installer's own post-partition boot briefly uses a dracut-generated initrd
(visible as `dracut-cmdline.service` etc. in early boot logs) — but
`configure-fde.sh`'s `update-initramfs -c -k all` overwrites
`/boot/initrd.img-<kernver>` with an initramfs-tools image, which is what
GRUB actually boots into afterward, so the initramfs-tools-based Clevis
integration this variant relies on is unaffected. If a future Ubuntu release
drops initramfs-tools in favor of dracut by default, this will need a
dracut-module rework (`clevis-dracut` instead of `clevis-initramfs`, and
dracut's own crypttab/naming conventions instead of `luks-<UUID>`).
