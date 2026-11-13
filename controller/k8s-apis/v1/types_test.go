// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestAddToSchemeRegistersEveryKind(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))

	for _, kind := range []string{
		"MachineImage", "MachineImageList",
		"ClusterRelease", "ClusterReleaseList",
		"NodePoolRelease", "NodePoolReleaseList",
	} {
		t.Run(kind, func(t *testing.T) {
			assert.True(t, scheme.Recognizes(SchemeGroupVersion.WithKind(kind)), "%s is not registered in %s", kind, SchemeGroupVersion)
		})
	}
}

func TestGroupVersion(t *testing.T) {
	assert.Equal(t, "internal.nvke.nvidia.com", GroupName)
	assert.Equal(t, "internal.nvke.nvidia.com/v1", GroupVersion.String())
}

func TestKubernetesVersionAnnotationKeys(t *testing.T) {
	tests := map[string]struct {
		got  string
		want string
	}{
		"fixed version": {
			got:  MachineInitKubernetesVersionAnnotation,
			want: "internal.nvke.nvidia.com/machine-init-kubernetes-version",
		},
		"injected version": {
			got:  MachineInitKubernetesVersionInjectedAnnotation,
			want: "internal.nvke.nvidia.com/machine-init-kubernetes-version-injected",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got)
		})
	}
}

func TestClusterReleaseDeepCopyIsIndependent(t *testing.T) {
	original := &ClusterRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Annotations: map[string]string{"key": "value"}},
		Spec: ClusterReleaseSpec{
			MachineImageRef: MachineImageReference{Name: "image"},
			MachineInit: MachineInit{
				UserDataTemplate: "#cloud-config\n",
				Infra:            &InfraBootstrap{PreKubeadmCommands: []string{"true"}},
			},
		},
	}

	clone := original.DeepCopy()
	clone.Annotations["key"] = "changed"
	clone.Spec.MachineInit.Infra.PreKubeadmCommands[0] = "false"

	assert.Equal(t, "value", original.Annotations["key"])
	assert.Equal(t, []string{"true"}, original.Spec.MachineInit.Infra.PreKubeadmCommands)
}
