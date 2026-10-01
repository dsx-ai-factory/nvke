// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	enginev1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/machinerelease"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestClient_ResolveNodePoolUpgrade(t *testing.T) {
	t.Parallel()

	reader := testReleaseReader(t, releaseFixtures{
		newReleaseFixture("nke-injected", "", "amd64", "", "2026-01-01", true, false),
		newReleaseFixture("nke-embedded-1.31.9", "1.31.9", "amd64", "", "2026-01-01", false, false),
		newReleaseFixture("nke-embedded-1.32.0", "1.32.0", "amd64", "", "2026-01-10", false, false),
	})

	nodePoolID := uuid.New()

	// The fixtures are ClusterReleases, so these pools are control-plane pools;
	// the non-control-plane branch reads NodePoolReleases and is covered below.
	controlPlanePool := func(release string) db.NodePool {
		pool := db.NodePool{ID: nodePoolID, KubernetesVersion: "1.31.9", ControlPlane: true}
		pool.SetNico(db.NicoNodePoolProvider{OSProfile: release})
		return pool
	}

	t.Run("injected release keeps its name, updates KubernetesVersion", func(t *testing.T) {
		t.Parallel()

		updated, err := New().ResolveNodePoolUpgrade(t.Context(), reader, controlPlanePool("nke-injected"), "1.32.0", discardLogger())
		require.NoError(t, err)
		assert.Equal(t, "1.32.0", updated.KubernetesVersion)
		provider, err := updated.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, "nke-injected", provider.OSProfile, "an injected release should not be selected anew")
	})

	t.Run("embedded release selects a new release for the target version", func(t *testing.T) {
		t.Parallel()

		updated, err := New().ResolveNodePoolUpgrade(t.Context(), reader, controlPlanePool("nke-embedded-1.31.9"), "1.32.0", discardLogger())
		require.NoError(t, err)
		assert.Equal(t, "1.32.0", updated.KubernetesVersion)
		provider, err := updated.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, "nke-embedded-1.32.0", provider.OSProfile)
	})

	t.Run("missing nico payload errors", func(t *testing.T) {
		t.Parallel()

		_, err := New().ResolveNodePoolUpgrade(t.Context(), reader, db.NodePool{ID: nodePoolID}, "1.32.0", discardLogger())
		require.Error(t, err)
	})

	t.Run("no reader errors", func(t *testing.T) {
		t.Parallel()

		_, err := New().ResolveNodePoolUpgrade(t.Context(), nil, controlPlanePool("nke-injected"), "1.32.0", discardLogger())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "release reader is not configured")
	})
}

// The upgrade is the path that reads the Kubernetes-version annotations, so it
// is where a release that declares its version regime as neither embedded nor
// injected is reported.
func TestClient_ResolveNodePoolUpgradeRejectsBadInjectionAnnotation(t *testing.T) {
	t.Parallel()

	fixture := newReleaseFixture("nke-undeclared", "1.31.9", "amd64", "", "2026-01-01", false, false)
	fixture.release.Annotations[enginev1.MachineInitKubernetesVersionInjectedAnnotation] = "maybe"
	reader := testReleaseReader(t, releaseFixtures{fixture})

	pool := db.NodePool{ID: uuid.New(), KubernetesVersion: "1.31.9", ControlPlane: true}
	pool.SetNico(db.NicoNodePoolProvider{OSProfile: "nke-undeclared"})

	_, err := New().ResolveNodePoolUpgrade(t.Context(), reader, pool, "1.32.0", discardLogger())
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
	assert.Contains(t, err.Error(), "maybe")
}

// A worker node pool stores a NodePoolRelease name, so resolution has to read
// that kind rather than the ClusterRelease the control-plane pools use.
func TestClient_ResolveNodePoolUpgradeReadsNodePoolReleases(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, enginev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(newNodePoolReleaseFixture("nke-worker-injected", true)...).Build()

	pool := db.NodePool{ID: uuid.New(), KubernetesVersion: "1.31.9"}
	pool.SetNico(db.NicoNodePoolProvider{OSProfile: "nke-worker-injected"})

	updated, err := New().ResolveNodePoolUpgrade(t.Context(), reader, pool, "1.32.0", discardLogger())
	require.NoError(t, err)
	assert.Equal(t, "1.32.0", updated.KubernetesVersion)
}

func TestSelectUpgradeRelease(t *testing.T) {
	releases := releaseFixtures{
		newReleaseFixture("nke-1.31.9-260101", "1.31.9", "amd64", "", "2026-01-01", false, false),
		newReleaseFixture("nke-1.31.9-260115", "1.31.9", "amd64", "", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260120", "1.31.9", "amd64", "", "2026-01-20", false, false),
		newReleaseFixture("nke-1.31.9-260115-arm64", "1.31.9", "arm64", "arm64", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260115-ovn", "1.31.9", "amd64", "ovn", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260120-ovn", "1.31.9", "amd64", "ovn", "2026-01-20", false, false),
		newReleaseFixture("nke-1.31.9-260110-osmo", "1.31.9", "amd64", "osmo", "2026-01-10", false, false),
		newReleaseFixture("nke-1.30.5-260115", "1.30.5", "amd64", "", "2026-01-15", false, false),
	}

	tests := []struct {
		name               string
		k8sVersion         string
		currentProfileName string
		expectedProfile    string
		expectError        bool
		errorContains      string
	}{
		{
			name:               "no current profile - no default to amd64/22.04, returns error (no matching profiles)",
			k8sVersion:         "1.31.9",
			currentProfileName: "",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "no OSProfile found for Kubernetes version 1.31.9",
		},
		{
			name:               "preserve amd64 architecture",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.30.5-260115",
			expectedProfile:    "nke-1.31.9-260120",
			expectError:        false,
		},
		{
			name:               "preserve arm64 architecture",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260115-arm64",
			expectedProfile:    "nke-1.31.9-260115-arm64",
			expectError:        false,
		},
		{
			name:               "preserve ovn variant",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260115-ovn",
			expectedProfile:    "nke-1.31.9-260120-ovn",
			expectError:        false,
		},
		{
			name:               "preserve osmo variant",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260110-osmo",
			expectedProfile:    "nke-1.31.9-260110-osmo",
			expectError:        false,
		},
		{
			name:               "no matching profile found",
			k8sVersion:         "1.32.0",
			currentProfileName: "",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "no OSProfile found for Kubernetes version 1.32.0",
		},
		{
			name:               "no matching arm64 profile for version",
			k8sVersion:         "1.30.5",
			currentProfileName: "nke-1.31.9-260115-arm64",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "no OSProfile found for Kubernetes version 1.30.5",
		},
		{
			name:               "pick newest by build_date when multiple matches",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260101",
			expectedProfile:    "nke-1.31.9-260120",
			expectError:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := testReleaseReader(t, releases)
			result, err := selectUpgradeRelease(t.Context(), reader, true, tt.k8sVersion, tt.currentProfileName,
				currentReleaseImage(t, reader, tt.currentProfileName), discardLogger())

			if tt.expectError {
				require.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedProfile, result)
			}
		})
	}
}

func TestSelectUpgradeRelease_ListError(t *testing.T) {
	reader := failingReleaseReader(t, fmt.Errorf("releases unavailable"))

	_, err := selectUpgradeRelease(t.Context(), reader, true, "1.31.9", "", nil, discardLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing ClusterReleases")
}

// The selection cannot guess the metadata of a release that is not installed,
// so resolving the current release fails before any selection happens.
func TestResolveRelease_CurrentReleaseNotInstalled(t *testing.T) {
	reader := testReleaseReader(t, releaseFixtures{
		newReleaseFixture("nke-1.31.9-260101", "1.31.9", "amd64", "", "2026-01-01", false, false),
	})

	_, err := resolveRelease(t.Context(), reader, true, "nke-unknown-profile")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `ClusterRelease "nke-unknown-profile" is not installed`)
}

func TestSelectUpgradeRelease_BuildDateOrdering(t *testing.T) {
	reader := testReleaseReader(t, releaseFixtures{
		newReleaseFixture("nke-1.30.5-260101", "1.30.5", "amd64", "", "2026-01-01", false, false),
		newReleaseFixture("nke-1.31.9-260105", "1.31.9", "amd64", "", "2026-01-05", false, false),
		newReleaseFixture("nke-1.31.9-260120", "1.31.9", "amd64", "", "2026-01-20", false, false),
		newReleaseFixture("nke-1.31.9-260115", "1.31.9", "amd64", "", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260110", "1.31.9", "amd64", "", "2026-01-10", false, false),
		newReleaseFixture("nke-1.31.9-260125", "1.31.9", "amd64", "", "2026-01-25", false, false),
	})

	result, err := selectUpgradeRelease(t.Context(), reader, true, "1.31.9", "nke-1.30.5-260101",
		currentReleaseImage(t, reader, "nke-1.30.5-260101"), discardLogger())
	require.NoError(t, err)
	assert.Equal(t, "nke-1.31.9-260125", result, "Should pick profile with newest build_date (2026-01-25)")
}

func TestSelectUpgradeRelease_VariantPreservation(t *testing.T) {
	reader := testReleaseReader(t, releaseFixtures{
		newReleaseFixture("nke-1.30.5-260101-ovn", "1.30.5", "amd64", "ovn", "2026-01-01", false, false),
		newReleaseFixture("nke-1.31.9-260115", "1.31.9", "amd64", "", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260115-ovn", "1.31.9", "amd64", "ovn", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260120-ovn", "1.31.9", "amd64", "ovn", "2026-01-20", false, false),
	})

	result, err := selectUpgradeRelease(t.Context(), reader, true, "1.31.9", "nke-1.30.5-260101-ovn",
		currentReleaseImage(t, reader, "nke-1.30.5-260101-ovn"), discardLogger())
	require.NoError(t, err)
	assert.Equal(t, "nke-1.31.9-260120-ovn", result, "Should preserve 'ovn' variant and pick newest")
}

func TestSelectUpgradeRelease_SkipsDeprecatedAndKeepsInjected(t *testing.T) {
	reader := testReleaseReader(t, releaseFixtures{
		newReleaseFixture("nke-1.30.5-260101", "1.30.5", "amd64", "", "2026-01-01", false, true),
		newReleaseFixture("nke-1.31.9-retired", "1.31.9", "amd64", "", "2026-02-01", false, true),
		newReleaseFixture("nke-1.31.9-fixed", "1.31.9", "amd64", "", "2026-01-10", false, false),
		newReleaseFixture("nke-injected", "", "amd64", "", "2026-01-20", true, false),
	})

	result, err := selectUpgradeRelease(t.Context(), reader, true, "1.35.0", "nke-1.30.5-260101",
		currentReleaseImage(t, reader, "nke-1.30.5-260101"), discardLogger())
	require.NoError(t, err)
	assert.Equal(t, "nke-injected", result)
}

func TestSelectUpgradeRelease_LoggingBehavior(t *testing.T) {
	reader := testReleaseReader(t, releaseFixtures{
		newReleaseFixture("nke-1.30.5-260101-ovn", "1.30.5", "amd64", "ovn", "2026-01-01", false, false),
		newReleaseFixture("nke-1.31.9-260115-ovn", "1.31.9", "amd64", "ovn", "2026-01-15", false, false),
	})
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	result, err := selectUpgradeRelease(t.Context(), reader, true, "1.31.9", "nke-1.30.5-260101-ovn",
		currentReleaseImage(t, reader, "nke-1.30.5-260101-ovn"), logger)
	require.NoError(t, err)
	assert.Equal(t, "nke-1.31.9-260115-ovn", result)
}

// releaseFixture is a ClusterRelease and the MachineImage it boots.
type releaseFixture struct {
	release *enginev1.ClusterRelease
	image   *enginev1.MachineImage
}

type releaseFixtures []releaseFixture

func (f releaseFixtures) objects() []ctrlclient.Object {
	objects := make([]ctrlclient.Object, 0, 2*len(f))
	for _, fixture := range f {
		objects = append(objects, fixture.release, fixture.image)
	}
	return objects
}

func testReleaseReader(t *testing.T, fixtures releaseFixtures) ctrlclient.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, enginev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(fixtures.objects()...).Build()
}

// currentReleaseImage resolves the image a named release boots, the way
// ResolveNodePoolUpgrade does before it selects an upgrade target.
func currentReleaseImage(t *testing.T, reader ctrlclient.Reader, name string) *enginev1.MachineImage {
	t.Helper()
	if name == "" {
		return nil
	}
	resolved, err := resolveRelease(t.Context(), reader, true, name)
	require.NoError(t, err)
	return resolved.image
}

// failingReleaseReader reads releases from an API server that is down.
func failingReleaseReader(t *testing.T, err error) ctrlclient.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, enginev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
			return err
		},
	}).Build()
}

// newReleaseFixture builds a ClusterRelease named after, and booting, an image
// with the given attributes. An injected release carries no Kubernetes version.
func newReleaseFixture(name, version, architecture, variant, buildDate string, injected, deprecated bool) releaseFixture {
	injectedValue := "false"
	if injected {
		injectedValue = "true"
		version = ""
	}
	return releaseFixture{
		release: &enginev1.ClusterRelease{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Annotations: map[string]string{
					enginev1.MachineInitKubernetesVersionAnnotation:         version,
					enginev1.MachineInitKubernetesVersionInjectedAnnotation: injectedValue,
				},
			},
			Spec: enginev1.ClusterReleaseSpec{
				MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(name)},
				MachineInit:     enginev1.MachineInit{UserDataTemplate: "#cloud-config\n"},
				Lifecycle:       enginev1.ReleaseLifecycle{Deprecated: deprecated},
			},
		},
		image: &enginev1.MachineImage{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: enginev1.MachineImageSpec{
				OperatingSystem: enginev1.OperatingSystem{Family: "ubuntu", Version: "22.04"},
				Architecture:    architecture,
				Variant:         variant,
				BuildDate:       buildDate,
				NICo:            enginev1.NICoBoot{IPXEScript: "#!ipxe\n"},
			},
		},
	}
}

// newNodePoolReleaseFixture builds a NodePoolRelease and the MachineImage it
// boots, the pair a worker pool's resolution reads.
func newNodePoolReleaseFixture(name string, injected bool) []ctrlclient.Object {
	injectedValue := "false"
	if injected {
		injectedValue = "true"
	}
	return []ctrlclient.Object{
		&enginev1.NodePoolRelease{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Annotations: map[string]string{
					enginev1.MachineInitKubernetesVersionAnnotation:         "",
					enginev1.MachineInitKubernetesVersionInjectedAnnotation: injectedValue,
				},
			},
			Spec: enginev1.NodePoolReleaseSpec{
				MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(name)},
				MachineInit:     enginev1.MachineInit{UserDataTemplate: "#cloud-config\n"},
			},
		},
		&enginev1.MachineImage{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: enginev1.MachineImageSpec{
				OperatingSystem: enginev1.OperatingSystem{Family: "ubuntu", Version: "22.04"},
				Architecture:    "amd64",
				BuildDate:       "2026-01-01",
				NICo:            enginev1.NICoBoot{IPXEScript: "#!ipxe\n"},
			},
		},
	}
}
