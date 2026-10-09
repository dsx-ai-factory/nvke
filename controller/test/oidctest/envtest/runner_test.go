// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/controller/test/oidctest/probe"
)

func TestStartRequiresAssetsAndAuthenticationConfig(t *testing.T) {
	t.Setenv(binaryAssetsEnv, "")
	_, err := Start(t, Config{})
	require.ErrorContains(t, err, "authentication configuration is required")
	_, err = Start(t, Config{AuthenticationConfig: []byte("config")})
	require.ErrorContains(t, err, "KUBEBUILDER_ASSETS is required")
}

func TestProbeRequestCopiesOnlyEndpointAndCA(t *testing.T) {
	runner := &Runner{adminConfig: &rest.Config{Host: "https://127.0.0.1:6443", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("CA")}}}
	request := runner.ProbeRequest(
		"token",
		probe.ResourceRequest{Resource: "configmaps", Namespace: "test"},
		probe.ResourceRequest{Resource: "secrets", Namespace: "test"},
	)
	require.Equal(t, runner.adminConfig.Host, request.Endpoint)
	require.Equal(t, []byte("CA"), request.CAPEM)
	require.Equal(t, "token", request.Token)
	runner.adminConfig.CAData[0] = 'X'
	require.Equal(t, []byte("CA"), request.CAPEM)
}
