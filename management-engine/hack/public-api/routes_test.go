// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Route tests keep HTTP bindings aligned with the protobuf service contract.

package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoregistry"
	"sigs.k8s.io/yaml"

	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

// TestHTTPBindingsMatchProtobufContract validates routes and rejects ambiguous bindings.
// Every public RPC must have one supported HTTP representation.
func TestHTTPBindingsMatchProtobufContract(t *testing.T) {
	data, err := os.ReadFile("../../public/tenant/apis/cluster/v1/gateway.yaml")
	require.NoError(t, err)
	files := new(protoregistry.Files)
	require.NoError(t, files.RegisterFile(clusterv1.File_cluster_v1_cluster_proto))
	routes, err := compileRoutes(data, files)
	require.NoError(t, err)
	require.Equal(t, []httpRoute{
		{method: "POST", path: "/v1/clusters", body: "*",
			fullMethod: clusterv1.ClusterService_CreateCluster_FullMethodName},
		{method: "GET", path: "/v1/clusters/{name}",
			fullMethod: clusterv1.ClusterService_GetCluster_FullMethodName},
		{method: "GET", path: "/v1/clusters",
			fullMethod: clusterv1.ClusterService_ListClusters_FullMethodName},
		{method: "PUT", path: "/v1/clusters/{name}", body: "*",
			fullMethod: clusterv1.ClusterService_ReplaceCluster_FullMethodName},
	}, routes)
	for name, test := range map[string]struct {
		diagnostic string
		mutate     func(*httpConfig)
	}{
		"unknown method": {"resolve HTTP selector", func(c *httpConfig) { c.HTTP.Rules[0].Selector += "Typo" }},
		"duplicate method": {"must identify one unbound unary RPC", func(c *httpConfig) {
			c.HTTP.Rules = append(c.HTTP.Rules, c.HTTP.Rules[0])
		}},
		"missing binding": {"has no HTTP binding", func(c *httpConfig) { c.HTTP.Rules = c.HTTP.Rules[1:] }},
		"unknown path field": {"HTTP path field private must identify a request string", func(c *httpConfig) {
			c.HTTP.Rules[1].Get = "/v1/clusters/{private}"
		}},
		"multiple verbs":   {"unsupported HTTP binding", func(c *httpConfig) { c.HTTP.Rules[0].Get = "/v1/clusters" }},
		"unsupported body": {"unsupported HTTP binding", func(c *httpConfig) { c.HTTP.Rules[0].Body = "spec" }},
		"body on get":      {"unsupported HTTP binding", func(c *httpConfig) { c.HTTP.Rules[1].Body = "*" }},
		"duplicate route": {"duplicate HTTP route GET /v1/clusters", func(c *httpConfig) {
			c.HTTP.Rules[1].Get = "/v1/clusters"
		}},
		"unsupported template": {"unsupported HTTP binding", func(c *httpConfig) {
			c.HTTP.Rules[1].Get = "/v1/clusters/{name=*}"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var config httpConfig
			require.NoError(t, yaml.UnmarshalStrict(data, &config))
			test.mutate(&config)
			changed, err := yaml.Marshal(config)
			require.NoError(t, err)
			_, err = compileRoutes(changed, files)
			require.ErrorContains(t, err, test.diagnostic)
		})
	}
}
