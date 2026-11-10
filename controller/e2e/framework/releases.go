//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// releaseCRDPaths returns the generated release CRD manifests the controller
// chart installs.
func releaseCRDPaths(t testing.TB) []string {
	t.Helper()

	paths := make([]string, 0, 3)
	for _, name := range []string{"machineimages", "clusterreleases", "nodepoolreleases"} {
		path := filepath.Join(RepoRoot(t), "deploy", "charts", "nvke-crds", "bases", "internal.nvke.nvidia.com_"+name+".yaml")
		_, err := os.Stat(path)
		require.NoError(t, err, "expected canonical release CRD at %s", path)
		paths = append(paths, path)
	}
	return paths
}

// installReleases creates every manifest of the committed fixture releases
// (test/fixtures/releases) through the API server.
func installReleases(t testing.TB, cfg *rest.Config) {
	t.Helper()

	dir := filepath.Join(RepoRoot(t), "test", "fixtures", "releases")
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "expected the fixture releases under %s", dir)
	sort.Strings(paths)

	api, err := client.New(cfg, client.Options{})
	require.NoError(t, err, "creating a client for the envtest API server")
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		obj := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal(data, &obj.Object), "parsing %s", path)
		require.NoError(t, api.Create(t.Context(), obj), "installing %s", filepath.Base(path))
	}
}
