// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package utils

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Metadata represents the metadata for an OSProfile
type Metadata struct {
	KubernetesVersion         string `yaml:"kubernetes_version"`
	KubernetesVersionInjected bool   `yaml:"kubernetes_version_injected"`
	Architecture              string `yaml:"architecture"`
	BuildDate                 string `yaml:"build_date"`
	UbuntuVersion             string `yaml:"ubuntu_version,omitempty"`
	Variant                   string `yaml:"variant,omitempty"`
	ImageBaseUrl              string `yaml:"image_base_url,omitempty"`
	BundleUrl                 string `yaml:"bundle_url,omitempty"`
}

// MetadataIndex represents the index of all OSProfile metadata
type MetadataIndex struct {
	Profiles           map[string]Metadata `yaml:"profiles"`
	DeprecatedProfiles map[string]Metadata `yaml:"-"`
}

// LoadOSProfileMetadata loads the OSProfile metadata from the metadata.yaml file
func LoadOSProfileMetadata(profilesDir string) (*MetadataIndex, error) {
	metadataPath := filepath.Join(profilesDir, "osProfiles", "metadata.yaml")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata file: %w", err)
	}

	var index MetadataIndex
	if err := yaml.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to parse metadata file: %w", err)
	}

	deprecatedPath := filepath.Join(profilesDir, "osProfiles", "deprecated.yaml")
	deprecatedData, err := os.ReadFile(deprecatedPath)
	if err != nil {
		if os.IsNotExist(err) {
			index.DeprecatedProfiles = make(map[string]Metadata)
			return &index, nil
		}
		return nil, fmt.Errorf("failed to read deprecated metadata file: %w", err)
	}

	var deprecatedIndex MetadataIndex
	if err := yaml.Unmarshal(deprecatedData, &deprecatedIndex); err != nil {
		return nil, fmt.Errorf("failed to parse deprecated metadata file: %w", err)
	}
	index.DeprecatedProfiles = deprecatedIndex.Profiles

	return &index, nil
}

// GetOSProfileMetadata returns the metadata for a specific OSProfile.
// If includeDeprecated is true, it will also return the metadata if it is in the deprecated profiles.
// This is useful to still find current OSProfiles that are deprecated.
func (idx *MetadataIndex) GetOSProfileMetadata(profileName string, includeDeprecated bool) (Metadata, bool) {
	meta, ok := idx.Profiles[profileName]
	if !ok && includeDeprecated {
		meta, ok = idx.DeprecatedProfiles[profileName]
	}
	return meta, ok
}

// FindOSProfiles returns all current (non-deprecated) OSProfiles matching the given criteria
//
// Note: The kubernetesVersion filter will also match KubernetesVersionInjected OSProfiles.
// This allows newer OSProfiles that support injecting the KubernetesVersion at provisioning time.
// to be used as upgrade targets so long as the rest of the criteria match.
func (idx *MetadataIndex) FindOSProfiles(kubernetesVersion, architecture, variant, ubuntuVersion string) map[string]Metadata {
	result := make(map[string]Metadata)
	for name, meta := range idx.Profiles {
		if !meta.KubernetesVersionInjected && meta.KubernetesVersion != kubernetesVersion {
			continue
		}
		if meta.Architecture != architecture {
			continue
		}
		if meta.Variant != variant {
			continue
		}
		if meta.UbuntuVersion != ubuntuVersion {
			continue
		}

		result[name] = meta
	}
	return result
}

// IsKubernetesVersionInjected returns true if the KubernetesVersion is injected at provisioning time.
//
// Prior to OSProfile 20260113, the KubernetesVersion was embedded into the OSProfile.
// Which required a different OS Image build for each os/arch/k8s version combination.
// From OSProfile 20260113+, the OSProfile contains a base OS and the KubernetesVersion
// can be injected into the node at provisioning time. So the NodePool.KubernetesVersion
// if not empty, contains the desired Kubernetes version for the node pool.
//
// Returns true if the KubernetesVersion is injected at provisioning time, and error.
func IsKubernetesVersionInjected(profilesDir, osProfile string) (bool, error) {
	// Load metadata to get K8s version from current OSProfile
	metadataIdx, err := LoadOSProfileMetadata(profilesDir)
	if err != nil {
		return false, fmt.Errorf("failed to load OSProfile metadata: %w", err)
	}

	currentMeta, ok := metadataIdx.GetOSProfileMetadata(osProfile, true)
	if !ok {
		return false, fmt.Errorf("OSProfile %q not found in metadata", osProfile)
	}

	return currentMeta.KubernetesVersionInjected, nil
}
