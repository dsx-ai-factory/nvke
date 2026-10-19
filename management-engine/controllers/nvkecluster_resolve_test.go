// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Resolution tests cover supported intent, binding selection, and template isolation.

package controllers

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

func requestCluster() *internalv1.NVKECluster {
	return &internalv1.NVKECluster{
		Name: "example", Namespace: "project-a", UID: "2587afe6-1ea2-4fea-8872-7c32a18f8587",
		Spec: internalv1.NVKEClusterSpec{
			Release:           internalv1.ReleaseSelection{Channel: internalv1.ReleaseChannelReference{Name: "stable"}},
			ControlPlaneClass: "standard",
			ClusterNetwork: internalv1.ClusterNetwork{
				Pods:          internalv1.NetworkRanges{CIDRBlocks: []internalv1.NetworkCIDR{"10.244.0.0/16"}},
				Services:      internalv1.NetworkRanges{CIDRBlocks: []internalv1.NetworkCIDR{"10.96.0.0/16"}},
				ServiceDomain: "cluster.local",
			},
		},
	}
}

func requestBinding() ControllerBinding {
	return ControllerBinding{
		Namespace: "project-a", Channel: "stable", ControlPlaneClass: "standard",
		Template: api.CreateClusterRequest{
			ControlPlaneKubernetesVersion: "1.35.1",
			Infra:                         &api.InfraClusterProvider{Cluster: map[string]any{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2", "kind": "DockerCluster", "spec": map[string]any{"test": "original"}}},
			ControlPlaneConfig: &api.CreateClusterControlPlaneConfig{
				DesiredCount: 3,
				Infra:        &api.InfraControlPlaneProvider{MachineTemplate: map[string]any{"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2", "kind": "DockerMachineTemplate"}},
			},
		},
	}
}

func TestResolveControllerRequest(t *testing.T) {
	cluster, binding := requestCluster(), requestBinding()
	binding.Template.ControlPlaneConfig.Infra.BootstrapFiles = []api.CloudInitFile{{Path: "/etc/example", Content: "original"}}
	cluster.Spec.AcceleratedRuntime = &internalv1.AcceleratedRuntimeSelection{Name: "none"}
	cluster.Spec.ClusterNetwork.Pods.CIDRBlocks[0] = "10.244.1.0/16"
	before, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	request, err := ResolveControllerRequest(cluster, []ControllerBinding{binding})
	if err != nil {
		t.Fatal(err)
	}
	if request.Name != "nke-"+string(cluster.UID) || !request.EtcdEncrypted || request.CniPlugin != api.CreateClusterRequestCniPluginOvnKubernetes {
		t.Fatalf("unexpected identity/defaults: %+v", request)
	}
	if request.Network.PodCIDR != "10.244.0.0/16" || request.Network.ServiceCIDR != "10.96.0.0/16" || request.ControlPlaneConfig.DesiredCount != 3 {
		t.Fatalf("incorrect network or class mapping: %+v", request)
	}
	request.Infra.Cluster["spec"].(map[string]any)["test"] = "changed"
	request.ControlPlaneConfig.DesiredCount = 1
	request.ControlPlaneConfig.Infra.MachineTemplate["kind"] = "ChangedTemplate"
	request.ControlPlaneConfig.Infra.BootstrapFiles[0].Content = "changed"
	after, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("resolution mutated the binding")
	}
}

func TestResolveControllerSelection(t *testing.T) {
	cases := []struct {
		name      string
		modify    func(*internalv1.NVKECluster, *[]ControllerBinding)
		wantError bool
	}{
		{"explicit default", func(_ *internalv1.NVKECluster, _ *[]ControllerBinding) {}, false},
		{"explicit version", func(c *internalv1.NVKECluster, b *[]ControllerBinding) {
			c.Spec.Release.Channel.Version = "release-1"
			(*b)[0].Version = "release-1"
		}, false},
		{"no implicit latest", func(_ *internalv1.NVKECluster, b *[]ControllerBinding) { (*b)[0].Version = "release-1" }, true},
		{"namespace isolation", func(c *internalv1.NVKECluster, _ *[]ControllerBinding) { c.Namespace = "other-project" }, true},
		{"different class", func(c *internalv1.NVKECluster, _ *[]ControllerBinding) { c.Spec.ControlPlaneClass = "large" }, true},
		{"unpersisted cluster", func(c *internalv1.NVKECluster, _ *[]ControllerBinding) { c.UID = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster, bindings := requestCluster(), []ControllerBinding{requestBinding()}
			tc.modify(cluster, &bindings)
			_, err := ResolveControllerRequest(cluster, bindings)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError = %v", err, tc.wantError)
			}
		})
	}
}

func TestResolveControllerUnsupportedIntent(t *testing.T) {
	cases := map[string]func(*internalv1.NVKECluster){
		"workers":        func(c *internalv1.NVKECluster) { c.Spec.NodePools = []internalv1.NodePool{{}} },
		"worker network": func(c *internalv1.NVKECluster) { c.Spec.Network = &internalv1.Network{VPCID: uuid.NewString()} },
		"authentication": func(c *internalv1.NVKECluster) { c.Spec.Authentication = &internalv1.ClusterAuthentication{} },
		"public API":     func(c *internalv1.NVKECluster) { c.Spec.APIServerAccess.PublicAccessEnabled = true },
		"public CIDRs": func(c *internalv1.NVKECluster) {
			c.Spec.APIServerAccess.PublicAccessCIDRs = []internalv1.PublicAccessCIDR{{CIDR: "10.0.0.0/8"}}
		},
		"accelerated runtime": func(c *internalv1.NVKECluster) {
			c.Spec.AcceleratedRuntime = &internalv1.AcceleratedRuntimeSelection{Name: "gpu", Version: "1"}
		},
		"custom domain": func(c *internalv1.NVKECluster) { c.Spec.ClusterNetwork.ServiceDomain = "custom.local" },
		"IPv6": func(c *internalv1.NVKECluster) {
			c.Spec.ClusterNetwork.Pods.CIDRBlocks = []internalv1.NetworkCIDR{"fd00::/48"}
		},
		"dual stack": func(c *internalv1.NVKECluster) {
			c.Spec.ClusterNetwork.Pods.CIDRBlocks = append(c.Spec.ClusterNetwork.Pods.CIDRBlocks, "fd00::/48")
		},
		"invalid prefix": func(c *internalv1.NVKECluster) { c.Spec.ClusterNetwork.Services.CIDRBlocks[0] = "invalid" },
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			cluster := requestCluster()
			modify(cluster)
			_, err := ResolveControllerRequest(cluster, []ControllerBinding{requestBinding()})
			if _, ok := errors.AsType[*InvalidSpecError](err); !ok {
				t.Fatalf("expected InvalidSpecError, got %v", err)
			}
		})
	}
}

func TestLoadControllerBindings(t *testing.T) {
	valid, err := json.Marshal([]ControllerBinding{requestBinding()})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"valid": string(valid), "empty": "[]", "null": "null",
		"unknown":   strings.Replace(string(valid), `"namespace":`, `"unknown":`, 1),
		"trailing":  string(valid) + "{}",
		"duplicate": "[" + string(valid[1:len(valid)-1]) + "," + string(valid[1:len(valid)-1]) + "]",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bindings.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			bindings, err := LoadControllerBindings(path)
			if name == "valid" {
				if err != nil || len(bindings) != 1 {
					t.Fatalf("bindings=%v error=%v", bindings, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected configuration rejection")
			}
		})
	}
}

func TestLoadControllerInvalidTemplates(t *testing.T) {
	cases := map[string]func(*api.CreateClusterRequest){
		"name":      func(r *api.CreateClusterRequest) { r.Name = "tenant-chosen" },
		"network":   func(r *api.CreateClusterRequest) { r.Network = &api.ClusterNetworkCreateConfig{} },
		"nodePools": func(r *api.CreateClusterRequest) { r.NodePools = &[]api.NodePoolCreateConfig{} },
		"CNI":       func(r *api.CreateClusterRequest) { r.CniPlugin = api.CreateClusterRequestCniPluginCalico },
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			binding := requestBinding()
			modify(&binding.Template)
			data, err := json.Marshal([]ControllerBinding{binding})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "bindings.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadControllerBindings(path); err == nil {
				t.Fatal("expected invalid template rejection")
			}
		})
	}
}
