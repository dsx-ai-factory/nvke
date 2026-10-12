// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const machineImageDoc = `
metadata:
  name: nvke-nico-vms-24-04-amd64-fde
spec:
  operatingSystem:
    family: ubuntu
    version: "24.04"
  architecture: amd64
  variant: fde
  buildDate: "2026-03-23"
  nico:
    ipxeScript: |
      #!ipxe
      chain ${boot-loader} disk_strategy={{ .DiskSelectionStrategy }}
`

const releaseDoc = `
metadata:
  name: ubuntu-24-04-workers-v1
  annotations:
    internal.nke.nvidia.com/machine-init-k8s-version-injected: "true"
spec:
  machineImageRef:
    name: ubuntu-24-04-amd64-v1
  machineInit:
    userDataTemplate: |
      #cloud-config
      hostname: {{ .Hostname }}
`

func TestDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    Kind
		doc     string
		wantErr string
		check   func(t *testing.T, r Resource)
	}{
		{
			name: "machine image",
			kind: KindMachineImage,
			doc:  machineImageDoc,
			check: func(t *testing.T, r Resource) {
				assert.Equal(t, "nvke-nico-vms-24-04-amd64-fde", r.Metadata.Name)
				assert.Equal(t, "amd64", r.Spec["architecture"])
			},
		},
		{
			name: "node pool release keeps annotations",
			kind: KindNodePoolRelease,
			doc:  releaseDoc,
			check: func(t *testing.T, r Resource) {
				assert.Equal(t, map[string]string{"internal.nke.nvidia.com/machine-init-k8s-version-injected": "true"}, r.Metadata.Annotations)
			},
		},
		{
			name: "cluster release",
			kind: KindClusterRelease,
			doc:  releaseDoc,
		},
		{
			name: "spec is sent as written, with nothing added for omitted fields",
			kind: KindClusterRelease,
			doc:  releaseDoc,
			check: func(t *testing.T, r Resource) {
				assert.NotContains(t, r.Spec, "lifecycle")
				assert.Len(t, r.Spec, 2)
			},
		},
		{
			name: "release may set its lifecycle",
			kind: KindClusterRelease,
			doc: releaseDoc + `  lifecycle:
    deprecated: true
`,
			check: func(t *testing.T, r Resource) {
				assert.Equal(t, map[string]any{"deprecated": true}, r.Spec["lifecycle"])
			},
		},
		{
			name:    "apiVersion is not accepted",
			kind:    KindMachineImage,
			doc:     "apiVersion: internal.nke.nvidia.com/v1\n" + machineImageDoc,
			wantErr: `unknown field "apiVersion"`,
		},
		{
			name:    "kind is not accepted",
			kind:    KindMachineImage,
			doc:     "kind: MachineImage\n" + machineImageDoc,
			wantErr: `unknown field "kind"`,
		},
		{
			name:    "namespace is not accepted in metadata",
			kind:    KindMachineImage,
			doc:     "metadata:\n  name: a\n  namespace: nke-system\nspec: {}\n",
			wantErr: "invalid metadata",
		},
		{
			name:    "resourceVersion is not accepted in metadata",
			kind:    KindMachineImage,
			doc:     "metadata:\n  name: a\n  resourceVersion: \"7\"\nspec: {}\n",
			wantErr: "invalid metadata",
		},
		{
			name:    "metadata is required",
			kind:    KindMachineImage,
			doc:     "spec:\n  architecture: amd64\n",
			wantErr: "metadata is required",
		},
		{
			name:    "name is required",
			kind:    KindMachineImage,
			doc:     "metadata:\n  annotations: {}\nspec:\n  architecture: amd64\n",
			wantErr: "metadata.name is required",
		},
		{
			name:    "name must be a valid object name",
			kind:    KindMachineImage,
			doc:     "metadata:\n  name: Not_A_Name\nspec:\n  architecture: amd64\n",
			wantErr: "is not a valid name",
		},
		{
			name:    "spec is required",
			kind:    KindMachineImage,
			doc:     "metadata:\n  name: a\n",
			wantErr: "spec is required",
		},
		{
			name:    "unknown spec field",
			kind:    KindMachineImage,
			doc:     "metadata:\n  name: a\nspec:\n  architecture: amd64\n  arch: amd64\n",
			wantErr: `unknown field "arch"`,
		},
		{
			name:    "a field with the wrong capitalization is unknown",
			kind:    KindMachineImage,
			doc:     "metadata:\n  name: a\nspec:\n  Architecture: amd64\n",
			wantErr: `unknown field "Architecture"`,
		},
		{
			name:    "a nested field with the wrong capitalization is unknown",
			kind:    KindClusterRelease,
			doc:     "metadata:\n  name: a\nspec:\n  machineImageRef:\n    Name: i\n",
			wantErr: `unknown field`,
		},
		{
			name:    "unknown nested spec field",
			kind:    KindClusterRelease,
			doc:     "metadata:\n  name: a\nspec:\n  machineImageRef:\n    name: i\n    namespace: x\n",
			wantErr: `unknown field "machineImageRef.namespace"`,
		},
		{
			name:    "a machine image spec does not fit a release",
			kind:    KindClusterRelease,
			doc:     machineImageDoc,
			wantErr: "invalid spec",
		},
		{
			name:    "wrongly typed value",
			kind:    KindMachineImage,
			doc:     "metadata:\n  name: a\nspec:\n  architecture: [amd64]\n",
			wantErr: "invalid spec",
		},
		{
			name:    "more than one document",
			kind:    KindMachineImage,
			doc:     machineImageDoc + "---\n" + machineImageDoc,
			wantErr: "multiple YAML documents",
		},
		{
			name:    "empty document",
			kind:    KindMachineImage,
			doc:     "",
			wantErr: "empty document",
		},
		{
			name:    "not a mapping",
			kind:    KindMachineImage,
			doc:     "- a\n- b\n",
			wantErr: "one mapping with a metadata and a spec, not a list",
		},
		{
			name: "JSON is accepted",
			kind: KindClusterRelease,
			doc:  `{"metadata":{"name":"a"},"spec":{"machineImageRef":{"name":"i"},"machineInit":{"userDataTemplate":"#cloud-config\n"}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Decode(tt.kind, []byte(tt.doc))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestDecodeUnknownKind(t *testing.T) {
	t.Parallel()

	_, err := Decode(Kind("Pod"), []byte(machineImageDoc))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown kind "Pod"`)
}

func TestManifest(t *testing.T) {
	t.Parallel()

	r, err := Decode(KindNodePoolRelease, []byte(releaseDoc))
	require.NoError(t, err)

	got := Manifest(KindNodePoolRelease, r)

	assert.Equal(t, "internal.nke.nvidia.com/v1", got["apiVersion"])
	assert.Equal(t, "NodePoolRelease", got["kind"])
	assert.Equal(t, map[string]any{
		"name": "ubuntu-24-04-workers-v1",
		"annotations": map[string]any{
			"internal.nke.nvidia.com/machine-init-k8s-version-injected": "true",
		},
	}, got["metadata"], "only what the operator wrote: nvkectl adds no GitOps annotations")
	assert.Equal(t, r.Spec, got["spec"])
	assert.Len(t, got, 4)
}

func TestManifestOmitsEmptyAnnotations(t *testing.T) {
	t.Parallel()

	got := Manifest(KindMachineImage, Resource{Metadata: Metadata{Name: "a"}, Spec: map[string]any{}})

	assert.Equal(t, map[string]any{"name": "a"}, got["metadata"])
}

// An unquoted date is a YAML timestamp. It must reach the cluster as the text
// the operator wrote: the MachineImage CRD requires a date, and a full
// timestamp is not one.
func TestDecodeKeepsAnUnquotedBuildDateAsWritten(t *testing.T) {
	t.Parallel()

	doc := "metadata:\n  name: a\nspec:\n  buildDate: 2026-03-23\n"

	r, err := Decode(KindMachineImage, []byte(doc))

	require.NoError(t, err)
	assert.Equal(t, "2026-03-23", r.Spec["buildDate"])
	assert.Equal(t, "2026-03-23", Manifest(KindMachineImage, r)["spec"].(map[string]any)["buildDate"])
}

func TestDecodeKeepsAQuotedBuildDateAsWritten(t *testing.T) {
	t.Parallel()

	r, err := Decode(KindMachineImage, []byte("metadata:\n  name: a\nspec:\n  buildDate: \"2026-03-23\"\n"))

	require.NoError(t, err)
	assert.Equal(t, "2026-03-23", r.Spec["buildDate"])
}
