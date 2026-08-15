// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

const testNodeID = "abcdef12-3456-7890-abcd-ef1234567890"

// osProfilesDir resolves the OS profiles tree used by profile-rendering tests.
// Production profiles live in nke-site-deploy (NKX-13910). Resolution order:
// CWE_PROFILES_DIR, sibling nke-site-deploy checkout, in-repo fixtures.
func osProfilesDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("CWE_PROFILES_DIR"); dir != "" {
		return path.Join(dir, "osProfiles")
	}
	sibling := "../../../nke-site-deploy/cwe-profiles/profiles/osProfiles"
	if _, err := os.Stat(sibling); err == nil {
		return sibling
	}
	fixture := "../../../test/fixtures/profiles/osProfiles"
	if _, err := os.Stat(fixture); err == nil {
		return fixture
	}
	t.Skip("no OS profiles available; set CWE_PROFILES_DIR to nke-site-deploy/cwe-profiles/*/profiles")
	return ""
}

const trustBundle = `-----BEGIN FAKE TEST-ONLY CERTIFICATE-----
MIICDzCCAbigAwIBAgIUTestCert1234567890ABCDEFGHIJK0wDQYJKoZIhvcNAQEL
BQAwRTELMAkGA1UEBhMCVVMxEzARBgNVBAgMClRlc3QgU3RhdGUxDTALBgNVBAoM
BFRFU1QxEjAQBgNVBAMMCVRFU1QgT05MWTAeFw0yNTAxMDEwMDAwMDBaFw0yNjAx
MDEwMDAwMDBaMEUxCzAJBgNVBAYTAlVTMRMwEQYDVQQIDApUZXN0IFN0YXRlMQ0w
CwYDVQQKDARURVNUMRIwEAYDVQQDDAlURVNUIE9OTFkwXDANBgkqhkiG9w0BAQEF
AANLADBIAkEAm5Hta1234567890testdatafakecert1234567890ABCDEF123456
78901234567890ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890QIDAQABMA0GCSqG
SIb3DQEBCwUAA0EATestSignature1234567890FakeDataNotRealCertificate123
-----END FAKE TEST-ONLY CERTIFICATE-----
-----BEGIN FAKE TEST-ONLY CERTIFICATE-----
MIICDzCCAbigAwIBAgIUTestCert0987654321ZYXWVUTSRQPONMLK0wDQYJKoZIhvcNAQEL
BQAwRTELMAkGA1UEBhMCVVMxEzARBgNVBAgMClRlc3QgU3RhdGUxDTALBgNVBAoM
BFRFU1QxEjAQBgNVBAMMCVRFU1QgT05MWTAeFw0yNTAxMDIwMDAwMDBaFw0yNjAx
MDIwMDAwMDBaMEUxCzAJBgNVBAYTAlVTMRMwEQYDVQQIDApUZXN0IFN0YXRlMQ0w
CwYDVQQKDARURVNUMRIwEAYDVQQDDAlURVNUIE9OTFkwXDANBgkqhkiG9w0BAQEF
AANLADBIAkEAp8Jtb0987654321testdatafakecert0987654321ZYXWVUT987
65432109876543210ZYXWVUTSRQPONMLKJIHGFEDCBA0987654321QIDAQABMA0GCSqG
SIb3DQEBCwUAA0EATestSignature0987654321FakeDataNotRealCertificate987
-----END FAKE TEST-ONLY CERTIFICATE-----`

func TestGetIpxeScript(t *testing.T) {
	strategies := []db.DiskSelectionStrategy{
		db.DiskSelectionStrategySmallest,
		db.DiskSelectionStrategyLargest,
	}

	profilesRoot := osProfilesDir(t)
	profiles, err := os.ReadDir(profilesRoot)
	require.NoError(t, err)

	for _, profile := range profiles {
		if !profile.IsDir() || profile.Name() == "deprecated" {
			continue
		}
		profilePath := path.Join(profilesRoot, profile.Name())

		ipxeBytes, err := os.ReadFile(path.Join(profilePath, "ipxeScript"))
		if os.IsNotExist(err) {
			t.Logf("Profile %s does not have an ipxeScript file; skipping", profile.Name())
			continue
		}
		require.NoError(t, err)

		// Only test profiles whose ipxe script uses the DiskSelectionStrategy template
		if !strings.Contains(string(ipxeBytes), ".DiskSelectionStrategy") {
			continue
		}

		for _, strategy := range strategies {
			t.Run(profile.Name()+"/"+string(strategy), func(t *testing.T) {
				script, err := getIpxeScript(profilePath, string(strategy))
				require.NoError(t, err)
				assert.Contains(t, script, "set disk-strategy "+string(strategy))
				assert.NotContains(t, script, "{{")
			})
		}
	}
}

const (
	testDgxcTenantUser         = "test-admin"
	testDgxcTenantHashedPasswd = "$6$testsalt12345678$testHashTestHashTestHashTestHashTestHashTestHashTestHashTestHashTestHashTestHashABCD56"
)

func TestGetUserData(t *testing.T) {
	activities := &Activities{
		cfg: config.Config{
			SpireServerAddress: "spire-server.example.com",
			SpireServerPort:    "9091",
			NodeAgentPort:      "15005",
			DgxcTenantUser:     testDgxcTenantUser,
			DgxcTenantPassword: "test-password",
		},
		trustDomain: "example.com",
	}

	original := getTrustDomainAndBundle
	t.Cleanup(func() { getTrustDomainAndBundle = original })
	getTrustDomainAndBundle = func(_ context.Context, _ config.Config) (string, string, error) {
		return "example.com", trustBundle, nil
	}

	originalHash := computeHashedPassword
	t.Cleanup(func() { computeHashedPassword = originalHash })
	computeHashedPassword = func(_ string) (string, error) {
		return testDgxcTenantHashedPasswd, nil
	}

	node := db.Node{
		ID:            uuid.MustParse(testNodeID),
		Name:          "spiffe-test-node",
		SpiffeEnabled: true,
	}

	nodeSet := db.NodeSet{
		KubernetesVersion: "1.31.9",
	}

	teleportCfg := &TeleportUserDataConfig{
		Enabled:      true,
		TokenName:    "nke-cwe-" + testNodeID,
		ProxyAddress: "nv-prd-dgxc.teleport.sh:443",
		JoinMethod:   "tpm",
		Labels: "cluster: test-cluster-id\n" +
			"cluster-name: test-cluster\n" +
			"environment: staging\n" +
			"managed-by: nke-cloud-workflow-engine\n" +
			"node-id: " + testNodeID + "\n" +
			"node-pool: test-nodepool-id\n" +
			"resource-group: nke\n" +
			"site: test-site-id\n",
	}

	cluster := db.Cluster{
		CniPlugin: db.CNIOvnKubernetes,
	}

	profilesRoot := osProfilesDir(t)
	profiles, err := os.ReadDir(profilesRoot)
	require.NoError(t, err)
	for _, profile := range profiles {
		if !profile.IsDir() {
			continue
		}
		profilePath := path.Join(profilesRoot, profile.Name())

		// If the profile has a testUserData.yaml file - test it
		testDataFilePath := path.Join(profilePath, "testUserData.yaml")
		testData, err := os.ReadFile(testDataFilePath)
		if os.IsNotExist(err) {
			t.Logf("Profile %s does not have a testData.yaml file\n", profile.Name())
			continue
		}
		require.NoError(t, err)

		t.Logf("Testing profile %s\n", profilePath)
		userData, err := activities.getUserData(
			context.Background(),
			profilePath,
			node,
			nodeSet.DiskSelectionStrategy,
			nodeSet.KubernetesVersion,
			teleportCfg,
			cluster,
		)
		require.NoError(t, err)

		// Strict string comparison of the userData and testData
		assert.Equal(t, string(testData), userData)

		// cloud-init's `#cloud-config` requires valid YAML
		// So require that both testData and the userData must be valid YAML
		// If the yaml is invalid in a Forge cloud-init or ipxe script the autoinstall may drop into the interactive installer.
		var testDataYaml map[string]interface{}
		err = yaml.Unmarshal(testData, &testDataYaml)
		require.NoError(t, err)

		var userDataYaml map[string]interface{}
		err = yaml.Unmarshal([]byte(userData), &userDataYaml)
		require.NoError(t, err)
	}
}

func TestActiveOSProfileTemplatesDoNotUseLegacyAttestationVars(t *testing.T) {
	profilesRoot := osProfilesDir(t)
	profiles, err := os.ReadDir(profilesRoot)
	require.NoError(t, err)

	for _, profile := range profiles {
		if !profile.IsDir() || !strings.HasPrefix(profile.Name(), "nke-") {
			continue
		}

		templatePath := path.Join(profilesRoot, profile.Name(), "userData.tmpl")
		templateData, err := os.ReadFile(templatePath)
		require.NoError(t, err, "reading active OS profile %s", profile.Name())

		// The legacy provisioning path no longer replaces these attestation values.
		assert.NotContains(t, string(templateData), "SERVER=REPLACED-BY-", "active OS profile %s", profile.Name())
		assert.NotContains(t, string(templateData), "SAN_IP=$(hostname -I", "active OS profile %s", profile.Name())
		assert.NotContains(t, string(templateData), "nke-attestation", "active OS profile %s", profile.Name())
	}
}

func TestGetUserDataTemplatedCNIImagePackage(t *testing.T) {
	activities := &Activities{
		cfg: config.Config{
			NodeAgentPort:      "15005",
			DgxcTenantUser:     testDgxcTenantUser,
			DgxcTenantPassword: "test-password",
		},
	}

	originalHash := computeHashedPassword
	t.Cleanup(func() { computeHashedPassword = originalHash })
	computeHashedPassword = func(_ string) (string, error) {
		return testDgxcTenantHashedPasswd, nil
	}

	fixtureProfile := "../../../test/fixtures/profiles/osProfiles/test-fixture-amd64"
	require.DirExists(t, fixtureProfile)

	userData, err := activities.getUserData(
		context.Background(),
		fixtureProfile,
		db.Node{Name: "calico-node"},
		"",
		"1.31.9",
		nil,
		db.Cluster{CniPlugin: db.CNICalico, CniVersion: "3.31.5"},
	)
	require.NoError(t, err)

	assert.Contains(t, userData, "nke-agent=0.1.26")
	assert.Contains(t, userData, "apt-get install -y nke-cni-calico-images-3.31.5")
	assert.NotContains(t, userData, "calico_v3.31.5")

	userData, err = activities.getUserData(
		context.Background(),
		fixtureProfile,
		db.Node{Name: "ovn-node"},
		"",
		"1.31.9",
		nil,
		db.Cluster{CniPlugin: db.CNIOvnKubernetes, CniVersion: "1.4.1"},
	)
	require.NoError(t, err)

	assert.Contains(t, userData, "apt-get install -y nke-cni-ovn-images-1.4.1")
	assert.NotContains(t, userData, "ovn_1.4.1")
}

func TestBuildTeleportLabels(t *testing.T) {
	nodeID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	siteID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	clusterID := uuid.MustParse("66666666-7777-8888-9999-000000000000")
	nodePoolID := uuid.MustParse("abababab-cdcd-efef-0101-232323232323")

	tests := []struct {
		name         string
		controlPlane bool
		wantRole     string
	}{
		{name: "control plane node", controlPlane: true, wantRole: "control-plane"},
		{name: "worker node", controlPlane: false, wantRole: "worker"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := db.Node{ID: nodeID, Name: "my-node-01"}
			cluster := db.Cluster{ID: clusterID, Name: "test-cluster"}
			nicoProvider := db.NicoClusterProvider{SiteID: siteID}
			nodePool := db.NodePool{ID: nodePoolID, ControlPlane: tt.controlPlane}

			got := buildTeleportLabels(node, cluster, nicoProvider, nodePool.ID, nodePool.ControlPlane, "test-site", "staging")

			assert.Contains(t, got, "node-role: "+tt.wantRole+"\n")
			assert.Contains(t, got, "node-name: my-node-01\n")
			assert.Contains(t, got, "node-id: "+nodeID.String()+"\n")
			assert.Contains(t, got, "site: "+siteID.String()+"\n")
			assert.Contains(t, got, "nke-site-controller-name: test-site\n")
			assert.Contains(t, got, "cluster: "+clusterID.String()+"\n")
			assert.Contains(t, got, "cluster-name: test-cluster\n")
			assert.Contains(t, got, "node-pool: "+nodePoolID.String()+"\n")
			assert.Contains(t, got, "environment: staging\n")
			assert.Contains(t, got, "resource-group: nke\n")
			assert.Contains(t, got, "managed-by: nke-cloud-workflow-engine\n")
		})
	}

	t.Run("nil node pool id", func(t *testing.T) {
		node := db.Node{ID: nodeID, Name: "my-node-01"}
		cluster := db.Cluster{ID: clusterID, Name: "test-cluster"}
		nicoProvider := db.NicoClusterProvider{SiteID: siteID}

		got := buildTeleportLabels(node, cluster, nicoProvider, uuid.Nil, false, "test-site", "staging")

		assert.NotContains(t, got, "node-pool:")
		assert.NotContains(t, got, uuid.Nil.String())
	})
}

func TestGetNVLinkActiveDeviceIDs(t *testing.T) {
	t.Run("uses active device IDs from instance type output", func(t *testing.T) {
		deviceIDs := getNVLinkActiveDeviceIDs(GetInstanceTypeOutput{
			NVLinkInterfaceCount:  4,
			NVLinkActiveDeviceIDs: []int32{0, 2, 3},
		})

		assert.Equal(t, []int32{0, 2, 3}, deviceIDs)
	})

	t.Run("expands from count when active device IDs are not provided", func(t *testing.T) {
		deviceIDs := getNVLinkActiveDeviceIDs(GetInstanceTypeOutput{
			NVLinkInterfaceCount: 4,
		})

		assert.Equal(t, []int32{0, 1, 2, 3}, deviceIDs)
	})
}
