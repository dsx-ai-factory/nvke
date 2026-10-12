// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestInstallCNIRequest(t *testing.T) {
	tests := []struct {
		name        string
		cluster     db.Cluster
		registry    string
		wantVariant nvkeproto.CniVariant
		wantVersion string
		wantConfig  map[string]string
		wantErr     string
	}{
		{
			// What cluster creation actually stores: server/clusters.go writes the
			// plugin default into CniVersion, so this is the normal API path, not
			// the empty-string one below.
			name:        "a cluster created with the default version is unpinned",
			cluster:     db.Cluster{CniPlugin: db.CNICalico, CniVersion: db.DefaultCalicoCNIVersion, PodCIDR: "10.100.0.0/16", ServiceCIDR: "10.200.0.0/16"},
			wantVariant: nvkeproto.CniVariant_CNI_VARIANT_CALICO,
			wantVersion: "",
		},
		{
			name:        "a row with no version is unpinned",
			cluster:     db.Cluster{CniPlugin: db.CNICalico},
			wantVariant: nvkeproto.CniVariant_CNI_VARIANT_CALICO,
			wantVersion: "",
		},
		{
			// OVN was excluded while the agent installed it with a plain
			// `helm install`, which fails on the retry this activity depends on.
			// #396 moved both charts to `upgrade --install`.
			name:        "ovn selects the ovn variant",
			cluster:     db.Cluster{CniPlugin: db.CNIOvnKubernetes},
			wantVariant: nvkeproto.CniVariant_CNI_VARIANT_OVN,
			wantVersion: "",
		},
		{
			name:        "an ovn version other than the default selects that stream",
			cluster:     db.Cluster{CniPlugin: db.CNIOvnKubernetes, CniVersion: " 1.4.5 "},
			wantVariant: nvkeproto.CniVariant_CNI_VARIANT_OVN,
			wantVersion: "1.4.5",
		},
		{
			name:        "an ovn cluster on the default version is unpinned",
			cluster:     db.Cluster{CniPlugin: db.CNIOvnKubernetes, CniVersion: db.DefaultOvnKubernetesCNIVersion},
			wantVariant: nvkeproto.CniVariant_CNI_VARIANT_OVN,
			wantVersion: "",
		},
		{
			// The in-init request carried this on the deprecated CniConfig field,
			// which the agent ignores once the in-init install is skipped.
			name:        "the CNI image registry override is carried over",
			cluster:     db.Cluster{CniPlugin: db.CNICalico},
			registry:    "registry.example.com/nvke",
			wantVariant: nvkeproto.CniVariant_CNI_VARIANT_CALICO,
			wantConfig:  map[string]string{cniRegistryHelmValue: "registry.example.com/nvke"},
		},
		{
			name:        "a version other than the default selects that stream",
			cluster:     db.Cluster{CniPlugin: db.CNICalico, CniVersion: " 3.29.0 "},
			wantVariant: nvkeproto.CniVariant_CNI_VARIANT_CALICO,
			wantVersion: "3.29.0",
		},
		{
			name:    "an unknown plugin is rejected",
			cluster: db.Cluster{CniPlugin: "flannel"},
			wantErr: "CNI plugin flannel has no post-init install path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := installCNIRequest(tt.cluster, tt.registry)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantVariant, req.GetCniVariant())
			// A non-empty version becomes an apt "=" pin in the agent, so an
			// unset cluster version must stay empty rather than take a default.
			require.Equal(t, tt.wantVersion, req.GetVersion())
			require.Equal(t, tt.cluster.PodCIDR, req.GetPodNetworkCidr())
			require.Equal(t, tt.cluster.ServiceCIDR, req.GetServiceCidr())
			require.Equal(t, tt.wantConfig, req.GetCniConfig())
		})
	}
}

// The mode is decided once, before kubeadm runs. A plugin with no post-init
// path must come back false here rather than fail after the control plane is
// already up with no networking.
func TestPostInitCNISupported(t *testing.T) {
	require.True(t, postInitCNISupported(db.CNICalico))
	require.True(t, postInitCNISupported(db.CNIOvnKubernetes))
	require.False(t, postInitCNISupported("flannel"))
}

// The activity retries indefinitely, so anything the agent cannot be talked out
// of has to come back non-retryable.
func TestInstallCNIError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantType   string
		wantSubstr string
	}{
		{
			name:       "an agent without the RPC names the version needed",
			err:        status.Error(codes.Unimplemented, "unknown method InstallCNI"),
			wantType:   "AgentTooOld",
			wantSubstr: "nke-agent 0.2.0 or later",
		},
		{
			name:     "a missing charts package is permanent",
			err:      status.Error(codes.FailedPrecondition, "nke-cni-calico-manifests-3.30.1 is not installed"),
			wantType: "InstallCNIPreconditionFailed",
		},
		{
			name:     "a bad request is permanent",
			err:      status.Error(codes.InvalidArgument, "cni_variant must be OVN or CALICO"),
			wantType: "InstallCNIPreconditionFailed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The client wraps the status error, so classification has to survive %w.
			err := installCNIError("node-1", fmt.Errorf("failed to install CNI on node node-1: %w", tt.err))
			var applicationErr *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationErr)
			require.True(t, applicationErr.NonRetryable(), "must not be retried forever")
			require.Equal(t, tt.wantType, applicationErr.Type())
			if tt.wantSubstr != "" {
				require.Contains(t, err.Error(), tt.wantSubstr)
			}
		})
	}
}

// A node that is still coming up must keep being retried.
func TestInstallCNIErrorRetriesTransient(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Internal} {
		err := installCNIError("node-1", status.Error(code, "boom"))
		var applicationErr *temporal.ApplicationError
		require.False(t, errors.As(err, &applicationErr) && applicationErr.NonRetryable(), "%s must stay retryable", code)
	}
}
