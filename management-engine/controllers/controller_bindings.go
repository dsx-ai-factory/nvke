// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Provisioning bindings supply deployment configuration for supported cluster selections.
package controllers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// ControllerBinding maps a namespace, release selection, and class to an existing NVKE controller request.
// An empty Version is an explicit default for requests that omit the channel version.
type ControllerBinding struct {
	Namespace         string                   `json:"namespace"`
	Channel           string                   `json:"channel"`
	Version           string                   `json:"version,omitempty"`
	ControlPlaneClass string                   `json:"controlPlaneClass"`
	Template          api.CreateClusterRequest `json:"template"`
}

// LoadControllerBindings reads a strict JSON array of trusted provisioning bindings.
func LoadControllerBindings(path string) ([]ControllerBinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read NVKE controller bindings: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var bindings []ControllerBinding
	if err := decoder.Decode(&bindings); err != nil {
		return nil, fmt.Errorf("NVKE controller bindings must be a JSON array with supported fields")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("NVKE controller bindings contain trailing data")
	}
	if len(bindings) == 0 {
		return nil, fmt.Errorf("NVKE controller bindings must contain at least one binding")
	}
	seen := map[[4]string]struct{}{}
	for i, binding := range bindings {
		if binding.Namespace == "" || binding.Channel == "" || binding.ControlPlaneClass == "" {
			return nil, fmt.Errorf("NVKE controller binding %d requires namespace, channel, and controlPlaneClass", i+1)
		}
		key := [4]string{binding.Namespace, binding.Channel, binding.Version, binding.ControlPlaneClass}
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("NVKE controller binding %d duplicates a selection", i+1)
		}
		seen[key] = struct{}{}
		// Cluster intent owns these fields. Bindings must not supply conflicting values.
		request := binding.Template
		if request.Name != "" || request.Network != nil || request.NodePools != nil {
			return nil, fmt.Errorf("NVKE controller binding %d: template must omit name, network, and nodePools", i+1)
		}
		if request.CniPlugin != "" && request.CniPlugin != api.CreateClusterRequestCniPluginOvnKubernetes {
			return nil, fmt.Errorf("NVKE controller binding %d: template CNI must be ovn-kubernetes", i+1)
		}
	}
	return bindings, nil
}
