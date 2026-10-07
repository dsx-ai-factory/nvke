// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nke/cli/internal/operator"
)

// fakeClient is an in-memory operator.Client that records the calls it gets.
type fakeClient struct {
	items map[string]operator.Resource // keyed "<kind>/<name>"
	calls []string
	// failWith, when set, is returned by every call.
	failWith error
}

func newFakeClient(items ...fakeItem) *fakeClient {
	f := &fakeClient{items: map[string]operator.Resource{}}
	for _, it := range items {
		f.items[key(it.kind, it.res.Metadata.Name)] = it.res
	}
	return f
}

type fakeItem struct {
	kind operator.Kind
	res  operator.Resource
}

func key(kind operator.Kind, name string) string { return string(kind) + "/" + name }

func (f *fakeClient) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeClient) Create(_ context.Context, kind operator.Kind, r operator.Resource) (operator.Resource, error) {
	f.record("create " + key(kind, r.Metadata.Name))
	if f.failWith != nil {
		return operator.Resource{}, f.failWith
	}
	if _, ok := f.items[key(kind, r.Metadata.Name)]; ok {
		return operator.Resource{}, fmt.Errorf("%s %q: %w", kind, r.Metadata.Name, operator.ErrAlreadyExists)
	}
	f.items[key(kind, r.Metadata.Name)] = r
	return r, nil
}

func (f *fakeClient) Get(_ context.Context, kind operator.Kind, name string) (operator.Resource, error) {
	f.record("get " + key(kind, name))
	if f.failWith != nil {
		return operator.Resource{}, f.failWith
	}
	r, ok := f.items[key(kind, name)]
	if !ok {
		return operator.Resource{}, fmt.Errorf("%s %q: %w", kind, name, operator.ErrNotFound)
	}
	return r, nil
}

func (f *fakeClient) List(_ context.Context, kind operator.Kind) ([]operator.Resource, error) {
	f.record("list " + string(kind))
	if f.failWith != nil {
		return nil, f.failWith
	}
	var out []operator.Resource
	for k, r := range f.items {
		if strings.HasPrefix(k, string(kind)+"/") {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

func (f *fakeClient) Delete(_ context.Context, kind operator.Kind, name string) error {
	f.record("delete " + key(kind, name))
	if f.failWith != nil {
		return f.failWith
	}
	if _, ok := f.items[key(kind, name)]; !ok {
		return fmt.Errorf("%s %q: %w", kind, name, operator.ErrNotFound)
	}
	delete(f.items, key(kind, name))
	return nil
}

func (f *fakeClient) SetDeprecated(_ context.Context, kind operator.Kind, name string, deprecated bool) (operator.Resource, error) {
	f.record(fmt.Sprintf("deprecate %s %t", key(kind, name), deprecated))
	if f.failWith != nil {
		return operator.Resource{}, f.failWith
	}
	r, ok := f.items[key(kind, name)]
	if !ok {
		return operator.Resource{}, fmt.Errorf("%s %q: %w", kind, name, operator.ErrNotFound)
	}
	r.Spec["lifecycle"] = map[string]any{"deprecated": deprecated}
	f.items[key(kind, name)] = r
	return r, nil
}

func (f *fakeClient) clientFn() ClientFn {
	return func() (operator.Client, error) { return f, nil }
}

// run executes a "<verb> ..." command line against the commands built by build
// and returns what it wrote.
func run(t *testing.T, verb string, build func() []*cobra.Command, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	parent := &cobra.Command{Use: verb, SilenceUsage: true, SilenceErrors: true}
	for _, c := range build() {
		parent.AddCommand(c)
	}
	var out, errOut bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errOut)
	parent.SetIn(strings.NewReader(stdin))
	parent.SetArgs(args)
	err = parent.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

const imageDoc = `metadata:
  name: img-1
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
      chain ${loader}
`

const releaseDoc = `metadata:
  name: rel-1
spec:
  machineImageRef:
    name: img-1
  machineInit:
    userDataTemplate: |
      #cloud-config
`

func image(name string) fakeItem {
	return fakeItem{operator.KindMachineImage, operator.Resource{
		Metadata: operator.Metadata{Name: name},
		Spec: map[string]any{
			"operatingSystem": map[string]any{"family": "ubuntu", "version": "24.04"},
			"architecture":    "amd64", "variant": "fde", "buildDate": "2026-03-23",
		},
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}}
}

func release(kind operator.Kind, name, imageName string, deprecated bool) fakeItem {
	return fakeItem{kind, operator.Resource{
		Metadata: operator.Metadata{Name: name},
		Spec: map[string]any{
			"machineImageRef": map[string]any{"name": imageName},
			"lifecycle":       map[string]any{"deprecated": deprecated},
		},
		CreatedAt: time.Now().Add(-26 * time.Hour),
	}}
}

type createCase struct {
	name     string
	verb     string
	doc      string
	wantKind operator.Kind
	wantOut  string
}

// existing is what must already be on the site: a release needs its machine image.
func (c createCase) existing() []fakeItem {
	if c.wantKind == operator.KindMachineImage {
		return nil
	}
	return []fakeItem{image("img-1")}
}

func TestCreate(t *testing.T) {
	t.Parallel()

	tests := []createCase{
		{"machine image", "machineimage", imageDoc, operator.KindMachineImage, `Machine image "img-1" created`},
		{"cluster release", "clusterrelease", releaseDoc, operator.KindClusterRelease, `Cluster release "rel-1" created`},
		{"node pool release", "nodepoolrelease", releaseDoc, operator.KindNodePoolRelease, `Node pool release "rel-1" created`},
	}
	for _, tt := range tests {
		t.Run(tt.name+" from a file", func(t *testing.T) {
			t.Parallel()
			f := newFakeClient(tt.existing()...)

			out, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, "",
				tt.verb, "-f", writeFile(t, tt.doc))

			require.NoError(t, err)
			assert.Contains(t, out, tt.wantOut)
			assert.Contains(t, f.calls[len(f.calls)-1], "create "+string(tt.wantKind)+"/")
		})
		t.Run(tt.name+" from stdin", func(t *testing.T) {
			t.Parallel()
			f := newFakeClient(tt.existing()...)

			out, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, tt.doc,
				tt.verb, "-f", "-")

			require.NoError(t, err)
			assert.Contains(t, out, tt.wantOut)
		})
	}
}

func TestCreateRejectsTheDocumentBeforeAnyClientIsBuilt(t *testing.T) {
	t.Parallel()

	built := false
	clientFn := func() (operator.Client, error) { built = true; return newFakeClient(), nil }

	_, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(clientFn) }, "",
		"machineimage", "-f", writeFile(t, "apiVersion: internal.nke.nvidia.com/v1\n"+imageDoc))

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown field "apiVersion"`)
	assert.False(t, built, "no site is needed to reject a malformed document")
}

func TestCreateListDocumentSaysOneResourcePerDocument(t *testing.T) {
	t.Parallel()
	f := newFakeClient()

	_, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, "",
		"machineimage", "-f", writeFile(t, "- metadata:\n    name: a\n  spec: {}\n"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "one mapping with a metadata and a spec, not a list")
	assert.Empty(t, f.calls)
}

func TestCreateHelpStartsWithASentence(t *testing.T) {
	t.Parallel()

	out, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(newFakeClient().clientFn()) }, "", "machineimage", "--help")

	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "Create a machine image on the active site's management cluster.\n"), out)
}

func TestCreateExistingNameFails(t *testing.T) {
	t.Parallel()
	f := newFakeClient(image("img-1"))

	_, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, "",
		"machineimage", "-f", writeFile(t, imageDoc))

	require.Error(t, err)
	assert.ErrorIs(t, err, operator.ErrAlreadyExists)
}

func TestCreateReleaseNeedsItsMachineImage(t *testing.T) {
	t.Parallel()

	for _, verb := range []string{"clusterrelease", "nodepoolrelease"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient()

			_, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, "",
				verb, "-f", writeFile(t, releaseDoc))

			require.Error(t, err)
			assert.Contains(t, err.Error(), `machine image "img-1" not found`)
			assert.Contains(t, err.Error(), "nkectl create machineimage")
			assert.Equal(t, []string{"get MachineImage/img-1"}, f.calls, "nothing is created")
		})
	}
}

func TestCreateMachineImageChecksNoReference(t *testing.T) {
	t.Parallel()
	f := newFakeClient()

	_, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, "",
		"machineimage", "-f", writeFile(t, imageDoc))

	require.NoError(t, err)
	assert.Equal(t, []string{"create MachineImage/img-1"}, f.calls)
}

func TestCreateReleaseReportsAFailedImageLookup(t *testing.T) {
	t.Parallel()
	f := newFakeClient()
	f.failWith = operator.ErrForbidden

	_, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, "",
		"nodepoolrelease", "-f", writeFile(t, releaseDoc))

	require.Error(t, err)
	assert.ErrorIs(t, err, operator.ErrForbidden)
	assert.Contains(t, err.Error(), `checking machine image "img-1"`)
	assert.Equal(t, []string{"get MachineImage/img-1"}, f.calls, "nothing is created")
}

func TestCreateOutputJSONIsThePortableShape(t *testing.T) {
	t.Parallel()
	f := newFakeClient(image("img-1"))

	out, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, "",
		"clusterrelease", "-f", writeFile(t, releaseDoc), "-o", "json")

	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.ElementsMatch(t, []string{"metadata", "spec"}, keys(got))
}

func TestCreateDryRunClientPrintsAManifestAndNeedsNoSite(t *testing.T) {
	t.Parallel()

	clientFn := func() (operator.Client, error) {
		t.Error("a client-side dry run must not build a client or look up a site")
		return nil, fmt.Errorf("unreachable")
	}

	out, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(clientFn) }, "",
		"machineimage", "-f", writeFile(t, imageDoc), "--dry-run=client")

	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(out), &got), "defaults to YAML")
	assert.Equal(t, "internal.nke.nvidia.com/v1", got["apiVersion"])
	assert.Equal(t, "MachineImage", got["kind"])
	assert.Equal(t, map[string]any{"name": "img-1"}, got["metadata"], "no GitOps annotations: that is not nkectl's concern")
	assert.Contains(t, out, "ipxeScript: |", "multi-line scripts stay readable")
	assert.NotContains(t, out, "annotations")
	assert.NotContains(t, out, "argocd")
}

func TestCreateDryRunClientJSON(t *testing.T) {
	t.Parallel()

	out, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(nil) }, "",
		"nodepoolrelease", "-f", writeFile(t, releaseDoc), "--dry-run=client", "-o", "json")

	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "NodePoolRelease", got["kind"])
}

func TestCreateDryRunStillRejectsABadDocument(t *testing.T) {
	t.Parallel()

	_, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(nil) }, "",
		"machineimage", "-f", writeFile(t, "metadata:\n  name: a\nspec:\n  arch: amd64\n"), "--dry-run=client")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown field "arch"`)
}

func TestCreateInvalidFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"dry-run value", []string{"machineimage", "-f", "-", "--dry-run=server"}, `invalid --dry-run value "server"`},
		{"output format", []string{"machineimage", "-f", "-", "-o", "xml"}, `unknown output format "xml"`},
		{"missing file flag", []string{"machineimage"}, `required flag(s) "filename" not set`},
		{"file that does not exist", []string{"machineimage", "-f", "/nonexistent/request.yaml"}, "opening"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient()

			_, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, imageDoc, tt.args...)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Empty(t, f.calls)
		})
	}
}

func TestClientErrorIsReturned(t *testing.T) {
	t.Parallel()

	clientFn := func() (operator.Client, error) { return nil, fmt.Errorf("site %q not found in config", "nope") }

	_, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(clientFn) }, "",
		"machineimage", "-f", writeFile(t, imageDoc))

	require.Error(t, err)
	assert.Contains(t, err.Error(), `site "nope" not found`)
}

func TestGetTableMachineImages(t *testing.T) {
	t.Parallel()
	f := newFakeClient(image("b-img"), image("a-img"))

	out, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "", "machineimages")

	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, []string{"NAME", "OS", "VERSION", "ARCH", "VARIANT", "BUILT", "AGE"}, strings.Fields(lines[0]))
	assert.Equal(t, []string{"a-img", "ubuntu", "24.04", "amd64", "fde", "2026-03-23", "2h"}, strings.Fields(lines[1]))
	assert.Equal(t, "b-img", strings.Fields(lines[2])[0])
}

func TestGetTableReleases(t *testing.T) {
	t.Parallel()
	f := newFakeClient(
		release(operator.KindClusterRelease, "cp-v1", "img-1", false),
		release(operator.KindClusterRelease, "cp-v0", "img-0", true),
		release(operator.KindNodePoolRelease, "w-v1", "img-1", false),
	)

	out, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "", "clusterreleases")

	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3, "only cluster releases")
	assert.Equal(t, []string{"NAME", "IMAGE", "DEPRECATED", "AGE"}, strings.Fields(lines[0]))
	assert.Equal(t, []string{"cp-v0", "img-0", "true", "1d"}, strings.Fields(lines[1]))
	assert.Equal(t, []string{"cp-v1", "img-1", "false", "1d"}, strings.Fields(lines[2]))
}

func TestGetSingularAliasWorks(t *testing.T) {
	t.Parallel()
	f := newFakeClient(release(operator.KindNodePoolRelease, "w-v1", "img-1", false))

	out, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "", "nodepoolrelease")

	require.NoError(t, err)
	assert.Contains(t, out, "w-v1")
}

func TestGetNamedResourcesKeepGivenOrderAndReportMissing(t *testing.T) {
	t.Parallel()
	f := newFakeClient(image("a-img"), image("b-img"))

	out, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "",
		"machineimages", "b-img", "missing", "a-img")

	require.Error(t, err)
	assert.EqualError(t, err, `machine image "missing" not found`)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, "b-img", strings.Fields(lines[1])[0])
	assert.Equal(t, "a-img", strings.Fields(lines[2])[0])
}

func TestGetOneNamePrintsOneObject(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient(
				release(operator.KindClusterRelease, "cp-v1", "img-1", false),
				release(operator.KindClusterRelease, "cp-v2", "img-2", false),
			)

			out, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "",
				"clusterreleases", "cp-v1", "-o", format)

			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(out), &got), "one name is one object, not a list")
			assert.ElementsMatch(t, []string{"metadata", "spec"}, keys(got), "no apiVersion, kind, uid or resourceVersion")
			assert.Equal(t, map[string]any{"name": "cp-v1"}, got["metadata"])
			assert.NotContains(t, out, "creationTimestamp")
		})
	}
}

func TestGetNoNameOrSeveralNamesPrintAList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"no name", []string{"clusterreleases"}, []string{"cp-v1", "cp-v2"}},
		{"two names in the given order", []string{"clusterreleases", "cp-v2", "cp-v1"}, []string{"cp-v2", "cp-v1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient(
				release(operator.KindClusterRelease, "cp-v1", "img-1", false),
				release(operator.KindClusterRelease, "cp-v2", "img-2", false),
			)

			out, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "",
				append(tt.args, "-o", "yaml")...)

			require.NoError(t, err)
			var got []map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(out), &got), "no name or several names are a list")
			names := make([]string, 0, len(got))
			for _, item := range got {
				assert.ElementsMatch(t, []string{"metadata", "spec"}, keys(item))
				names = append(names, item["metadata"].(map[string]any)["name"].(string))
			}
			assert.Equal(t, tt.want, names)
		})
	}
}

func TestGetOneMissingNamePrintsNothingAndFails(t *testing.T) {
	t.Parallel()
	f := newFakeClient()

	out, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "",
		"clusterreleases", "nope", "-o", "json")

	assert.EqualError(t, err, `cluster release "nope" not found`)
	assert.Empty(t, out, "no [] or null is printed for a name that does not exist")
}

// TestGetOneNameOutputCreatesACopy is the clone workflow: the output for one
// name, renamed, is accepted by create unchanged, because published content is
// immutable and a change is published under a new name.
func TestGetOneNameOutputCreatesACopy(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient(image("img-1"), release(operator.KindNodePoolRelease, "w-v1", "img-1", true))

			saved, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "",
				"nodepoolreleases", "w-v1", "-o", format)
			require.NoError(t, err)
			renamed := strings.Replace(saved, "w-v1", "w-v2", 1)
			if format == "json" {
				renamed = strings.Replace(saved, `"w-v1"`, `"w-v2"`, 1)
			}

			out, _, err := run(t, "create", func() []*cobra.Command { return NewCreateCmds(f.clientFn()) }, renamed,
				"nodepoolrelease", "-f", "-")

			require.NoError(t, err)
			assert.Contains(t, out, `Node pool release "w-v2" created`)
			copied, ok := f.items["NodePoolRelease/w-v2"]
			require.True(t, ok, "the copy exists under the new name")
			assert.Equal(t, f.items["NodePoolRelease/w-v1"].Spec, copied.Spec, "the copy has the same content")
		})
	}
}

func TestGetEmptyJSONIsAnEmptyList(t *testing.T) {
	t.Parallel()
	f := newFakeClient()

	out, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "", "machineimages", "-o", "json")

	require.NoError(t, err)
	assert.JSONEq(t, "[]", out)
}

func TestGetInvalidOutputFormat(t *testing.T) {
	t.Parallel()
	f := newFakeClient()

	_, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "", "machineimages", "-o", "xml")

	require.Error(t, err)
	assert.Empty(t, f.calls)
}

func TestGetListErrorIsReturned(t *testing.T) {
	t.Parallel()
	f := newFakeClient()
	f.failWith = operator.ErrCRDsNotInstalled

	_, _, err := run(t, "get", func() []*cobra.Command { return NewGetCmds(f.clientFn()) }, "", "clusterreleases")

	assert.ErrorIs(t, err, operator.ErrCRDsNotInstalled)
}

func TestDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		verb     string
		item     fakeItem
		wantOut  string
		wantNote string
	}{
		{
			"machine image", "machineimage", image("img-1"),
			"Machine image \"img-1\" deleted\n",
			"does not check whether a cluster release or node pool release still names this machine image",
		},
		{
			"cluster release", "clusterrelease", release(operator.KindClusterRelease, "c-v1", "img-1", false),
			"Cluster release \"c-v1\" deleted\n",
			"does not check whether a cluster still selects this cluster release",
		},
		{
			"node pool release", "nodepoolrelease", release(operator.KindNodePoolRelease, "w-v1", "img-1", false),
			"Node pool release \"w-v1\" deleted\n",
			"does not check whether a node pool still selects this node pool release",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient(tt.item)

			out, errOut, err := run(t, "delete", func() []*cobra.Command { return NewDeleteCmds(f.clientFn()) }, "",
				tt.verb, tt.item.res.Metadata.Name)

			require.NoError(t, err)
			assert.Equal(t, tt.wantOut, out)
			assert.Contains(t, errOut, tt.wantNote)
			assert.Empty(t, f.items)
		})
	}
}

func TestDeleteHelpOnlySuggestsDeprecateForReleases(t *testing.T) {
	t.Parallel()

	for verb, want := range map[string]bool{"machineimage": false, "clusterrelease": true, "nodepoolrelease": true} {
		out, _, err := run(t, "delete", func() []*cobra.Command { return NewDeleteCmds(newFakeClient().clientFn()) }, "", verb, "--help")

		require.NoError(t, err)
		assert.Equal(t, want, strings.Contains(out, "nkectl deprecate"), verb)
	}
}

func TestDeleteMissing(t *testing.T) {
	t.Parallel()
	f := newFakeClient()

	out, errOut, err := run(t, "delete", func() []*cobra.Command { return NewDeleteCmds(f.clientFn()) }, "",
		"machineimage", "nope")

	assert.ErrorIs(t, err, operator.ErrNotFound)
	assert.Empty(t, out)
	assert.Empty(t, errOut, "no warning about a deletion that did not happen")
}

func TestDeleteNeedsExactlyOneName(t *testing.T) {
	t.Parallel()
	f := newFakeClient()

	for _, args := range [][]string{{"machineimage"}, {"machineimage", "a", "b"}} {
		_, _, err := run(t, "delete", func() []*cobra.Command { return NewDeleteCmds(f.clientFn()) }, "", args...)
		require.Error(t, err)
	}
	assert.Empty(t, f.calls)
}

func TestDeprecate(t *testing.T) {
	t.Parallel()
	f := newFakeClient(release(operator.KindClusterRelease, "cp-v1", "img-1", false))

	out, _, err := run(t, "deprecate", func() []*cobra.Command { return []*cobra.Command{NewDeprecateCmd(f.clientFn())} }, "",
		"deprecate", "clusterrelease", "cp-v1")

	require.NoError(t, err)
	assert.Equal(t, "Cluster release \"cp-v1\" deprecated\n", out)
	assert.Equal(t, []string{"deprecate ClusterRelease/cp-v1 true"}, f.calls)
}

func TestDeprecateHasNoMachineImageCommand(t *testing.T) {
	t.Parallel()
	f := newFakeClient(image("img-1"))

	_, _, err := run(t, "deprecate", func() []*cobra.Command { return []*cobra.Command{NewDeprecateCmd(f.clientFn())} }, "",
		"deprecate", "machineimage", "img-1")

	require.Error(t, err)
	assert.Empty(t, f.calls)
}

func TestDeprecateMissingRelease(t *testing.T) {
	t.Parallel()
	f := newFakeClient()

	_, _, err := run(t, "deprecate", func() []*cobra.Command { return []*cobra.Command{NewDeprecateCmd(f.clientFn())} }, "",
		"deprecate", "nodepoolrelease", "nope")

	assert.ErrorIs(t, err, operator.ErrNotFound)
}

func TestHelpNeedsNoSite(t *testing.T) {
	t.Parallel()

	clientFn := func() (operator.Client, error) {
		t.Error("--help must not look up a site")
		return nil, fmt.Errorf("unreachable")
	}
	builds := map[string]func() []*cobra.Command{
		"create": func() []*cobra.Command { return NewCreateCmds(clientFn) },
		"get":    func() []*cobra.Command { return NewGetCmds(clientFn) },
		"delete": func() []*cobra.Command { return NewDeleteCmds(clientFn) },
	}
	for verb, build := range builds {
		out, _, err := run(t, verb, build, "", "machineimage", "--help")
		if verb == "get" {
			out, _, err = run(t, verb, build, "", "machineimages", "--help")
		}
		require.NoError(t, err, verb)
		assert.NotEmpty(t, out, verb)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
