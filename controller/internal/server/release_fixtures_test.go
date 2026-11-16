// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	internalv1 "github.com/dsx-ai-factory/nvke/controller/k8s-apis/v1"
)

const testImageName = "test-image"

// releaseScheme registers the release kinds the way the controller's client does.
func releaseScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, internalv1.AddToScheme(scheme))
	return scheme
}

// releaseClient is a cluster holding only the given objects, plus the image
// every test release boots.
func releaseClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	all := append([]ctrlclient.Object{testImage()}, objects...)
	return fake.NewClientBuilder().WithScheme(releaseScheme(t)).WithObjects(all...).Build()
}

func testImage() *internalv1.MachineImage {
	return &internalv1.MachineImage{
		ObjectMeta: metav1.ObjectMeta{Name: testImageName},
		Spec: internalv1.MachineImageSpec{
			OperatingSystem: internalv1.OperatingSystem{Family: "ubuntu", Version: "22.04"},
			Architecture:    "amd64",
			NICo:            internalv1.NICoBoot{IPXEScript: "#!ipxe\n"},
		},
	}
}

func releaseAnnotations() map[string]string {
	return map[string]string{
		internalv1.MachineInitKubernetesVersionAnnotation:         "1.31.12",
		internalv1.MachineInitKubernetesVersionInjectedAnnotation: "false",
	}
}

func testClusterRelease(name string, deprecated bool) *internalv1.ClusterRelease {
	return &internalv1.ClusterRelease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: releaseAnnotations()},
		Spec: internalv1.ClusterReleaseSpec{
			MachineImageRef: internalv1.MachineImageReference{Name: testImageName},
			MachineInit:     internalv1.MachineInit{UserDataTemplate: "#cloud-config\n"},
			Lifecycle:       internalv1.ReleaseLifecycle{Deprecated: deprecated},
		},
	}
}

func testNodePoolRelease(name string, deprecated bool) *internalv1.NodePoolRelease {
	return &internalv1.NodePoolRelease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: releaseAnnotations()},
		Spec: internalv1.NodePoolReleaseSpec{
			MachineImageRef: internalv1.MachineImageReference{Name: testImageName},
			MachineInit:     internalv1.MachineInit{UserDataTemplate: "#cloud-config\n"},
			Lifecycle:       internalv1.ReleaseLifecycle{Deprecated: deprecated},
		},
	}
}
