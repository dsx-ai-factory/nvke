# nkectl update cluster Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `nkectl update cluster <name> [--set key=value ...] [-f patch.yaml]`, letting operators PATCH a running tenant cluster without deleting/recreating it.

**Architecture:** Follow the existing `cli/internal/cwe/clusters` package pattern used by `create`/`get`/`describe`/`delete`. Add an `UpdateCluster` method to `CWEClient` that PATCHes `/clusters/{id}`. Add a new `clusters.NewUpdateCmd` cobra command in a new `update.go` file (the `--set`/`-f` merge logic is substantial enough to warrant its own file rather than growing `clusters.go` further). `--set` is resolved through a static, testable `map[string]setter` table keyed by dot-notation path — not reflection/mapstructure — matching the codebase's existing preference for explicit, typed code over generic magic (see `sitespec`'s `setScalar`/`checkSettable`). `-f` loads a YAML patch via the existing `util.YAMLToRequest` helper; `--set` entries are applied afterward, so they naturally override values from `-f`.

**Tech Stack:** Go, cobra, the oapi-codegen–generated `github.com/NVIDIA/nke/controller/api` client, `github.com/google/uuid`, `gopkg.in/yaml.v3`, `testify` (existing dep, though this package's tests use plain `testing` — match the file you're in).

**Spec:** `/Users/anishagupta/dev/nvcloud/nke/knowledge-base/working-docs/design-update-cluster-command.md` (Option C, confirmed). **Correction to the spec:** the design doc's flat `controlPlaneConfig.<field>` key list is stale. The actual `UpdateClusterRequest`/`UpdateClusterControlPlaneConfig` schema (`controller/api/models.gen.go:1030-1095`, `controller/api/openapi.yaml:878-1040`) nests provider-specific fields under `controlPlaneConfig.nico.*` (NICo) or `controlPlaneConfig.infra.*` (CAPI infra provider) — mutually exclusive, API-enforced. This plan's `--set` key table below is the corrected, current one; update the design doc separately if desired.

## Global Constraints

- Errors: wrap with `fmt.Errorf("...: %w", err)` — no `github.com/pkg/errors` (`AGENTS.md`).
- Logging: none needed in this feature (no background/async work in the CLI process itself).
- Tests: table-driven, colocated `*_test.go`, run against a real `httptest.Server` fake (`fakeCWE` in `cli/internal/cwe/clusters/fake_cwe_test.go`) — do not add a mocked/interface double for the generated client (matches existing `create`/`delete`/`get` tests).
- Doc comments: exported declarations start with the identifier name; no design rationale or PR context in comments — link to the spec doc instead (`cli/AGENTS.md`).
- `FIXME:` only, never `TODO:`, no internal ticket IDs anywhere in this repo (public repo — see root `CLAUDE.md`).
- License header on every new file — copy the header from `cli/internal/cwe/clusters/clusters.go:1-2`.
- Run `make fmt && make lint && make test` from `cli/` before considering any task done; run `make preflight` from the repo root before opening a PR.
- Commits follow Conventional Commits; branch is already `feat/nkectl-update-cluster`.

## Corrected `--set` key reference

| `--set` key | Go field | Type / syntax |
| :---- | :---- | :---- |
| `controlPlaneKubernetesVersion` | `UpdateClusterRequest.ControlPlaneKubernetesVersion` | string, e.g. `1.31.0` |
| `enableCPNodeRecycling` | `.EnableCPNodeRecycling` | bool: `true`/`false` |
| `teleportEnabled` | `.TeleportEnabled` | bool |
| `enableEtcdEncryption` | `.EnableEtcdEncryption` | bool, `true` only (API rejects `false`) |
| `enableOidc` | `.EnableOidc` | bool, `true` only (API rejects `false`) |
| `controlPlaneConfig.desiredCount` | `.ControlPlaneConfig.DesiredCount` | int, e.g. `3` |
| `controlPlaneConfig.maxSurge` | `.ControlPlaneConfig.MaxSurge` | int |
| `controlPlaneConfig.systemReservedMemory` | `.ControlPlaneConfig.SystemReservedMemory` | string, e.g. `16Gi` |
| `controlPlaneConfig.provisioningMonitorDisabled` | `.ControlPlaneConfig.ProvisioningMonitorDisabled` | bool |
| `controlPlaneConfig.teleportEnabled` | `.ControlPlaneConfig.TeleportEnabled` | bool |
| `controlPlaneConfig.nodeLabels` | `.ControlPlaneConfig.NodeLabels` | `[]string`, comma-separated, replaces list |
| `controlPlaneConfig.nico.configProfile` | `.ControlPlaneConfig.Nico.ConfigProfile` | string |
| `controlPlaneConfig.nico.osProfile` | `.ControlPlaneConfig.Nico.OSProfile` | string |
| `controlPlaneConfig.nico.instanceTypePreference` | `.ControlPlaneConfig.Nico.InstanceTypePreference` | `[]uuid`, comma-separated, replaces list |
| `controlPlaneConfig.nico.sshKeyGroupIDs` | `.ControlPlaneConfig.Nico.SSHKeyGroupIDs` | `[]uuid`, comma-separated |
| `controlPlaneConfig.nico.subnetIDs` | `.ControlPlaneConfig.Nico.SubnetIDs` | `[]uuid`, comma-separated |
| `controlPlaneConfig.nico.vpcPrefixIDs` | `.ControlPlaneConfig.Nico.VpcPrefixIDs` | `[]uuid`, comma-separated |
| `controlPlaneConfig.infra.preKubeadmCommands` | `.ControlPlaneConfig.Infra.PreKubeadmCommands` | `[]string`, comma-separated |
| `controlPlaneConfig.infra.spireAttestationMethod` | `.ControlPlaneConfig.Infra.SpireAttestationMethod` | string enum: `joinToken` or `tpm` |

`controlPlaneConfig.infra.bootstrapFiles` and `controlPlaneConfig.infra.machineTemplate` are structured (a list of file objects / an arbitrary JSON manifest) and are **not** `--set`-able — they require `-f`. `network` is immutable and intentionally has no `--set` key or field-setter; a user who puts it in a `-f` patch file gets the API's 400 verbatim, per the design doc.

---

## File Structure

- Modify: `cli/internal/clients/clusters.go` — add `CWEClient.UpdateCluster`.
- Modify: `cli/internal/cwe/clusters/fake_cwe_test.go` — add a PATCH route to the shared test fake so both the client test and the command tests can use it.
- Create: `cli/internal/cwe/clusters/update.go` — `NewUpdateCmd`, `--set` key table, patch-file loading, merge logic.
- Create: `cli/internal/cwe/clusters/update_test.go` — unit tests for the `--set` parser and end-to-end command tests.
- Create: `cli/cmd/nkectl/update.go` — `newUpdateCmd()`, mirrors `create.go`.
- Modify: `cli/cmd/nkectl/root.go` — register `newUpdateCmd()`.
- Modify: `cli/README.md` — add `update` to the command list and an example.

---

### Task 1: `CWEClient.UpdateCluster` + fake PATCH route

**Files:**
- Modify: `cli/internal/clients/clusters.go`
- Modify: `cli/internal/cwe/clusters/fake_cwe_test.go`
- Test: `cli/internal/clients/clusters_test.go` (new file in that package — check it doesn't already exist under a different name first: `ls cli/internal/clients/*_test.go`)

**Interfaces:**
- Produces: `func (c *CWEClient) UpdateCluster(ctx context.Context, id openapi_types.UUID, req api.UpdateClusterJSONRequestBody) (*api.Cluster, error)` — later tasks call this.
- Produces (test fake): `fakeCWE` now answers `PATCH /clusters/{id}`, decoding the body into `api.UpdateClusterJSONRequestBody`, applying only the non-nil fields it understands (`ControlPlaneKubernetesVersion`, `EnableCPNodeRecycling`, `TeleportEnabled`, `EnableEtcdEncryption`, `EnableOidc`) onto the stored `api.Cluster`, and returning the updated record with 200. Unknown-cluster-id still returns the existing `api.GenericError` 404 shape.

- [ ] **Step 1: Write the failing client test**

Check first whether `cli/internal/clients/` already has test infrastructure:

```bash
ls cli/internal/clients/*_test.go
```

If none exists, create `cli/internal/clients/clusters_test.go`:

```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/cli/internal/config"
	api "github.com/NVIDIA/nke/controller/api"
)

func TestUpdateClusterSendsPatchAndReturnsTheUpdatedCluster(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/clusters/"+id.String() {
			t.Errorf("request = %s %s, want PATCH /api/clusters/%s", r.Method, r.URL.Path, id)
		}
		var got api.UpdateClusterJSONRequestBody
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if got.ControlPlaneKubernetesVersion == nil || *got.ControlPlaneKubernetesVersion != "1.31.0" {
			t.Errorf("controlPlaneKubernetesVersion = %v, want 1.31.0", got.ControlPlaneKubernetesVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(api.Cluster{ID: id, Name: "updated-cluster"})
	}))
	defer srv.Close()

	c, err := New(config.SiteConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	version := "1.31.0"
	got, err := c.UpdateCluster(context.Background(), id, api.UpdateClusterJSONRequestBody{
		ControlPlaneKubernetesVersion: &version,
	})
	if err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	if got.Name != "updated-cluster" {
		t.Errorf("returned cluster name = %q, want updated-cluster", got.Name)
	}
}

func TestUpdateClusterSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"controlPlaneKubernetesVersion must be greater than the current version"}`))
	}))
	defer srv.Close()

	c, err := New(config.SiteConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	_, err = c.UpdateCluster(context.Background(), uuid.New(), api.UpdateClusterJSONRequestBody{})
	if err == nil {
		t.Fatal("UpdateCluster against a 400 server returned no error")
	}
	if got := err.Error(); got == "" {
		t.Fatal("error message is empty")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cli && go test ./internal/clients/... -run TestUpdateCluster -v`
Expected: FAIL — `c.UpdateCluster undefined (type *CWEClient has no field or method UpdateCluster)`

- [ ] **Step 3: Implement `UpdateCluster`**

Add to `cli/internal/clients/clusters.go`, after `GetCluster` (after line 59):

```go
// UpdateCluster applies a partial update to a cluster identified by UUID and
// returns the cluster as it now stands. Only the fields set in req are
// changed; nil fields are left untouched by the server.
func (c *CWEClient) UpdateCluster(ctx context.Context, id openapi_types.UUID, req api.UpdateClusterJSONRequestBody) (*api.Cluster, error) {
	resp, err := c.raw.UpdateClusterWithResponse(ctx, id, req)
	if err != nil {
		return nil, fmt.Errorf("updating cluster: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("updating cluster: %w", apiError(resp.Status(), resp.Body))
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("updating cluster: empty response body")
	}
	return resp.JSON200, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd cli && go test ./internal/clients/... -run TestUpdateCluster -v`
Expected: PASS

- [ ] **Step 5: Add the PATCH route to the shared fake used by command-level tests**

`cli/internal/cwe/clusters/fake_cwe_test.go` is shared by every command test in the package (Task 4's tests need it too). Add a case to `ServeHTTP` (after the `isItem && r.Method == http.MethodDelete` case around line 106):

```go
	case isItem && r.Method == http.MethodPatch:
		f.update(w, id, r)
```

Add the handler after `remove` (after line 173):

```go
// update answers PATCH /clusters/{id}, applying the subset of fields this
// fake understands and returning the updated record. It exists to exercise
// the CLI's request-building and response-decoding, not CWE's real merge
// semantics — keep it to the fields the update-cluster tests actually cover.
func (f *fakeCWE) update(w http.ResponseWriter, id string, r *http.Request) {
	for i, c := range f.clusters {
		if c.ID.String() != id {
			continue
		}
		var req api.UpdateClusterJSONRequestBody
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, api.GenericError{Message: "malformed body: " + err.Error()})
			return
		}
		if req.ControlPlaneKubernetesVersion != nil {
			c.ControlPlaneKubernetesVersion = *req.ControlPlaneKubernetesVersion
		}
		if req.EnableCPNodeRecycling != nil {
			c.EnableCPNodeRecycling = req.EnableCPNodeRecycling
		}
		if req.TeleportEnabled != nil {
			c.TeleportEnabled = req.TeleportEnabled
		}
		if req.EnableEtcdEncryption != nil {
			c.EtcdEncrypted = *req.EnableEtcdEncryption
		}
		f.clusters[i] = c
		writeJSON(w, http.StatusOK, c)
		return
	}
	writeJSON(w, http.StatusNotFound, api.GenericError{Message: "cluster " + id + " not found"})
}
```

`api.Cluster`'s field names and types were confirmed by reading `controller/api/models.gen.go:80-107` while writing this plan: `EnableCPNodeRecycling *bool`, `TeleportEnabled *bool` (both pointers, assign the request's pointer directly — do not dereference into a copy), `EtcdEncrypted bool` (not a pointer — dereference `req.EnableEtcdEncryption` as shown above). If a future task touches this fake again, re-check the struct rather than assuming these snippets still match, since `models.gen.go` is regenerated output.

- [ ] **Step 6: Run the full clients and clusters package tests**

Run: `cd cli && go test ./internal/clients/... ./internal/cwe/clusters/... -v`
Expected: PASS (existing `create`/`get`/`describe`/`delete` tests still pass — the fake's routing table grew but no existing case changed).

- [ ] **Step 7: Commit**

```bash
git add cli/internal/clients/clusters.go cli/internal/clients/clusters_test.go cli/internal/cwe/clusters/fake_cwe_test.go
git commit -m "feat(cli): add CWEClient.UpdateCluster and a fake PATCH route"
```

---

### Task 2: `--set` key parser

**Files:**
- Create: `cli/internal/cwe/clusters/update.go` (this task writes only the parser; Task 3 adds the cobra command in the same file)
- Test: `cli/internal/cwe/clusters/update_test.go`

**Interfaces:**
- Consumes: `api.UpdateClusterRequest` (from `controller/api/models.gen.go`, already imported as `api` elsewhere in this package), `gopkg.in/yaml.v3` (already a dependency, used by `util.YAMLToRequest` for `-f`).
- Produces: `func applySets(req *api.UpdateClusterRequest, entries []string) error` — merges `--set key=value` entries onto `req`. Task 3 calls this once with the full `--set` slice, after `-f` (if any) has already been decoded onto the same `req`.

**Note — this section was revised from the original plan during implementation.** The plan originally called for a hand-written `map[string]setter` table with one closure per field (`cpConfig`/`nico`/`infra` pointer-chasing helpers, a `setBool` helper, explicit `strconv.Atoi`/`ParseBool` calls per field). Code review during Task 2 flagged that this defeated Option C's core stated benefit — "new mutable fields require no CLI change" — since every new API field would still need a new Go closure here. The implementation below instead assembles `--set` entries into a nested JSON document (the same shape a `-f` patch file decodes from) and decodes it onto `req` with `json.Decoder.DisallowUnknownFields()`, exactly like `loadCreateClusterRequest` does for `-f`. The generated struct's own json tags — not a hand-maintained table — decide which keys are valid and what type each value must be. The only remaining per-field knowledge is `arrayKeys`, a set of dot-paths whose value is a list rather than a scalar (needed because a single value with no comma is otherwise indistinguishable from a scalar).

- [ ] **Step 1: Write the failing tests**

Use the test file exactly as it was written and committed in Task 2 — see `cli/internal/cwe/clusters/update_test.go` for the full set of `TestApplySet*` cases (scalar fields, `controlPlaneConfig` scalars, `nico`/`infra` fields, unknown key, missing `=`, invalid bool/int/UUID, later-entry-wins). No changes were needed to the tests themselves; only the implementation they exercise changed.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd cli && go test ./internal/cwe/clusters/... -run TestApplySet -v`
Expected: FAIL — `undefined: applySets` (compile error)

- [ ] **Step 3: Implement the parser**

Create `cli/internal/cwe/clusters/update.go`:

```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	api "github.com/NVIDIA/nke/controller/api"
)

// arrayKeys is the set of --set paths whose value is a comma-separated list
// rather than a scalar, so a single value with no comma is still wrapped as
// a one-element array. This is the only per-field knowledge applySets needs:
// value *types* (string, bool, int, []uuid.UUID, ...) come entirely from the
// generated UpdateClusterRequest struct's own json tags, enforced by the
// json.Decoder in applySets — not hand-listed here. A comma-count heuristic
// alone can't tell a single-element list from a scalar, hence this table.
var arrayKeys = map[string]bool{ //nolint:gochecknoglobals // static lookup table, not mutated after init
	"controlPlaneConfig.nodeLabels":                  true,
	"controlPlaneConfig.nico.instanceTypePreference": true,
	"controlPlaneConfig.nico.sshKeyGroupIDs":         true,
	"controlPlaneConfig.nico.subnetIDs":              true,
	"controlPlaneConfig.nico.vpcPrefixIDs":           true,
	"controlPlaneConfig.infra.preKubeadmCommands":    true,
}

// applySets merges --set entries onto req. Each key is dot notation
// matching UpdateClusterRequest's json field names (e.g.
// "controlPlaneConfig.nico.subnetIDs"). Entries are assembled into a nested
// JSON document — the same shape a -f patch file would decode from — and
// applied with the same json.Decoder path loadCreateClusterRequest uses for
// -f, so the generated struct's json tags decide which keys are valid and
// what type each value must parse as. Unknown keys or values that don't fit
// their field's type surface as the decoder's own errors.
//
// Decoding directly onto req (rather than into a fresh struct) merges into
// whatever -f already populated: encoding/json only overwrites fields
// present in the JSON document, so a key --set doesn't mention keeps its -f
// value, and a key both set and -f'd takes the --set value, since this runs
// after -f is decoded.
func applySets(req *api.UpdateClusterRequest, entries []string) error {
	if len(entries) == 0 {
		return nil
	}

	patch := map[string]interface{}{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return fmt.Errorf("--set %q: expected key=value", entry)
		}
		parsed, err := parseSetValue(key, value)
		if err != nil {
			return fmt.Errorf("--set %q: %w", entry, err)
		}
		if err := setPath(patch, strings.Split(key, "."), parsed); err != nil {
			return fmt.Errorf("--set %q: %w", entry, err)
		}
	}

	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("encoding --set values: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		return fmt.Errorf("applying --set: %w", err)
	}
	return nil
}

// setPath writes value into the nested map at the dot-separated path,
// creating intermediate maps as needed.
func setPath(m map[string]interface{}, path []string, value interface{}) error {
	if path[0] == "" {
		return fmt.Errorf("empty key")
	}
	if len(path) == 1 {
		m[path[0]] = value
		return nil
	}
	next, ok := m[path[0]].(map[string]interface{})
	if !ok {
		next = map[string]interface{}{}
		m[path[0]] = next
	}
	return setPath(next, path[1:], value)
}

// parseSetValue coerces a --set value's raw text into the JSON type it most
// likely represents. Keys in arrayKeys always become an array, splitting on
// comma (even a single value with no comma becomes a one-element array,
// since a bare comma count can't distinguish a scalar from a single-element
// list). Otherwise the whole value is parsed as one YAML scalar — the same
// mechanism a -f patch file's values go through — so "true"/"false" become
// booleans, bare integers become numbers, and everything else stays a
// string, without hand-listing which spellings mean which type here.
func parseSetValue(key, raw string) (interface{}, error) {
	if arrayKeys[key] {
		parts := strings.Split(raw, ",")
		out := make([]interface{}, len(parts))
		for i, p := range parts {
			v, err := parseScalar(p)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	return parseScalar(raw)
}

// parseScalar parses one YAML scalar, e.g. "true", "3", "16Gi", or a UUID
// string, into the Go value it represents (bool, int, float64, or string).
func parseScalar(raw string) (interface{}, error) {
	var v interface{}
	if err := yaml.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err != nil {
		return nil, fmt.Errorf("parsing %q: %w", raw, err)
	}
	return v, nil
}
```

Note: `yaml.v3`'s scalar resolver also treats `on`/`off`/`yes`/`no` (any case) as booleans, per YAML 1.1 semantics — the same resolver `-f` patch files already go through via `util.YAMLToRequest`. None of this schema's string-typed `--set` keys (profile names, the `spireAttestationMethod` enum) collide with those spellings in practice.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd cli && go test ./internal/cwe/clusters/... -run TestApplySet -v`
Expected: PASS for all `TestApplySet*` cases.

- [ ] **Step 5: Run `go vet` and `gofmt`**

Run: `cd cli && go vet ./internal/cwe/clusters/... && gofmt -l internal/cwe/clusters/update.go`
Expected: no output from either. If `gofmt` lists the file, run `gofmt -w internal/cwe/clusters/update.go` (map literal alignment is the usual culprit after editing a map like `arrayKeys`).

- [ ] **Step 6: Commit**

```bash
git add cli/internal/cwe/clusters/update.go cli/internal/cwe/clusters/update_test.go
git commit -m "feat(cli): add --set key parser for nkectl update cluster"
```

---

### Task 3: `-f` patch file loading + cobra command

**Files:**
- Modify: `cli/internal/cwe/clusters/update.go` (add to the file created in Task 2)
- Modify: `cli/internal/cwe/clusters/update_test.go` (add command-level tests)

**Interfaces:**
- Consumes: `applySets` (Task 2), `util.YAMLToRequest[T]` (`cli/internal/util/convert.go:20`), `ClientFn` (`clusters.go:25`), `CWEClient.UpdateCluster` (Task 1), `CWEClient.FindClusterByName` (`cli/internal/clients/clusters.go:79`).
- Produces: `func NewUpdateCmd(clientFn ClientFn) *cobra.Command` — registered by Task 4/5's `cli/cmd/nkectl/update.go`.

- [ ] **Step 1: Write the failing command tests**

Append to `cli/internal/cwe/clusters/update_test.go`:

```go
func TestUpdateClusterSetSendsThePatchAndReportsSuccess(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	out, err := run(t, NewUpdateCmd(clientFn), "my-cluster",
		"--set", "controlPlaneKubernetesVersion=1.31.0",
		"--set", "enableCPNodeRecycling=true",
	)
	if err != nil {
		t.Fatalf("update cluster: %v", err)
	}

	want := []string{
		"GET /api/clusters?name=my-cluster",
		"PATCH /api/clusters/" + seed.ID.String(),
	}
	seen := fake.seen()
	if len(seen) != len(want) {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, seen[i], want[i])
		}
	}

	stored := fake.stored()
	if len(stored) != 1 || stored[0].ControlPlaneKubernetesVersion != "1.31.0" {
		t.Fatalf("stored cluster = %+v, want controlPlaneKubernetesVersion 1.31.0", stored)
	}

	if !strings.Contains(out, `"my-cluster" updated`) {
		t.Errorf("output does not confirm the update:\n%s", out)
	}
	if !strings.Contains(out, seed.ID.String()) {
		t.Errorf("output omits the cluster id:\n%s", out)
	}
}

func TestUpdateClusterFileLoadsThePatch(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	cmd := NewUpdateCmd(clientFn)
	cmd.SetIn(strings.NewReader(`controlPlaneKubernetesVersion: "1.31.0"`))
	_, err := run(t, cmd, "my-cluster", "-f", "-")
	if err != nil {
		t.Fatalf("update cluster -f -: %v", err)
	}

	stored := fake.stored()
	if len(stored) != 1 || stored[0].ControlPlaneKubernetesVersion != "1.31.0" {
		t.Fatalf("stored cluster = %+v, want controlPlaneKubernetesVersion 1.31.0", stored)
	}
}

func TestUpdateClusterSetOverridesFile(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	cmd := NewUpdateCmd(clientFn)
	cmd.SetIn(strings.NewReader(`controlPlaneKubernetesVersion: "1.30.5"`))
	_, err := run(t, cmd, "my-cluster", "-f", "-", "--set", "controlPlaneKubernetesVersion=1.31.0")
	if err != nil {
		t.Fatalf("update cluster: %v", err)
	}

	stored := fake.stored()
	if len(stored) != 1 || stored[0].ControlPlaneKubernetesVersion != "1.31.0" {
		t.Fatalf("stored cluster = %+v, want --set to win with 1.31.0", stored)
	}
}

func TestUpdateClusterRejectsUnknownClusterName(t *testing.T) {
	_, clientFn := newFakeCWE(t)

	_, err := run(t, NewUpdateCmd(clientFn), "ghost", "--set", "teleportEnabled=true")
	if err == nil {
		t.Fatal("updating an unknown cluster returned no error")
	}
	if !strings.Contains(err.Error(), `cluster "ghost" not found`) {
		t.Errorf("error = %v, want a not-found error naming the cluster", err)
	}
}

func TestUpdateClusterRejectsBadSetEntry(t *testing.T) {
	seed := seedCluster("my-cluster")
	_, clientFn := newFakeCWE(t, seed)

	_, err := run(t, NewUpdateCmd(clientFn), "my-cluster", "--set", "notAField=x")
	if err == nil {
		t.Fatal("update accepted an unknown --set key")
	}
}

func TestUpdateClusterRequiresFileOrSet(t *testing.T) {
	seed := seedCluster("my-cluster")
	_, clientFn := newFakeCWE(t, seed)

	_, err := run(t, NewUpdateCmd(clientFn), "my-cluster")
	if err == nil {
		t.Fatal("update with neither -f nor --set returned no error")
	}
}

func TestUpdateClusterSurfacesServerRejection(t *testing.T) {
	// newBrokenCWE always answers with the same status/body regardless of
	// path, so this exercises the client/error-surfacing path, not a
	// specific fakeCWE route. A seeded fakeCWE is not needed here.
	broken := newBrokenCWE(t, 409, `{"message":"k8s_upgrade is already in progress"}`)
	_, err := run(t, NewUpdateCmd(broken), "my-cluster", "--set", "teleportEnabled=true")
	if err == nil {
		t.Fatal("update against a 409 server returned no error")
	}
	if !strings.Contains(err.Error(), "k8s_upgrade is already in progress") {
		t.Errorf("error = %v, want it to carry the server's message", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd cli && go test ./internal/cwe/clusters/... -run TestUpdateCluster -v`
Expected: FAIL — `undefined: NewUpdateCmd` (compile error)

- [ ] **Step 3: Implement `loadUpdateClusterRequest` and `NewUpdateCmd`**

Append to `cli/internal/cwe/clusters/update.go`:

```go
// NewUpdateCmd returns the "nkectl update cluster <name>" subcommand.
func NewUpdateCmd(clientFn ClientFn) *cobra.Command {
	var (
		filename string
		sets     []string
	)

	cmd := &cobra.Command{
		Use:     "cluster <name>",
		Aliases: []string{"clusters"},
		Short:   "Update a tenant cluster",
		Long: `Update a tenant cluster on the site controller.

PATCH semantics apply: only the fields you supply are changed. Use --set for
one or two fields, -f for a full patch document, or both together (--set
wins on any key present in both).

Some downstream effects (a rolling Kubernetes upgrade, control-plane node
recycling, etcd encryption enablement) run asynchronously after this command
returns.

Example — bump the Kubernetes version:
  nkectl update cluster my-cluster --set controlPlaneKubernetesVersion=1.31.0

Example — patch file:
  nkectl update cluster my-cluster -f patch.yaml

Example — file plus an inline override:
  nkectl update cluster my-cluster -f patch.yaml --set controlPlaneConfig.desiredCount=5`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if filename == "" && len(sets) == 0 {
				return fmt.Errorf("nothing to update: pass --set or -f")
			}
			req, err := loadUpdateClusterRequest(cmd, filename, sets)
			if err != nil {
				return err
			}
			c, err := clientFn()
			if err != nil {
				return err
			}
			cl, err := c.FindClusterByName(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			updated, err := c.UpdateCluster(cmd.Context(), cl.ID, req)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cluster %q updated (id: %s)\n", updated.Name, updated.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Update is async — run 'nkectl get clusters --cluster %s' to check status.\n", updated.Name)
			return nil
		},
	}

	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Path to a YAML patch file, or '-' to read from stdin")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Set a field: key=value (repeatable); dot notation for nested fields, comma-separated for lists")
	return cmd
}

// loadUpdateClusterRequest builds an UpdateClusterJSONRequestBody from an
// optional YAML patch file and a list of --set entries. When path is
// non-empty its contents are decoded first; each entry in sets is then
// applied in order, so --set overrides a same-named field loaded from path.
func loadUpdateClusterRequest(cmd *cobra.Command, path string, sets []string) (api.UpdateClusterJSONRequestBody, error) {
	var req api.UpdateClusterJSONRequestBody
	if path != "" {
		var r io.Reader
		if path == "-" {
			r = cmd.InOrStdin()
		} else {
			f, err := os.Open(path)
			if err != nil {
				return req, fmt.Errorf("opening %q: %w", path, err)
			}
			defer f.Close()
			r = f
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return req, fmt.Errorf("reading patch: %w", err)
		}
		req, err = util.YAMLToRequest[api.UpdateClusterJSONRequestBody](data)
		if err != nil {
			return req, err
		}
	}
	if err := applySets(&req, sets); err != nil {
		return req, err
	}
	return req, nil
}
```

Merge these additions into the import block at the top of `cli/internal/cwe/clusters/update.go` (Task 2 already has `bytes`, `encoding/json`, `fmt`, `strings`, `gopkg.in/yaml.v3`, and `api`) — final import block:

```go
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/nke/cli/internal/util"
	api "github.com/NVIDIA/nke/controller/api"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd cli && go test ./internal/cwe/clusters/... -v`
Expected: PASS for every test in the package, including the pre-existing `create`/`get`/`describe`/`delete` tests (Task 1's fake changes must not have broken them) and every `TestUpdateCluster*`/`TestApplySet*` case.

- [ ] **Step 5: Run lint**

Run: `cd cli && make lint`
Expected: no findings. If `--set`'s repeated invalid-YAML-empty-body edge case or cyclomatic complexity on `NewUpdateCmd`'s closure trips a lint rule, restructure (e.g., extract the `RunE` body into a standalone function) rather than adding `//nolint`.

- [ ] **Step 6: Commit**

```bash
git add cli/internal/cwe/clusters/update.go cli/internal/cwe/clusters/update_test.go
git commit -m "feat(cli): implement nkectl update cluster command"
```

---

### Task 4: Register the command and update docs

**Files:**
- Create: `cli/cmd/nkectl/update.go`
- Modify: `cli/cmd/nkectl/root.go`
- Modify: `cli/README.md`

**Interfaces:**
- Consumes: `clusters.NewUpdateCmd` (Task 3), `loadClient` (`cli/cmd/nkectl/root.go:54`).
- Produces: `nkectl update cluster <name>` reachable from the built binary — the end-to-end deliverable for this plan.

- [ ] **Step 1: Create the verb-group file**

Create `cli/cmd/nkectl/update.go`, modeled exactly on `create.go`:

```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/cwe/clusters"
)

func newUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update a resource",
	}
	cmd.AddCommand(clusters.NewUpdateCmd(loadClient))
	return cmd
}
```

- [ ] **Step 2: Register it on the root command**

In `cli/cmd/nkectl/root.go`, add one line after `root.AddCommand(newCreateCmd())` (line 42):

```go
	root.AddCommand(newCreateCmd())
	root.AddCommand(newUpdateCmd())
```

- [ ] **Step 3: Build and manually verify the command is wired up**

Run:

```bash
cd cli && go build -o /tmp/nkectl-plan-check ./cmd/nkectl && /tmp/nkectl-plan-check update cluster --help
```

Expected: cobra's help output for `update cluster`, showing the `--set` and `-f/--filename` flags and the example text from Task 3. Then:

```bash
rm /tmp/nkectl-plan-check
```

- [ ] **Step 4: Update the README**

In `cli/README.md`:
- Line 12, change:
  ```
  - **Cluster lifecycle** — `nkectl get/create/delete/describe clusters` manages tenant clusters through the CWE API
  ```
  to:
  ```
  - **Cluster lifecycle** — `nkectl get/create/update/delete/describe clusters` manages tenant clusters through the CWE API
  ```
- After line 62 (`nkectl create cluster -f <spec-file> ...`), add:
  ```
  nkectl update cluster <name> --set controlPlaneKubernetesVersion=1.31.0
  nkectl update cluster <name> -f patch.yaml   # or combine with --set; --set wins
  ```

- [ ] **Step 5: Run the full verification suite**

Run:

```bash
cd cli && make fmt && make lint && make test
```

Expected: all green. Then from the repo root:

```bash
make preflight
```

Expected: passes (this also catches anything Task 1–3 missed in other modules, e.g. if `controller/api` needed regenerating — it should not, since this plan only consumes the existing generated types).

- [ ] **Step 6: Commit**

```bash
git add cli/cmd/nkectl/update.go cli/cmd/nkectl/root.go cli/README.md
git commit -m "feat(cli): register nkectl update cluster and document it"
```

---

## Self-Review Notes

- **Spec coverage:** every mutable field in the corrected `--set` key reference table has a task-2 setter and a test. `network` (immutable) intentionally has no setter — a `-f` patch containing it round-trips through `util.YAMLToRequest` untouched and the API's 400 surfaces verbatim via `apiError`, matching the design doc's stated behavior; no extra code needed for that case; consider adding one integration-style test for it if reviewers want explicit coverage. The 400/404/409 error-surfacing behaviors are covered by `TestUpdateClusterSurfacesServerRejection` (409) and `TestUpdateClusterRejectsUnknownClusterName` (404 via `FindClusterByName`); 400 is exercised at the client layer in Task 1's `TestUpdateClusterSurfacesAPIError`. The doc's async-output message format is matched exactly in Task 3's `RunE`.
- **Placeholder scan:** no TBD/TODO markers; every step has runnable code or an exact command.
- **Type consistency:** `applySet`/`applySets`/`NewUpdateCmd`/`loadUpdateClusterRequest` names and signatures are used identically across Tasks 2–4.
- **Field names verified:** `api.Cluster`'s relevant fields were read directly (`controller/api/models.gen.go:80-107`) rather than guessed: `EnableCPNodeRecycling *bool`, `TeleportEnabled *bool`, `EtcdEncrypted bool`. Task 1 Step 5's fake handler snippet uses the confirmed names and types.
