// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ipxe

import (
	"testing"
)

// Production iPXE scripts from profiles/osProfiles — verbatim copies.

// nke-260306-24.04-amd64-fde/ipxeScript
const productionAMD64FDEScript = `#!ipxe
set boot-loader https://example.test/nke-isos/qcow/efi/x86_64/2026-03-10/1773155704/qcow-imager.efi
set image-url https://example.test/nke-isos/qcow/images/24.04/x86_64/lvm/2026-03-04/1772660145/nke-24.04-x86_64-lvm-2026-03-04-1772660145.qcow2
set disk-strategy {{ .DiskSelectionStrategy }}

# You probably don't need to change these
set console-config console=tty0 console=ttyS1,115200
set kernel-params pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on
set luks-key luks.key=changeit
set cloud-init-ds ds=nocloud-net;s=${cloudinit-url}

chain ${boot-loader} ${console-config} ${kernel-params} ${luks-key} image_url=${image-url} disk_strategy=${disk-strategy} ${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes
boot
`

// nke-260306-24.04-arm64-fde/ipxeScript
const productionARM64FDEScript = `#!ipxe
set boot-loader https://example.test/nke-isos/qcow/efi/aarch64/2026-03-06/1772834088/qcow-imager.efi
set image-url https://example.test/nke-isos/qcow/images/24.04/aarch64/lvm/2026-03-04/1772595122/nke-24.04-aarch64-lvm-2026-03-04-1772595122.qcow2
set disk-strategy {{ .DiskSelectionStrategy }}

# You probably don't need to change these
set console-config console=ttyAMA0,115200
set kernel-params ip=dhcp fb=false pci=realloc=off loglevel=1 iommu=pt
set luks-key luks.key=changeit
set cloud-init-ds ds=nocloud-net;s=${cloudinit-url}

chain ${boot-loader} ${console-config} ${kernel-params} ${luks-key} image_url=${image-url} disk_strategy=${disk-strategy} ${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes
boot
`

func TestParseProductionAMD64FDEScript(t *testing.T) {
	spec, err := Parse(productionAMD64FDEScript)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if spec.ImageURL != "https://example.test/nke-isos/qcow/images/24.04/x86_64/lvm/2026-03-04/1772660145/nke-24.04-x86_64-lvm-2026-03-04-1772660145.qcow2" {
		t.Errorf("ImageURL = %q", spec.ImageURL)
	}
	if !spec.UpdateGrubTmpl {
		t.Error("UpdateGrubTmpl should be true")
	}
	if !spec.UpdateGrubCfg {
		t.Error("UpdateGrubCfg should be true")
	}
	if spec.LUKSKey != "luks.key=changeit" {
		t.Errorf("LUKSKey = %q", spec.LUKSKey)
	}
	if spec.ConsoleConfig != "console=tty0 console=ttyS1,115200" {
		t.Errorf("ConsoleConfig = %q", spec.ConsoleConfig)
	}
	if spec.KernelParams != "pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on" {
		t.Errorf("KernelParams = %q", spec.KernelParams)
	}
}

func TestParseProductionARM64FDEScript(t *testing.T) {
	spec, err := Parse(productionARM64FDEScript)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if spec.ImageURL != "https://example.test/nke-isos/qcow/images/24.04/aarch64/lvm/2026-03-04/1772595122/nke-24.04-aarch64-lvm-2026-03-04-1772595122.qcow2" {
		t.Errorf("ImageURL = %q", spec.ImageURL)
	}
	if !spec.UpdateGrubTmpl {
		t.Error("UpdateGrubTmpl should be true")
	}
	if spec.LUKSKey != "luks.key=changeit" {
		t.Errorf("LUKSKey = %q", spec.LUKSKey)
	}
	if spec.ConsoleConfig != "console=ttyAMA0,115200" {
		t.Errorf("ConsoleConfig = %q", spec.ConsoleConfig)
	}
	if spec.KernelParams != "ip=dhcp fb=false pci=realloc=off loglevel=1 iommu=pt" {
		t.Errorf("KernelParams = %q", spec.KernelParams)
	}
}

func TestParseNoChainLine(t *testing.T) {
	_, err := Parse("#!ipxe\nset foo bar\n")
	if err == nil {
		t.Fatal("expected error for script without chain command")
	}
}

func TestParseNoImageURL(t *testing.T) {
	_, err := Parse("#!ipxe\nchain http://example.com/boot\n")
	if err == nil {
		t.Fatal("expected error for script without image_url")
	}
}

func TestParseMinimal(t *testing.T) {
	script := `#!ipxe
set image-url https://example.com/image.qcow2
chain http://boot image_url=${image-url}
`
	spec, err := Parse(script)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if spec.ImageURL != "https://example.com/image.qcow2" {
		t.Errorf("ImageURL = %q", spec.ImageURL)
	}
	if spec.UpdateGrubTmpl {
		t.Error("UpdateGrubTmpl should be false")
	}
}
