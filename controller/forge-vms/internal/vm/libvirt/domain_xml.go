// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package libvirt

import (
	"bytes"
	"text/template"
)

// DomainConfig holds the parameters used for rendering libvirt domain XML.
type DomainConfig struct {
	Name        string
	UUID        string // optional; when set, pins the domain UUID (used for deterministic swtpm state paths)
	VCPUs       int
	MemoryMiB   int
	OverlayPath string
	SeedISOPath string
	BridgeName  string
	NetworkName string // optional; when set, attach to this libvirt network instead of BridgeName
	NICMAC      string // optional; when set, pins the guest NIC MAC address
	Arch        string // "x86_64" or "aarch64"; defaults to x86_64
	UEFI        bool   // when true, uses OVMF firmware for UEFI boot
	TPM         bool   // when true, attaches a swtpm TPM 2.0 emulator device
	DomainType  string // "kvm" or "qemu"; defaults to "kvm", falls back to "qemu" if KVM unavailable
}

const domainTemplate = `<domain type='{{.DomainType}}'>
  <name>{{.Name}}</name>
{{- if .UUID}}
  <uuid>{{.UUID}}</uuid>
{{- end}}
  <memory unit='MiB'>{{.MemoryMiB}}</memory>
  <vcpu>{{.VCPUs}}</vcpu>
{{- if or (eq .Arch "aarch64") .UEFI}}
  <os firmware='efi'>
    <type arch='{{.Arch}}'{{if eq .Arch "aarch64"}} machine='virt'{{end}}>hvm</type>
    <boot dev='hd'/>
  </os>
{{- else}}
  <os>
    <type arch='{{.Arch}}'>hvm</type>
    <boot dev='hd'/>
  </os>
{{- end}}
{{- if eq .Arch "aarch64"}}
{{-   if eq .DomainType "kvm"}}
  <cpu mode='host-passthrough'/>
{{-   else}}
  <cpu mode='custom' match='exact'><model>cortex-a57</model></cpu>
{{-   end}}
{{- else}}
  <cpu mode='custom' match='exact'>
    <model>Nehalem</model>
  </cpu>
{{- end}}
  <features>
    <acpi/>
{{- if eq .Arch "aarch64"}}
    <gic version='3'/>
{{- else}}
    <apic/>
{{- end}}
  </features>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='{{.OverlayPath}}'/>
      <target dev='vda' bus='virtio'/>
    </disk>
{{- if .SeedISOPath}}
{{- if eq .Arch "aarch64"}}
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='{{.SeedISOPath}}'/>
      <target dev='vdb' bus='virtio'/>
      <readonly/>
    </disk>
{{- else}}
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='{{.SeedISOPath}}'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
{{- end}}
{{- end}}
{{- if .NetworkName}}
    <interface type='network'>
      <source network='{{.NetworkName}}'/>
      <model type='virtio'/>
{{- if .NICMAC}}
      <mac address='{{.NICMAC}}'/>
{{- end}}
    </interface>
{{- else}}
    <interface type='bridge'>
      <source bridge='{{.BridgeName}}'/>
      <model type='virtio'/>
{{- if .NICMAC}}
      <mac address='{{.NICMAC}}'/>
{{- end}}
    </interface>
{{- end}}
    <serial type='pty'>
      <log file='{{.OverlayPath}}.ttyS0.log' append='off'/>
      <target port='0'/>
    </serial>
    <serial type='pty'>
      <log file='{{.OverlayPath}}.ttyS1.log' append='off'/>
      <target port='1'/>
    </serial>
    <console type='pty'>
      <log file='{{.OverlayPath}}.ttyS0.log' append='off'/>
      <target type='serial' port='0'/>
    </console>
{{- if .TPM}}
    <tpm model='tpm-tis'>
      <backend type='emulator' version='2.0' persistent_state='yes'/>
    </tpm>
{{- end}}
  </devices>
</domain>`

var domainTmpl = template.Must(template.New("domain").Parse(domainTemplate))

// GenerateDomainXML renders libvirt domain XML for the provided config.
func GenerateDomainXML(cfg DomainConfig) string {
	if cfg.Arch == "" {
		cfg.Arch = "x86_64"
	}
	if cfg.DomainType == "" {
		cfg.DomainType = "kvm"
	}

	var buf bytes.Buffer
	_ = domainTmpl.Execute(&buf, cfg)
	return buf.String()
}
