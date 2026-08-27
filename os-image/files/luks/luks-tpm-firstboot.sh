#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# LUKS TPM2 first-boot binding.
# - Grows partition, LUKS, PV, LV, and filesystem to use the full disk.
# - Binds the LUKS volume to the local TPM via Clevis, then removes the
#   build-time passphrase slot (TPM-only unlock).
#
# This image always ships with the TPM-only policy (no PCR binding) and
# passphrase removal enabled — see os-image/docs/full-disk-encryption.md
# for the tradeoff and how to change it.

set -euo pipefail
exec &> >(tee -a /var/log/luks-tpm-firstboot.log)

echo "$(date): Starting first-boot configuration..."

# Set by configure-fde.sh into /etc/luks-tpm-firstboot.env (see
# luks-tpm-firstboot.service's EnvironmentFile=), matching the password set
# in autoinstall-fde.pkrtpl's storage.layout at build time. Not hardcoded
# here: a build with a non-default LUKS_PASSPHRASE would otherwise silently
# ship a script with the wrong value, only discovered when first-boot
# binding fails on real target hardware.
: "${LUKS_PASSPHRASE:?LUKS_PASSPHRASE must be set (see /etc/luks-tpm-firstboot.env)}"
# Empty = bind to the local TPM key hierarchy only (no measured-boot PCR
# policy). This survives firmware/GRUB/kernel updates without a re-seal step.
# Set e.g. "7" to add a Secure-Boot-state PCR policy; see the docs for the
# re-seal workflow that then becomes necessary on every relevant update.
PCR_IDS=""
REMOVE_PASSPHRASE="true"

# ============================================================================
# Extend storage to use all available disk space
# Order: partition -> LUKS -> PV -> LV -> filesystem
# ============================================================================
echo "Extending storage to use all available space..."

ROOT_VG="ubuntu-vg"
ROOT_LV="ubuntu-lv"

PV_DEVICE=$(pvs --noheadings -o pv_name -S vg_name=${ROOT_VG} 2>/dev/null | tr -d ' ')
echo "PV device: ${PV_DEVICE}"

if [[ -n "${PV_DEVICE}" ]]; then
    # This script only ever runs on a LUKS-backed PV: configure-fde.sh
    # requires at least one crypto_LUKS device before it will install or
    # enable luks-tpm-firstboot.service at all (autoinstall-lvm.pkrtpl
    # always sets luks_enabled = true for this variant). A non-LUKS PV here
    # would mean something upstream is broken, not a legitimate second case
    # to handle — the non-FDE variant has its own separate, LUKS-free
    # lvm-growth-firstboot.sh for that.
    if [[ "${PV_DEVICE}" != /dev/mapper/* ]]; then
        echo "Warning: expected a LUKS mapper device for PV ${PV_DEVICE}, got something else. Skipping storage resize."
    else
        LUKS_NAME=$(basename "${PV_DEVICE}")
        echo "LUKS device detected: ${LUKS_NAME}"

        LUKS_BACKING=$(cryptsetup status "${LUKS_NAME}" 2>/dev/null | grep "device:" | awk '{print $2}')
        echo "LUKS backing device: ${LUKS_BACKING}"

        if [[ -n "${LUKS_BACKING}" ]]; then
            if [[ "${LUKS_BACKING}" =~ ^/dev/nvme ]]; then
                DISK=$(echo "${LUKS_BACKING}" | sed 's/p[0-9]*$//')
                PART_NUM=$(echo "${LUKS_BACKING}" | grep -oE 'p[0-9]+$' | tr -d 'p')
            else
                DISK=$(echo "${LUKS_BACKING}" | sed 's/[0-9]*$//')
                PART_NUM=$(echo "${LUKS_BACKING}" | grep -oE '[0-9]+$')
            fi

            echo "Disk: ${DISK}, Partition: ${PART_NUM}"

            echo "Growing partition ${DISK} partition ${PART_NUM}..."
            udevadm settle 2>/dev/null || true
            # sgdisk -e moves the backup GPT header to the end of the disk first —
            # required when the disk was expanded after the image was written.
            sgdisk -e "${DISK}" 2>/dev/null || true
            if parted -s "${DISK}" resizepart "${PART_NUM}" 100% 2>/dev/null; then
                echo "Partition grown successfully."
                partx -u "${DISK}" 2>/dev/null || true
                sleep 1
            else
                echo "Partition already at maximum size or parted failed."
            fi

            echo "Resizing LUKS container ${LUKS_NAME}..."
            LUKS_RESIZE_OUTPUT=$(echo -n "${LUKS_PASSPHRASE}" | cryptsetup resize "${LUKS_NAME}" 2>&1) && LUKS_RESIZE_RC=0 || LUKS_RESIZE_RC=$?
            if [[ ${LUKS_RESIZE_RC} -ne 0 ]]; then
                echo "Passphrase resize failed (exit code: ${LUKS_RESIZE_RC}): ${LUKS_RESIZE_OUTPUT}"
                echo "Attempting resize via clevis/TPM..."
                CLEVIS_SLOT=$(cryptsetup luksDump "${LUKS_BACKING}" 2>/dev/null | awk '/^Tokens:/,0' | grep -A1 'clevis' | grep 'Keyslot:' | awk '{print $2}' | head -1)
                if [[ -n "${CLEVIS_SLOT}" ]]; then
                    LUKS_RESIZE_OUTPUT=$(clevis luks pass -d "${LUKS_BACKING}" -s "${CLEVIS_SLOT}" 2>/dev/null | cryptsetup resize --key-file=- "${LUKS_NAME}" 2>&1) && LUKS_RESIZE_RC=0 || LUKS_RESIZE_RC=$?
                fi
            fi
            if [[ ${LUKS_RESIZE_RC} -eq 0 ]]; then
                echo "LUKS container resized successfully."
            else
                echo "LUKS resize failed (exit code: ${LUKS_RESIZE_RC})"
                echo "LUKS resize output: ${LUKS_RESIZE_OUTPUT}"
            fi
        fi
    fi

    echo "Resizing PV ${PV_DEVICE}..."
    if pvresize "${PV_DEVICE}" 2>/dev/null; then
        echo "PV resized successfully."
    else
        echo "PV resize failed or already at maximum size."
    fi

    echo "Extending LV /dev/${ROOT_VG}/${ROOT_LV}..."
    if lvextend -l +100%FREE "/dev/${ROOT_VG}/${ROOT_LV}" 2>/dev/null; then
        echo "LV extended successfully."
    else
        echo "LV already at maximum size or no free space in VG."
    fi

    echo "Resizing filesystem..."
    if resize2fs "/dev/${ROOT_VG}/${ROOT_LV}" 2>/dev/null; then
        echo "Filesystem resized successfully."
    else
        echo "Filesystem resize failed."
    fi
else
    echo "Warning: Could not find PV for VG ${ROOT_VG}"
fi

echo ""
echo "Current disk usage:"
df -h /

# ============================================================================
# Clean up temporary build keyfile (from the Packer/autoinstall build)
# ============================================================================
BUILD_KEYFILE="/etc/luks-build.key"
if [[ -f "${BUILD_KEYFILE}" ]]; then
    echo "Cleaning up temporary build keyfile..."
    for dev in $(lsblk -ln -o NAME,FSTYPE | grep crypto_LUKS | awk '{print "/dev/" $1}'); do
        cryptsetup luksRemoveKey "${dev}" "${BUILD_KEYFILE}" 2>/dev/null || true
    done
    rm -f "${BUILD_KEYFILE}"
    echo "Build keyfile removed."
fi

# ============================================================================
# TPM Binding
# ============================================================================
echo ""
echo "$(date): Starting LUKS TPM2 binding..."

if [[ ! -c /dev/tpm0 ]]; then
    echo "WARNING: TPM device /dev/tpm0 not found!"
    echo "TPM binding skipped - system will continue to use existing unlock method."
    echo "Ensure TPM2 is enabled in BIOS/UEFI for production deployments."
    echo "$(date): TPM not available, skipping binding." >> /var/log/luks-tpm-firstboot.log
    # Leave the service enabled so it retries on the next boot.
    exit 0
fi

# Detect the strongest PCR bank supported by this TPM (only used if PCR_IDS
# is non-empty). Tries sha512 -> sha384 -> sha256 so the same image works on
# sha256-only and sha384-only TPMs without baking the bank at build time.
detect_pcr_bank() {
    local target_pcr="${PCR_IDS%%,*}"
    for bank in sha512 sha384 sha256; do
        local pcr_out
        pcr_out=$(tpm2_pcrread "${bank}:0" 2>/dev/null) || continue
        if ! echo "${pcr_out}" | grep -qE '0x0*[1-9a-fA-F]'; then
            continue
        fi
        local target_out
        target_out=$(tpm2_pcrread "${bank}:${target_pcr}" 2>/dev/null) || continue
        if echo "${target_out}" | grep -qE '0x0*[1-9a-fA-F]'; then
            echo "${bank}"
            return 0
        fi
    done
    for bank in sha512 sha384 sha256; do
        tpm2_pcrread "${bank}:0" 2>/dev/null | grep -qE '0x0*[1-9a-fA-F]' && { echo "${bank}"; return 0; }
    done
    echo "sha256"
}

if [[ -n "${PCR_IDS}" ]]; then
    PCR_BANK=$(detect_pcr_bank)
    TPM_POLICY=$(printf '{"pcr_bank":"%s","pcr_ids":"%s"}' "${PCR_BANK}" "${PCR_IDS}")
    echo "TPM PCR bank: ${PCR_BANK}, PCRs: ${PCR_IDS}"
else
    PCR_BANK=""
    TPM_POLICY='{}'
    echo "TPM policy: local TPM only (no PCR binding)"
fi

PCR_FILE="/root/.pcr"
echo "Recording TPM policy context to ${PCR_FILE}..."
{
    echo "# TPM policy context at time of LUKS binding"
    echo "# Generated: $(date -Iseconds)"
    echo "# Policy: ${TPM_POLICY}"
    echo ""
    if [[ -n "${PCR_IDS}" ]]; then
        echo "# PCR Bank: ${PCR_BANK}"
        echo "# PCR IDs: ${PCR_IDS}"
        echo ""
        for pcr in $(echo "${PCR_IDS}" | tr ',' ' '); do
            echo "# PCR ${pcr}:"
            if command -v tpm2_pcrread &>/dev/null; then
                tpm2_pcrread "${PCR_BANK}:${pcr}" 2>/dev/null || echo "  (failed to read)"
            else
                echo "  (tpm2_pcrread not available)"
            fi
            echo ""
        done
        echo "# Full PCR state (${PCR_BANK} bank):"
        if command -v tpm2_pcrread &>/dev/null; then
            tpm2_pcrread "${PCR_BANK}" 2>/dev/null || echo "  (failed to read)"
        fi
    else
        echo "# PCR binding: disabled"
        echo "# Unlock is bound to the local TPM key hierarchy only."
        if command -v tpm2_pcrread &>/dev/null; then
            echo ""
            echo "# Current PCR state is informational only:"
            tpm2_pcrread 2>/dev/null || echo "  (failed to read)"
        fi
    fi
} > "${PCR_FILE}"

chmod 600 "${PCR_FILE}"
echo "TPM policy context recorded to ${PCR_FILE}"
cat "${PCR_FILE}"

LUKS_DEVICES=$(lsblk -ln -o NAME,FSTYPE | grep crypto_LUKS | awk '{print "/dev/" $1}')

if [[ -z "${LUKS_DEVICES}" ]]; then
    echo "No LUKS devices found."
    exit 0
fi

echo "Found LUKS devices: ${LUKS_DEVICES}"

for dev in ${LUKS_DEVICES}; do
    echo "Processing ${dev}..."

    if clevis luks list -d "${dev}" 2>/dev/null | grep -q tpm2; then
        echo "  Already bound to TPM2, skipping."
        continue
    fi

    echo "  Binding to TPM2..."
    if echo -n "${LUKS_PASSPHRASE}" | clevis luks bind -d "${dev}" -k - tpm2 "${TPM_POLICY}"; then
        echo "  Successfully bound to TPM2."

        # `clevis luks bind` returning 0 means the metadata was written; it
        # does NOT mean the TPM can unseal the key. Verify before removing
        # the passphrase fallback.
        echo "  Verifying Clevis can unseal the LUKS key..."
        VERIFY_SLOT=$(clevis luks list -d "${dev}" 2>/dev/null | grep tpm2 | awk -F: '{print $1}' | tr -d ' ' | head -1)
        if [[ -n "${VERIFY_SLOT}" ]] && clevis luks pass -d "${dev}" -s "${VERIFY_SLOT}" 2>/dev/null | cryptsetup luksOpen --test-passphrase "${dev}" --key-file=- 2>/dev/null; then
            echo "  Clevis unseal verified OK."
        else
            echo "  ERROR: clevis luks bind succeeded but unseal verification FAILED."
            echo "  Keeping passphrase slot intact to avoid lockout."
            if [[ -n "${PCR_IDS}" ]]; then
                echo "  Current PCR values:"
                tpm2_pcrread "${PCR_BANK}:${PCR_IDS}" 2>/dev/null || true
            fi
            CLEVIS_SLOT=$(clevis luks list -d "${dev}" 2>/dev/null | grep tpm2 | awk -F: '{print $1}' | tr -d ' ' | head -1)
            if [[ -n "${CLEVIS_SLOT}" ]]; then
                echo -n "${LUKS_PASSPHRASE}" | cryptsetup luksKillSlot "${dev}" "${CLEVIS_SLOT}" --key-file=- 2>/dev/null || true
                echo "  Rolled back Clevis binding (slot ${CLEVIS_SLOT})."
            fi
            exit 1
        fi

        if [[ "${REMOVE_PASSPHRASE}" == "true" ]]; then
            echo "  Removing passphrase slot..."
            if echo -n "${LUKS_PASSPHRASE}" | cryptsetup luksRemoveKey "${dev}" -; then
                echo "  Passphrase slot removed."
            else
                echo "  Warning: Failed to remove passphrase (may already be removed)."
            fi
        fi
    else
        echo "  ERROR: Failed to bind to TPM2!"
        exit 1
    fi
done

echo "Updating crypttab for clevis-based unlock..."
if [[ -f /etc/crypttab ]]; then
    LUKS_UUID=$(blkid -t TYPE=crypto_LUKS -s UUID -o value | head -1)
    if [[ -n "${LUKS_UUID}" ]]; then
        echo "luks-${LUKS_UUID} UUID=${LUKS_UUID} none luks,initramfs" > /etc/crypttab
        echo "Updated crypttab with luks-<UUID> naming:"
        cat /etc/crypttab
    else
        if ! grep -q 'initramfs' /etc/crypttab; then
            sed -i 's/luks$/luks,initramfs/' /etc/crypttab
            echo "Added 'initramfs' option to crypttab"
        fi
        cat /etc/crypttab
    fi
fi

echo "Removing luks-cmdline initramfs script (replaced by clevis)..."
if [[ -f /etc/initramfs-tools/scripts/local-top/luks-cmdline ]]; then
    rm -f /etc/initramfs-tools/scripts/local-top/luks-cmdline
    echo "Removed luks-cmdline script"
fi

echo "Updating initramfs..."
update-initramfs -u -k all

echo "Cleaning up GRUB configuration..."
GRUB_UPDATED=false

# autoinstall-lvm.pkrtpl always writes luks.key= to
# /etc/default/grub.d/99-luks-key.cfg, never to /etc/default/grub directly —
# glob the drop-in dir rather than hardcode that filename, in case it ever
# changes.
for dropin in /etc/default/grub.d/*.cfg; do
    if [[ -f "$dropin" ]] && grep -q 'luks.key=' "$dropin" 2>/dev/null; then
        sed -i 's/ luks.key=[^ "]*//g' "$dropin"
        echo "Removed luks.key from $dropin"
        GRUB_UPDATED=true
    fi
done

if [[ "$GRUB_UPDATED" == "true" ]]; then
    update-grub
    echo "GRUB configuration updated."
else
    echo "No luks.key found in GRUB config."
fi

echo "$(date): LUKS TPM2 binding complete!"

# Success marker: only written on the success path — a failed bind leaves it
# absent so the still-enabled service retries on the next boot.
FIRSTBOOT_DONE_MARKER="/root/.luks-tpm-firstboot-done"
date -Iseconds > "${FIRSTBOOT_DONE_MARKER}"
chmod 600 "${FIRSTBOOT_DONE_MARKER}"
echo "Wrote completion marker: ${FIRSTBOOT_DONE_MARKER}"

systemctl disable luks-tpm-firstboot.service
echo "First-boot service disabled."

# LUKS_PASSPHRASE is moot once the passphrase slot above is removed — the
# env file that held it (see configure-fde.sh) has no further use.
rm -f /etc/luks-tpm-firstboot.env
echo "Removed /etc/luks-tpm-firstboot.env."
