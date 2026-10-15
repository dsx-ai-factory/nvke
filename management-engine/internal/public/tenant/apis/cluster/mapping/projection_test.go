// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// These tests exercise generated mappings to protect field exposure, validation, and copy boundaries.
package mapping

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

// createSpec supplies a valid request so each test can isolate its intended mapping boundary.
func createSpec() *clusterv1.CreateClusterSpec {
	return &clusterv1.CreateClusterSpec{
		Release: &clusterv1.CreateRelease{
			Channel: &clusterv1.CreateReleaseChannel{Name: new("stable")},
		},
		ControlPlaneClass: new("standard"),
		ClusterNetwork: &clusterv1.CreateClusterNetwork{
			Pods:     &clusterv1.CreateClusterNetworkPods{CidrBlocks: []string{"10.244.0.0/16"}},
			Services: &clusterv1.CreateClusterNetworkServices{CidrBlocks: []string{"10.96.0.0/12"}},
		},
	}
}

// TestProjectionMasksPrivateDataAndUsesSnakeCase protects public field exposure
// and verifies that mutable request and response data cannot change stored values.
func TestProjectionMasksPrivateDataAndUsesSnakeCase(t *testing.T) {
	input := createSpec()
	stored, err := CreateSpecToInternal(input)
	require.NoError(t, err)
	input.ClusterNetwork.Pods.CidrBlocks[0] = "changed-after-create"
	require.NoError(t, unstructured.SetNestedField(stored, "private-value", "release", "channel", "privateField"))
	stored["authentication"] = map[string]any{"private": "not public"}
	require.NoError(t, unstructured.SetNestedField(stored, "cluster.local", "clusterNetwork", "serviceDomain"))
	public, err := ProjectSpec(stored)
	require.NoError(t, err)
	encoded, err := protojson.Marshal(public)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"release":{"channel":{"name":"stable"}},
		"control_plane_class":"standard",
		"cluster_network":{"pods":{"cidr_blocks":["10.244.0.0/16"]},"services":{"cidr_blocks":["10.96.0.0/12"]}}
	}`, string(encoded))
	public.ClusterNetwork.Pods.CidrBlocks[0] = "changed"
	storedCIDRs, _, err := unstructured.NestedStringSlice(stored, "clusterNetwork", "pods", "cidrBlocks")
	require.NoError(t, err)
	require.Equal(t, []string{"10.244.0.0/16"}, storedCIDRs)
}

// TestReplacementPreservesPrivateAndCreateOnlyFields limits replacement to reviewed
// leaves and checks that later request mutations cannot change validated values.
func TestReplacementPreservesPrivateAndCreateOnlyFields(t *testing.T) {
	stored, err := CreateSpecToInternal(createSpec())
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(stored, "old-version", "release", "channel", "version"))
	require.NoError(t, unstructured.SetNestedField(stored, "private", "release", "channel", "futureField"))
	beforeNetwork := runtime.DeepCopyJSONValue(stored["clusterNetwork"])
	replacement := &clusterv1.ReplaceClusterSpec{
		Release: &clusterv1.ReplaceRelease{
			Channel: &clusterv1.ReplaceReleaseChannel{Name: new("preview")},
		},
		ControlPlaneClass: new("large"),
	}
	prepared, err := PrepareReplacement(replacement)
	require.NoError(t, err)
	*replacement.ControlPlaneClass = "changed-after-validation"
	require.NoError(t, prepared.Apply(stored))
	channel, _, err := unstructured.NestedMap(stored, "release", "channel")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"name": "preview", "futureField": "private"}, channel)
	require.Equal(t, beforeNetwork, stored["clusterNetwork"])
	require.Equal(t, "large", stored["controlPlaneClass"])
}

// TestDerivedConstraintsAndPresence checks generated admission against schema
// bounds and presence rules without returning invalid values in diagnostics.
func TestDerivedConstraintsAndPresence(t *testing.T) {
	t.Run("maximum unicode version", func(t *testing.T) {
		input := createSpec()
		input.Release.Channel.Version = new(strings.Repeat("é", 128))
		_, err := CreateSpecToInternal(input)
		require.NoError(t, err)
	})

	for _, tc := range []struct {
		name, field, description string
		mutate                   func(*clusterv1.CreateClusterSpec)
		replacement              *clusterv1.ReplaceClusterSpec
	}{
		{name: "missing spec", field: "spec", description: "is required"},
		{name: "required release", field: "spec.release", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release = nil }},
		{name: "required name", field: "spec.release.channel.name", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release.Channel.Name = nil }},
		{name: "empty name", field: "spec.release.channel.name", description: "has invalid length", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release.Channel.Name = new("") }},
		{name: "invalid name", field: "spec.release.channel.name", description: "has invalid format", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release.Channel.Name = new("UPPER") }},
		{name: "empty optional version", field: "spec.release.channel.version", description: "has invalid length", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release.Channel.Version = new("") }},
		{name: "long optional version", field: "spec.release.channel.version", description: "has invalid length", mutate: func(s *clusterv1.CreateClusterSpec) {
			s.Release.Channel.Version = new(strings.Repeat("é", 129))
		}},
		{name: "required class", field: "spec.control_plane_class", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.ControlPlaneClass = nil }},
		{name: "invalid class", field: "spec.control_plane_class", description: "has invalid format", mutate: func(s *clusterv1.CreateClusterSpec) {
			s.ControlPlaneClass = new("PRIVATE-INVALID-VALUE")
		}},
		{name: "missing networks", field: "spec.cluster_network", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.ClusterNetwork = nil }},
		{name: "missing pod networks", field: "spec.cluster_network.pods", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.ClusterNetwork.Pods = nil }},
		{name: "empty cidr list", field: "spec.cluster_network.pods.cidr_blocks", description: "has invalid item count", mutate: func(s *clusterv1.CreateClusterSpec) { s.ClusterNetwork.Pods.CidrBlocks = nil }},
		{name: "dual stack create", field: "spec.cluster_network.pods.cidr_blocks", description: "has invalid item count", mutate: func(s *clusterv1.CreateClusterSpec) {
			s.ClusterNetwork.Pods.CidrBlocks = []string{"10.244.0.0/16", "2001:db8::/64"}
		}},
		{name: "long cidr item", field: "spec.cluster_network.pods.cidr_blocks[0]", description: "has invalid length", mutate: func(s *clusterv1.CreateClusterSpec) {
			s.ClusterNetwork.Pods.CidrBlocks[0] = strings.Repeat("PRIVATE-INVALID-VALUE", 3)
		}},
		{name: "replacement requires class", field: "spec.control_plane_class", description: "is required", replacement: &clusterv1.ReplaceClusterSpec{
			Release: &clusterv1.ReplaceRelease{Channel: &clusterv1.ReplaceReleaseChannel{Name: new("preview")}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.replacement != nil {
				_, err = PrepareReplacement(tc.replacement)
			} else {
				var input *clusterv1.CreateClusterSpec
				if tc.mutate != nil {
					input = createSpec()
					tc.mutate(input)
				}
				_, err = CreateSpecToInternal(input)
			}
			var fieldError *FieldError
			require.ErrorAs(t, err, &fieldError)
			require.Equal(t, tc.field, fieldError.Field)
			require.Equal(t, tc.description, fieldError.Description)
			require.NotContains(t, err.Error(), "PRIVATE-INVALID-VALUE")
		})
	}
}

// TestProjectionRetainsStoredDualStack ensures stricter create limits do not erase valid stored networks.
func TestProjectionRetainsStoredDualStack(t *testing.T) {
	stored, err := CreateSpecToInternal(createSpec())
	require.NoError(t, err)
	cidrs := []string{"10.244.0.0/16", "2001:db8::/64"}
	require.NoError(t, unstructured.SetNestedStringSlice(stored, cidrs, "clusterNetwork", "pods", "cidrBlocks"))
	projected, err := ProjectSpec(stored)
	require.NoError(t, err)
	require.Equal(t, cidrs, projected.ClusterNetwork.Pods.CidrBlocks)
}

// TestMappingsRejectMalformedStorageWithoutEchoingValues exercises malformed read
// and replacement ancestry so accessor diagnostics cannot reveal stored values.
func TestMappingsRejectMalformedStorageWithoutEchoingValues(t *testing.T) {
	prepared, err := PrepareReplacement(&clusterv1.ReplaceClusterSpec{
		Release: &clusterv1.ReplaceRelease{
			Channel: &clusterv1.ReplaceReleaseChannel{Name: new("preview")},
		},
		ControlPlaneClass: new("large"),
	})
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		stored  map[string]any
		replace bool
	}{
		{name: "read object", stored: map[string]any{"release": "private-secret"}},
		{name: "read string", stored: map[string]any{
			"controlPlaneClass": map[string]any{"secret": "private-secret"},
		}},
		{name: "read list item", stored: map[string]any{
			"clusterNetwork": map[string]any{"pods": map[string]any{"cidrBlocks": []any{"private-secret", int64(4)}}},
		}},
		{name: "replace parent", stored: map[string]any{"release": "private-secret"}, replace: true},
		{name: "replace nested parent", stored: map[string]any{
			"release": map[string]any{"channel": "private-secret"},
		}, replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.replace {
				err = prepared.Apply(tc.stored)
			} else {
				_, err = ProjectSpec(tc.stored)
			}
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private-secret")
		})
	}
}
