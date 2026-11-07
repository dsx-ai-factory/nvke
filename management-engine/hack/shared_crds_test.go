// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hack

import (
	"bytes"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

func sharedCRDs(t *testing.T, input []byte) map[string]unstructured.Unstructured {
	t.Helper()
	result := map[string]unstructured.Unstructured{}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(input), 4096)
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if object.GetKind() != "CustomResourceDefinition" {
			continue
		}
		if _, ok := result[object.GetName()]; ok {
			t.Fatalf("duplicate CRD %s", object.GetName())
		}
		result[object.GetName()] = object
	}
	return result
}

func TestSharedCRDs(t *testing.T) {
	const chartRoot = "../../controller/deploy/charts/"
	canonical := map[string]unstructured.Unstructured{}
	paths, err := filepath.Glob(chartRoot + "nvke-crds/bases/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		maps.Copy(canonical, sharedCRDs(t, data))
	}
	if len(canonical) != 3 {
		t.Fatalf("expected three catalog CRDs, got %d", len(canonical))
	}
	releaseNames := []string{
		"clusterreleases.internal.nvke.nvidia.com", "machineimages.internal.nvke.nvidia.com",
		"nodepoolreleases.internal.nvke.nvidia.com",
	}
	allNames := make([]string, 0, len(canonical))
	for name := range canonical {
		allNames = append(allNames, name)
	}
	sort.Strings(allNames)
	for _, tc := range []struct {
		name, chart string
		values      []string
		want        []string
	}{
		{"standalone", "nvke-crds", nil, allNames},
		{"disabled", "nvke-crds", []string{"--set", "enabled=false"}, []string{}},
		{"existing-controller", "controller", nil, releaseNames},
		{"external-owner", "controller", []string{"--set", "releaseCRDs.install=false"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helm := os.Getenv("HELM")
			if helm == "" {
				helm = "helm"
			}
			args := append([]string{"template", "schema-check", chartRoot + tc.chart}, tc.values...)
			output, err := exec.Command(helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
			objects := sharedCRDs(t, output)
			names := make([]string, 0, len(objects))
			for name, object := range objects {
				names = append(names, name)
				expected, ok := canonical[name]
				if !ok || !reflect.DeepEqual(expected.Object["spec"], object.Object["spec"]) {
					t.Fatalf("schema %s differs from its canonical generated definition", name)
				}
				annotations := object.GetAnnotations()
				if annotations["helm.sh/resource-policy"] != "keep" ||
					annotations["argocd.argoproj.io/sync-options"] != "Prune=false" {
					t.Fatalf("missing retention on %s: %v", name, annotations)
				}
			}
			sort.Strings(names)
			if !reflect.DeepEqual(names, tc.want) {
				t.Fatalf("got %v, want %v", names, tc.want)
			}
		})
	}
}
