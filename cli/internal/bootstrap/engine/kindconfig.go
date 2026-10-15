// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// kindClusterConfig mirrors the subset of kind's Cluster config
// (kind.x-k8s.io/v1alpha4) this package generates. Single control-plane
// node: the bootstrap cluster only ever hosts the controller for the duration of one
// `nvkectl bootstrap site` run, it is not a multi-node target.
type kindClusterConfig struct {
	Kind       string          `yaml:"kind"`
	APIVersion string          `yaml:"apiVersion"`
	Nodes      []kindNodeEntry `yaml:"nodes"`
}

type kindNodeEntry struct {
	Role              string           `yaml:"role"`
	Image             string           `yaml:"image,omitempty"`
	ExtraMounts       []kindMountEntry `yaml:"extraMounts,omitempty"`
	ExtraPortMappings []kindPortEntry  `yaml:"extraPortMappings,omitempty"`
}

type kindMountEntry struct {
	HostPath      string `yaml:"hostPath"`
	ContainerPath string `yaml:"containerPath"`
	ReadOnly      bool   `yaml:"readOnly,omitempty"`
}

type kindPortEntry struct {
	ContainerPort int32  `yaml:"containerPort"`
	HostPort      int32  `yaml:"hostPort"`
	Protocol      string `yaml:"protocol,omitempty"`
	ListenAddress string `yaml:"listenAddress,omitempty"`
}

// renderKindConfig marshals cfg into a kind Cluster config YAML document.
func renderKindConfig(cfg Config) ([]byte, error) {
	node := kindNodeEntry{
		Role:  "control-plane",
		Image: cfg.NodeImage,
	}
	for _, m := range cfg.ExtraMounts {
		node.ExtraMounts = append(node.ExtraMounts, kindMountEntry(m))
	}
	for _, p := range cfg.ExtraPortMappings {
		node.ExtraPortMappings = append(node.ExtraPortMappings, kindPortEntry(p))
	}

	doc := kindClusterConfig{
		Kind:       "Cluster",
		APIVersion: "kind.x-k8s.io/v1alpha4",
		Nodes:      []kindNodeEntry{node},
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("marshaling kind cluster config: %w", err)
	}
	return out, nil
}

// writeKindConfig renders cfg's kind cluster config and writes it to a
// temporary file. The returned cleanup func removes it; callers should defer
// it immediately.
func writeKindConfig(cfg Config) (path string, cleanup func(), err error) {
	body, err := renderKindConfig(cfg)
	if err != nil {
		return "", func() {}, err
	}

	f, err := os.CreateTemp("", "nvkectl-bootstrap-kind-config-*.yaml")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating kind config file: %w", err)
	}
	name := f.Name()
	cleanup = func() { _ = os.Remove(name) }

	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("writing kind config file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("closing kind config file: %w", err)
	}
	return name, cleanup, nil
}
