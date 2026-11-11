// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clusterConfigurationFixture builds a persisted ClusterConfiguration in either
// kubeadm API shape, which is the axis these merges most often get wrong:
// v1beta3 extraArgs are a map and v1beta4's are a list, and emitting the wrong
// one breaks the next kubeadm re-render.
func clusterConfigurationFixture(apiVersion, kubernetesVersion, etcd string) []byte {
	return []byte("apiVersion: " + apiVersion + "\n" +
		"kind: ClusterConfiguration\n" +
		"kubernetesVersion: " + kubernetesVersion + "\n" + etcd)
}

const (
	v1beta3LocalEtcd = `etcd:
  local:
    dataDir: /var/lib/etcd
    extraArgs:
      quota-backend-bytes: "17179869184"
`
	v1beta4LocalEtcd = `etcd:
  local:
    dataDir: /var/lib/etcd
    extraArgs:
    - name: quota-backend-bytes
      value: "17179869184"
`
)

func TestMergeBootstrapDefragIntoClusterConfiguration(t *testing.T) {
	t.Run("v1beta3 at the floor gains both settings in map shape", func(t *testing.T) {
		raw := clusterConfigurationFixture("kubeadm.k8s.io/v1beta3", "v1.34.2", v1beta3LocalEtcd)

		merged, changed, err := MergeBootstrapDefragIntoClusterConfiguration(raw)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Contains(t, string(merged), "bootstrap-defrag-threshold-megabytes: \"200\"")
		assert.Contains(t, string(merged), "etcd-compaction-interval: 5m")
		assert.NotContains(t, string(merged), "- name: quota-backend-bytes", "v1beta3 must stay map-shaped")
	})

	t.Run("v1beta4 at the floor gains both settings in list shape", func(t *testing.T) {
		raw := clusterConfigurationFixture("kubeadm.k8s.io/v1beta4", "v1.34.2", v1beta4LocalEtcd)

		merged, changed, err := MergeBootstrapDefragIntoClusterConfiguration(raw)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Contains(t, string(merged), "- name: bootstrap-defrag-threshold-megabytes\n      value: \"200\"")
		assert.Contains(t, string(merged), "- name: etcd-compaction-interval\n    value: 5m")
	})

	// The sharp one. Writing the threshold here would hand etcd 3.5 a flag it
	// exits on, so the next control-plane node to join would crashloop its
	// member rather than merely skipping the defragmentation.
	t.Run("below the floor only the apiserver interval is pinned", func(t *testing.T) {
		raw := clusterConfigurationFixture("kubeadm.k8s.io/v1beta3", "v1.33.6", v1beta3LocalEtcd)

		merged, changed, err := MergeBootstrapDefragIntoClusterConfiguration(raw)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.NotContains(t, string(merged), "bootstrap-defrag-threshold-megabytes")
		assert.Contains(t, string(merged), "etcd-compaction-interval: 5m")
	})

	// An operator may pin an etcd that the Kubernetes version does not imply,
	// to work around a defect. Inferring from kubernetesVersion here would hand
	// etcd 3.5 a flag it exits on, so the pinned tag has to win.
	t.Run("a pinned etcd 3.5 suppresses the threshold above the k8s floor", func(t *testing.T) {
		raw := clusterConfigurationFixture("kubeadm.k8s.io/v1beta3", "v1.34.2", `etcd:
  local:
    dataDir: /var/lib/etcd
    imageTag: 3.5.21-0
    extraArgs:
      quota-backend-bytes: "17179869184"
`)

		merged, changed, err := MergeBootstrapDefragIntoClusterConfiguration(raw)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.NotContains(t, string(merged), "bootstrap-defrag-threshold-megabytes")
		assert.Contains(t, string(merged), "etcd-compaction-interval: 5m")
		assert.Contains(t, string(merged), "imageTag: 3.5.21-0", "the pin must survive the merge")
	})

	// The opposite pin, which the k8s version alone would wrongly refuse.
	t.Run("a pinned etcd 3.6 earns the threshold below the k8s floor", func(t *testing.T) {
		raw := clusterConfigurationFixture("kubeadm.k8s.io/v1beta4", "v1.33.6", `etcd:
  local:
    dataDir: /var/lib/etcd
    imageTag: 3.6.5-0
    extraArgs:
    - name: quota-backend-bytes
      value: "17179869184"
`)

		merged, changed, err := MergeBootstrapDefragIntoClusterConfiguration(raw)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Contains(t, string(merged), "- name: bootstrap-defrag-threshold-megabytes\n      value: \"200\"")
	})

	t.Run("external etcd has no local member to configure", func(t *testing.T) {
		raw := clusterConfigurationFixture("kubeadm.k8s.io/v1beta3", "v1.34.2",
			"etcd:\n  external:\n    endpoints:\n    - https://10.0.0.1:2379\n")

		merged, changed, err := MergeBootstrapDefragIntoClusterConfiguration(raw)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.NotContains(t, string(merged), "bootstrap-defrag-threshold-megabytes")
		assert.Contains(t, string(merged), "etcd-compaction-interval: 5m")
	})

	// A threshold already present with a different value is reconciled to ours
	// rather than left alone. Unlike the etcd image tag, this flag is the
	// controller's own setting, so converging it is the reconciler's job.
	t.Run("an existing threshold is reconciled to the controller's value", func(t *testing.T) {
		raw := clusterConfigurationFixture("kubeadm.k8s.io/v1beta3", "v1.34.2", `etcd:
  local:
    dataDir: /var/lib/etcd
    extraArgs:
      bootstrap-defrag-threshold-megabytes: "2048"
      quota-backend-bytes: "17179869184"
`)

		merged, changed, err := MergeBootstrapDefragIntoClusterConfiguration(raw)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Contains(t, string(merged), "bootstrap-defrag-threshold-megabytes: \"200\"")
		assert.NotContains(t, string(merged), "\"2048\"")
	})

	// Idempotence is what lets the caller skip the ConfigMap write, so a
	// re-run has to report changed=false and emit identical bytes.
	t.Run("re-running is a byte-stable no-op", func(t *testing.T) {
		raw := clusterConfigurationFixture("kubeadm.k8s.io/v1beta4", "v1.34.2", v1beta4LocalEtcd)

		once, changed, err := MergeBootstrapDefragIntoClusterConfiguration(raw)
		require.NoError(t, err)
		require.True(t, changed)

		twice, changed, err := MergeBootstrapDefragIntoClusterConfiguration(once)
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Equal(t, string(once), string(twice))
	})
}

func TestSupportsBootstrapDefrag(t *testing.T) {
	tests := []struct {
		name              string
		kubernetesVersion string
		etcdImageTag      string
		want              bool
	}{
		// No pinned image: the Kubernetes version stands in for the etcd that
		// kubeadm would default to.
		{name: "well below the floor", kubernetesVersion: "v1.31.0", want: false},
		{name: "last release defaulting to etcd 3.5", kubernetesVersion: "v1.33.6", want: false},
		{name: "first release defaulting to etcd 3.6", kubernetesVersion: "v1.34.0", want: true},
		{name: "patch release above the floor", kubernetesVersion: "v1.34.2", want: true},
		{name: "later minor", kubernetesVersion: "v1.36.0", want: true},
		{name: "version without the v prefix", kubernetesVersion: "1.34.0", want: true},
		// etcd 3.5 exits 2 on an unknown flag rather than ignoring it, so an
		// unusable version has to leave the flag off. The cost of being wrong
		// in this direction is a member that defragments on recycle instead of
		// on restart; the other direction crashloops it.
		{name: "empty version", kubernetesVersion: "", want: false},
		{name: "unparseable version", kubernetesVersion: "not-a-version", want: false},

		// A pinned image tag overrides the inference in both directions. The
		// Kubernetes-to-etcd mapping is kubeadm's default, not a constraint,
		// and both pairings below are wire-compatible.
		{
			name:              "etcd 3.5 pinned on a release that would default to 3.6",
			kubernetesVersion: "v1.34.2",
			etcdImageTag:      "3.5.21-0",
			want:              false,
		},
		{
			name:              "etcd 3.6 pinned on a release that would default to 3.5",
			kubernetesVersion: "v1.33.6",
			etcdImageTag:      "3.6.5-0",
			want:              true,
		},
		{
			name:              "pinned tag at the etcd floor exactly",
			kubernetesVersion: "v1.31.0",
			etcdImageTag:      "3.6.0",
			want:              true,
		},
		{
			name:              "pinned tag just below the etcd floor",
			kubernetesVersion: "v1.34.2",
			etcdImageTag:      "3.5.24-0",
			want:              false,
		},
		{
			name:              "pinned tag with a v prefix",
			kubernetesVersion: "v1.33.6",
			etcdImageTag:      "v3.6.5-0",
			want:              true,
		},
		// A tag we cannot read is the dangerous case: guessing from the
		// Kubernetes version here would be exactly the override this gate
		// exists to avoid, so it refuses instead.
		{
			name:              "unparseable tag refuses rather than falling back",
			kubernetesVersion: "v1.34.2",
			etcdImageTag:      "latest",
			want:              false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, supportsBootstrapDefrag(tt.kubernetesVersion, tt.etcdImageTag))
		})
	}
}

// The threshold only reaches a member through the rendered etcd extraArgs, so
// the gate is worth nothing unless it holds there too. No golden fixture covers
// this: they all render at a supported version.
func TestClusterConfigurationBootstrapDefragThreshold(t *testing.T) {
	tests := []struct {
		name              string
		kubernetesVersion string
		want              string
	}{
		{name: "at the floor the threshold is set", kubernetesVersion: "v1.34.2", want: "200"},
		{name: "below the floor there is no threshold at all", kubernetesVersion: "v1.33.6", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validInitConfig()
			cfg.KubernetesVersion = tt.kubernetesVersion

			args := clusterConfiguration(cfg).Etcd.Local.ExtraArgs

			assert.Equal(t, tt.want, args[etcdBootstrapDefragThresholdFlagName])
			// Never the pre-3.6 spelling, on any release. The agent reads only
			// the stable name, so a member carrying the experimental one reports
			// no threshold and reads as an unmigrated node forever.
			assert.NotContains(t, args, "experimental-"+etcdBootstrapDefragThresholdFlagName)
		})
	}
}

// Unlike the threshold, this is a kube-apiserver flag with no version
// constraint, and pinning it on every release is the point: the design set out
// to stop the only remaining compactor depending on an upstream default.
func TestClusterConfigurationPinsEtcdCompactionInterval(t *testing.T) {
	for _, kubernetesVersion := range []string{"v1.33.6", "v1.34.2"} {
		t.Run(kubernetesVersion, func(t *testing.T) {
			cfg := validInitConfig()
			cfg.KubernetesVersion = kubernetesVersion

			assert.Equal(t, "5m", clusterConfiguration(cfg).APIServer.ExtraArgs[etcdCompactionIntervalFlagName])
		})
	}
}
