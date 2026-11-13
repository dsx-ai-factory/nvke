// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

const chartsDir = "../../deploy/charts"

// crdDocuments returns every CustomResourceDefinition in a multi-document YAML
// stream, keyed by name. Anything else in the stream is ignored.
func crdDocuments(t *testing.T, stream string) map[string]map[string]any {
	t.Helper()
	crds := map[string]map[string]any{}
	for _, document := range strings.Split(stream, "\n---") {
		var object map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(document), &object), "decoding YAML document:\n%s", document)
		if object["kind"] != "CustomResourceDefinition" {
			continue
		}
		metadata, _ := object["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		require.NotContains(t, crds, name, "duplicate CRD %s", name)
		crds[name] = object
	}
	return crds
}

// assembleControllerChart copies the controller chart into dir with the nvke-crds
// chart as its dependency, the way a built chart carries it, without running
// helm dependency build. Any dependency archive a local build left behind is
// dropped so it cannot shadow the sources.
func assembleControllerChart(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.CopyFS(dir, os.DirFS(filepath.Join(chartsDir, "controller"))))
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "charts")))
	require.NoError(t, os.CopyFS(filepath.Join(dir, "charts", "nvke-crds"), os.DirFS(filepath.Join(chartsDir, "nvke-crds"))))
}

// TestNVKECRDsRendering holds the nvke-crds chart to the files this module
// generates into the chart's bases directory: it renders every one of them,
// schema intact, with the retention annotations that keep an uninstall or an
// Argo prune from deleting the custom resources, and the controller chart pulls
// it in unless releaseCRDs.install says another installer owns the schemas.
func TestNVKECRDsRendering(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm required for render checks")
	}
	crdsChart := filepath.Join(chartsDir, "nvke-crds")
	paths, err := filepath.Glob(filepath.Join(crdsChart, "bases", "*.yaml"))
	require.NoError(t, err)
	canonical := map[string]map[string]any{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		for name, crd := range crdDocuments(t, string(data)) {
			canonical[name] = crd
		}
	}
	wantNames := []string{
		"clusterreleases.internal.nvke.nvidia.com",
		"machineimages.internal.nvke.nvidia.com",
		"nodepoolreleases.internal.nvke.nvidia.com",
	}
	allNames := make([]string, 0, len(canonical))
	for name := range canonical {
		allNames = append(allNames, name)
	}
	sort.Strings(allNames)
	require.Equal(t, wantNames, allNames, "nvke-crds/bases holds the wrong definitions")

	controllerChart := filepath.Join(t.TempDir(), "controller")
	assembleControllerChart(t, controllerChart)

	for _, test := range []struct {
		name  string
		chart string
		set   []string
		want  []string
	}{
		{"standalone", crdsChart, nil, wantNames},
		{"disabled", crdsChart, []string{"--set", "enabled=false"}, []string{}},
		{"controller chart installs them by default", controllerChart, nil, wantNames},
		{"controller chart leaves them to another installer", controllerChart, []string{"--set", "releaseCRDs.install=false"}, []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"template", "schema-check", test.chart}, test.set...)
			output, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
			require.NoError(t, err, "helm template:\n%s", output)
			rendered := crdDocuments(t, string(output))
			names := make([]string, 0, len(rendered))
			for name, crd := range rendered {
				names = append(names, name)
				want, ok := canonical[name]
				require.True(t, ok, "rendered CRD %s has no generated definition", name)
				require.Equal(t, want["spec"], crd["spec"], "rendered schema %s differs from its generated definition", name)
				metadata, _ := crd["metadata"].(map[string]any)
				annotations, _ := metadata["annotations"].(map[string]any)
				require.Equal(t, "keep", annotations["helm.sh/resource-policy"], "retention on %s", name)
				require.Equal(t, "Prune=false", annotations["argocd.argoproj.io/sync-options"], "retention on %s", name)
			}
			sort.Strings(names)
			require.Equal(t, test.want, names)
		})
	}
}
