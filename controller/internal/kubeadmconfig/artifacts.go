// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kubeadmconfig

// BootstrapArtifacts contains the kubeadm YAML plus any prerequisite files
// that must exist on disk before kubeadm runs.
type BootstrapArtifacts struct {
	// ConfigYAML is the config to be passed to kubeadm init/join via --config.
	// It might contain more than one document (e.g. kubeadm and kubelet).
	ConfigYAML []byte
	// Files is a list of files to be written to disk before kubeadm runs.
	// These are typically files referenced by the control plane static pod manifests.
	Files []File
}

// File is a spec for file to be written to disk.
type File struct {
	Path    string
	Content []byte
	Mode    uint32
	Owner   string
}
