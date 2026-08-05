// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package seed

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v4"
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
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	TenantID     string `yaml:"tenant_id"`
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

	return doc, nil
}
