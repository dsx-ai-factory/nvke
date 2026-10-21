// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestKeyRendersBothForms(t *testing.T) {
	k := Key("node-id")
	assert.Equal(t, "nvke.nvidia.com/node-id", k.String())
	assert.Equal(t, "nke.nvidia.com/node-id", k.Legacy())
	assert.Equal(t, []string{"nvke.nvidia.com/node-id", "nke.nvidia.com/node-id"}, k.All())
}

func TestGetPrefersTheCurrentDomain(t *testing.T) {
	k := Key("reboot")

	tests := []struct {
		name    string
		labels  map[string]string
		want    string
		wantHas bool
	}{
		{name: "absent", labels: map[string]string{}},
		{name: "current only", labels: map[string]string{k.String(): "operator"}, want: "operator", wantHas: true},
		{name: "legacy only", labels: map[string]string{k.Legacy(): "operator"}, want: "operator", wantHas: true},
		{
			name:    "both, current wins",
			labels:  map[string]string{k.String(): "current", k.Legacy(): "legacy"},
			want:    "current",
			wantHas: true,
		},
		{
			name:    "current set to empty still counts as present",
			labels:  map[string]string{k.String(): "", k.Legacy(): "legacy"},
			want:    "",
			wantHas: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := k.Get(test.labels)
			assert.Equal(t, test.want, got)
			assert.Equal(t, test.wantHas, ok)
			assert.Equal(t, test.want, k.Value(test.labels))
			assert.Equal(t, test.wantHas, k.Has(test.labels))
		})
	}
}

func TestMatchesAcceptsEitherDomain(t *testing.T) {
	k := Key("NeedsRecycle")
	assert.True(t, k.Matches("nvke.nvidia.com/NeedsRecycle"))
	assert.True(t, k.Matches("nke.nvidia.com/NeedsRecycle"))
	assert.False(t, k.Matches("nvke.nvidia.com/NeedsRepair"))
	assert.False(t, k.Matches("other.nvidia.com/NeedsRecycle"))
	assert.False(t, k.Matches(""))
}

func TestDeleteRemovesBothForms(t *testing.T) {
	k := Key("reboot-started")
	labels := map[string]string{k.String(): "a", k.Legacy(): "b", "unrelated": "c"}
	k.Delete(labels)
	assert.Equal(t, map[string]string{"unrelated": "c"}, labels)
}

func TestDeleteLegacyKeepsTheCurrentForm(t *testing.T) {
	k := Key("cluster-id")
	labels := map[string]string{k.String(): "a", k.Legacy(): "b"}
	k.DeleteLegacy(labels)
	assert.Equal(t, map[string]string{k.String(): "a"}, labels)
}

func TestSelectorsRenderKubectlArguments(t *testing.T) {
	k := Key("reboot-status")
	assert.Equal(t, "nvke.nvidia.com/reboot-status=InProgress", k.Selector("InProgress"))
	assert.Equal(t, []string{"nvke.nvidia.com/reboot-status-", "nke.nvidia.com/reboot-status-"}, k.RemoveSelectors())
}

func TestIsOurs(t *testing.T) {
	assert.True(t, IsOurs("nvke.nvidia.com/reboot"))
	assert.True(t, IsOurs("nke.nvidia.com/reboot"))
	assert.False(t, IsOurs("nvke.nvidia.com"))
	assert.False(t, IsOurs("node.kubernetes.io/instance-type"))
	// A subdomain is a different domain: owner.nke.nvidia.com keys are not ours.
	assert.False(t, IsOurs("owner.nke.nvidia.com/cluster"))
}

func TestBothForms(t *testing.T) {
	assert.Equal(t,
		[]string{"nvke.nvidia.com/provisioning", "nke.nvidia.com/provisioning"},
		BothForms("nvke.nvidia.com/provisioning"))
	assert.Equal(t,
		[]string{"nvke.nvidia.com/provisioning", "nke.nvidia.com/provisioning"},
		BothForms("nke.nvidia.com/provisioning"))
	// Anything else is returned untouched, so a caller can pass an arbitrary
	// key through without special-casing.
	assert.Equal(t, []string{"example.com/thing"}, BothForms("example.com/thing"))
}

// Every key has to be a valid Kubernetes qualified name in both domains, or the
// API server rejects the object at write time rather than here.
func TestEveryKeyIsAValidQualifiedName(t *testing.T) {
	all := []Key{
		NodeID, NodePoolID, ClusterID, IPAddress, SiteID,
		MachineID, NodeSiteID, NodeSiteName, NodeVPCID, NodeVPCName,
		Reboot, RebootStarted, RebootAttempt, LastCompletedReboot,
		RebootStatus, RebootStatusTimestamp, Terminate, TerminateStatus,
		ManagedBy, KubeconfigMode, NodeMachineHealthIssue,
		MachineHostID, MachineRebootCompleteTimestamp, MachineReboot,
		MachineHealthIssue, MachineSpireAgentSpiffeIDPath,
		MaintenanceTaint, ProvisioningTaint, NeedsRecycle, NeedsRepair,
	}
	require.NotEmpty(t, all)
	for _, k := range all {
		for _, name := range k.All() {
			assert.Empty(t, validation.IsQualifiedName(name), "key %q", name)
		}
	}
}

// The two domains must stay distinct, and both must be ones the node-metadata
// validator already reserves (controllerconfig.NodeMetadataReservedInternalDomains).
func TestDomainsAreDistinct(t *testing.T) {
	assert.NotEqual(t, Domain, LegacyDomain)
	assert.Equal(t, "nvke.nvidia.com", Domain)
	assert.Equal(t, "nke.nvidia.com", LegacyDomain)
}
