// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package seed

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v4"
)

// SiteBackend selects the runtime implementation for resources in a site.
type SiteBackend string

const (
	SiteBackendVM        SiteBackend = "vm"
	SiteBackendContainer SiteBackend = "container"
)

type Document struct {
	Tenants              []Tenant              `yaml:"tenants"`
	Sites                []Site                `yaml:"sites"`
	VPCs                 []VPC                 `yaml:"vpcs"`
	Subnets              []Subnet              `yaml:"subnets"`
	VPCPrefixes          []VPCPrefix           `yaml:"vpc_prefixes"`
	InstanceTypes        []InstanceType        `yaml:"instance_types"`
	SSHKeyGroups         []SSHKeyGroup         `yaml:"ssh_key_groups"`
	IPBlocks             []IPBlock             `yaml:"ip_blocks"`
	InfiniBandPartitions []InfiniBandPartition `yaml:"infiniband_partitions"`
}

type Tenant struct {
	ID          string `yaml:"id"`
	OrgID       string `yaml:"org_id"`
	DisplayName string `yaml:"display_name"`
}

type Site struct {
	ID           string      `yaml:"id"`
	Name         string      `yaml:"name"`
	TenantID     string      `yaml:"tenant_id"`
	Backend      SiteBackend `yaml:"backend"`
	Capabilities struct {
		NativeNetworking bool `yaml:"native_networking"`
	} `yaml:"capabilities"`
}

type VPC struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	TenantID string `yaml:"tenant_id"`
	SiteID   string `yaml:"site_id"`
}

type Subnet struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	VPCID        string `yaml:"vpc_id"`
	SiteID       string `yaml:"site_id"`
	CIDR         string `yaml:"cidr"`
	PrefixLength int    `yaml:"prefix_length"`
}

type VPCPrefix struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	VPCID        string `yaml:"vpc_id"`
	SiteID       string `yaml:"site_id"`
	PrefixLength int    `yaml:"prefix_length"`
	Prefix       string `yaml:"prefix"`
}

type InstanceType struct {
	ID        string `yaml:"id"`
	Name      string `yaml:"name"`
	SiteID    string `yaml:"site_id"`
	GPUCount  int    `yaml:"gpu_count"`
	CPUCount  int    `yaml:"cpu_count"`
	MemoryGiB int    `yaml:"memory_gib"`
	DiskGiB   int    `yaml:"disk_gib"`
}

type SSHKeyGroup struct {
	ID      string   `yaml:"id"`
	Name    string   `yaml:"name"`
	SiteIDs []string `yaml:"site_ids"`
}

type IPBlock struct {
	ID     string `yaml:"id"`
	Name   string `yaml:"name"`
	SiteID string `yaml:"site_id"`
}

type InfiniBandPartition struct {
	ID     string `yaml:"id"`
	Name   string `yaml:"name"`
	SiteID string `yaml:"site_id"`
}

func Load(path string) (Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("failed to read seed file: %w", err)
	}

	var doc Document
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Document{}, fmt.Errorf("failed to parse seed file: %w", err)
	}
	if err := doc.NormalizeAndValidate(); err != nil {
		return Document{}, err
	}

	return doc, nil
}

// NormalizeAndValidate applies backward-compatible defaults and rejects seed
// values that would make backend dispatch ambiguous. It intentionally mutates
// the document so callers that construct seed documents in code receive the
// same defaults as YAML-loaded documents.
func (d *Document) NormalizeAndValidate() error {
	for i := range d.Sites {
		backend := SiteBackend(strings.ToLower(strings.TrimSpace(string(d.Sites[i].Backend))))
		if backend == "" {
			backend = SiteBackendVM
		}
		switch backend {
		case SiteBackendVM, SiteBackendContainer:
			d.Sites[i].Backend = backend
		default:
			return fmt.Errorf("site %s has unsupported backend %q", d.Sites[i].ID, backend)
		}
	}
	return nil
}
