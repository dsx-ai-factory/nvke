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

const controllerChartDir = "../../deploy/charts/controller"

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

// TestControllerChartCRDRendering checks that the controller chart renders the generated CRDs.
func TestControllerChartCRDRendering(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm required for render checks")
	}
	paths, err := filepath.Glob(filepath.Join(controllerChartDir, "files", "crds", "*.yaml"))
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
	require.Equal(t, wantNames, allNames, "files/crds holds the wrong definitions")

	for _, test := range []struct {
		name string
		set  []string
		want []string
	}{
		{"installed by default", nil, wantNames},
		{"left to another installer", []string{"--set", "releaseCRDs.install=false"}, []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"template", "schema-check", controllerChartDir}, test.set...)
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
				require.Equal(t, "-2", annotations["argocd.argoproj.io/sync-wave"], "retention on %s", name)
				require.Equal(t, "Prune=false", annotations["argocd.argoproj.io/sync-options"], "retention on %s", name)
				// The retention set is built once per CRD. Reusing one map across the loop would
				// still pass the three checks above, so hold the annotations to the generated
				// ones plus exactly those three.
				generated, _ := want["metadata"].(map[string]any)["annotations"].(map[string]any)
				require.Len(t, annotations, len(generated)+3, "annotations of %s are the generated ones plus the retention set", name)
			}
			sort.Strings(names)
			require.Equal(t, test.want, names)
		})
	}
}
