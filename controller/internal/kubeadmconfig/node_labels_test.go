// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestBootstrapNodeLabelsReachKubelet(t *testing.T) {
	for _, tc := range []struct{ name, labels string }{
		{name: "without labels"},
		{name: "DPU host and existing labels", labels: "k8s.ovn.org/dpu-host=,lab=managed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := tc.labels
			initCfg := validInitConfig()
			initCfg.Labels = labels
			joinCfg := validWorkerJoinConfig()
			joinCfg.Labels = labels
			for _, render := range []func() (*BootstrapArtifacts, error){initCfg.Render, joinCfg.Render} {
				artifacts, err := render()
				require.NoError(t, err)
				var config struct {
					NodeRegistration struct {
						KubeletExtraArgs map[string]string `json:"kubeletExtraArgs"`
					} `json:"nodeRegistration"`
				}
				require.NoError(t, yaml.NewYAMLOrJSONDecoder(bytes.NewReader(artifacts.ConfigYAML), 4096).Decode(&config))
				args := config.NodeRegistration.KubeletExtraArgs
				if labels == "" {
					require.NotContains(t, args, "node-labels")
				} else {
					require.Equal(t, labels, args["node-labels"])
				}
			}
		})
	}
}
