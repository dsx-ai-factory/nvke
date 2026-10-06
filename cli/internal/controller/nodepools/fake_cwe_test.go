// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/clients"
	"github.com/dsx-ai-factory/nke/cli/internal/config"
	api "github.com/dsx-ai-factory/nke/controller/api"
)

// run executes cmd with args and returns everything it wrote.
func run(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// fakeCWE is an httptest server answering the node pool endpoints nkectl
// calls, backed by an in-memory slice.
//
// It exists so a test can run the real path end to end: cobra flags, the real
// ControllerClient, real HTTP, real JSON decoding, and the real output formatting.
type fakeCWE struct {
	mu sync.Mutex
	// nodePools is the store. clusterNames maps each pool's cluster ID to the
	// cluster name it answers the clusterName filter under, which is all this
	// fake needs of a cluster.
	nodePools    []api.NodePool
	clusterNames map[uuid.UUID]string
	requests     []string
	patches      []map[string]any
	creates      []map[string]any
	server       *httptest.Server
	// patchStatus and patchBody, when set, make every PATCH fail with them.
	// The list lookup keeps working, so a test can reach the update call.
	patchStatus int
	patchBody   string
	// clusterStatus and clusterBody, when set, fail GET /clusters only.
	clusterStatus int
	clusterBody   string
	// createStatus and createBody, when set, make every POST fail with them.
	createStatus int
	createBody   string
	// deleteStatus and deleteBody, when set, make every DELETE fail with them
	// instead of removing the pool. The list lookup keeps working, so a test
	// can reach the delete call.
	deleteStatus int
	deleteBody   string
}

// failCreateWith makes the fake reject every POST /nodePools with status and
// body, leaving the other routes answering normally.
func (f *fakeCWE) failCreateWith(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createStatus, f.createBody = status, body
}

// failClustersWith fails GET /clusters while still listing node pools.
func (f *fakeCWE) failClustersWith(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clusterStatus, f.clusterBody = status, body
}

// failPatchWith makes the fake reject every PATCH with status and body while
// still answering the list lookup normally. A fake that failed both would let
// a test pass on the lookup error and never reach the update at all.
func (f *fakeCWE) failPatchWith(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patchStatus, f.patchBody = status, body
}

// failDeleteWith makes the fake reject every DELETE with status and body
// instead of removing the pool, while still answering the list lookup
// normally. Used to pin CWE rejections (e.g. last control-plane pool)
// verbatim through the CLI.
func (f *fakeCWE) failDeleteWith(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteStatus, f.deleteBody = status, body
}

// seed is a node pool to serve, together with the name of the cluster that
// owns it. Pools sharing a cluster name are served under one cluster ID.
type seed struct {
	clusterName      string
	name             string
	desiredNodeCount int
	controlPlane     bool
	// conditions overrides the default, which marks the pool provisioned.
	conditions *api.NodePoolConditions
}

// newFakeCWE starts a fake CWE holding a node pool per seed, and returns it
// together with a ControllerClientFn wired to it.
func newFakeCWE(t *testing.T, seeds ...seed) (*fakeCWE, ControllerClientFn) {
	t.Helper()
	f := &fakeCWE{clusterNames: map[uuid.UUID]string{}}
	clusterIDs := map[string]uuid.UUID{}
	for _, s := range seeds {
		clusterID, ok := clusterIDs[s.clusterName]
		if !ok {
			clusterID = uuid.New()
			clusterIDs[s.clusterName] = clusterID
			f.clusterNames[clusterID] = s.clusterName
		}
		f.nodePools = append(f.nodePools, api.NodePool{
			ID:                uuid.New(),
			ClusterID:         clusterID,
			Name:              s.name,
			NodeNamePrefix:    s.name,
			KubernetesVersion: "1.30.0",
			DesiredNodeCount:  s.desiredNodeCount,
			NodeLabels:        map[string]string{},
			ControlPlane:      s.controlPlane,
			Conditions:        conditionsFor(s),
		})
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f, clientFnFor(f.server.URL)
}

// conditionsFor defaults an unset seed to a provisioned pool.
func conditionsFor(s seed) api.NodePoolConditions {
	if s.conditions != nil {
		return *s.conditions
	}
	return api.NodePoolConditions{Provisioned: api.Condition{Status: api.ConditionStatusTrue}}
}

// newBrokenCWE returns a ControllerClientFn for a server that answers every request with
// status and body, for the error paths.
func newBrokenCWE(t *testing.T, status int, body string) ControllerClientFn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return clientFnFor(srv.URL)
}

// clientFnFor builds a real ControllerClient against url. Nothing here is a stub: the
// client is the one the binary uses, so a change to its URL handling, status
// checks or error parsing shows up in these tests.
func clientFnFor(url string) ControllerClientFn {
	return func() (*clients.ControllerClient, error) {
		return clients.New(config.SiteConfig{URL: url})
	}
}

// seen returns the requests the CLI made, as "METHOD /path?query".
func (f *fakeCWE) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// createBodies returns the POST bodies the CLI sent, decoded as raw maps so a
// test can assert on exactly which fields went over the wire.
func (f *fakeCWE) createBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.creates...)
}

// patchBodies returns the PATCH bodies the CLI sent, decoded as raw maps so a
// test can assert on exactly which fields went over the wire rather than on
// what the typed struct would have filled in.
func (f *fakeCWE) patchBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.patches...)
}

// clusterIDFor returns the ID the fake serves the named cluster under.
func (f *fakeCWE) clusterIDFor(t *testing.T, name string) uuid.UUID {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, clusterName := range f.clusterNames {
		if clusterName == name {
			return id
		}
	}
	t.Fatalf("cluster %q not seeded", name)
	return uuid.Nil
}

// stored returns the node pools the fake currently holds.
func (f *fakeCWE) stored() []api.NodePool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.NodePool(nil), f.nodePools...)
}

func (f *fakeCWE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())

	// The client appends /api to the configured site URL. Matching on that
	// prefix rather than stripping it silently means a client that stops
	// sending it fails these tests instead of passing them.
	path, hasPrefix := strings.CutPrefix(r.URL.Path, "/api")
	if !hasPrefix {
		writeJSON(w, http.StatusNotFound,
			api.GenericError{Message: "request did not carry the /api prefix: " + r.URL.Path})
		return
	}

	switch id, isItem := strings.CutPrefix(path, "/nodePools/"); {
	case path == "/nodePools" && r.Method == http.MethodGet:
		f.list(w, r.URL.Query().Get("clusterName"), r.URL.Query().Get("name"))
	case path == "/nodePools" && r.Method == http.MethodPost:
		f.create(w, r)
	case path == "/clusters" && r.Method == http.MethodGet:
		f.listClusters(w, r.URL.Query().Get("name"))
	case isItem && r.Method == http.MethodPatch:
		f.update(w, r, id)
	case isItem && r.Method == http.MethodDelete:
		f.delete(w, id)
	default:
		writeJSON(w, http.StatusNotFound,
			api.GenericError{Message: "no route for " + r.Method + " " + path})
	}
}

// list answers GET /nodePools, applying the clusterName and name filters the
// CLI uses to resolve a pool by name.
func (f *fakeCWE) list(w http.ResponseWriter, clusterName, name string) {
	out := []api.NodePool{}
	for _, p := range f.nodePools {
		if name != "" && p.Name != name {
			continue
		}
		if clusterName != "" && f.clusterNames[p.ClusterID] != clusterName {
			continue
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, api.ListNodePoolsResponse{NodePools: out})
}

// listClusters answers GET /clusters. The name filter is honoured because
// --cluster resolves through here; ignoring it makes every name ambiguous.
func (f *fakeCWE) listClusters(w http.ResponseWriter, name string) {
	if f.clusterStatus != 0 {
		writeJSON(w, f.clusterStatus, json.RawMessage(f.clusterBody))
		return
	}
	out := []api.Cluster{}
	for id, clusterName := range f.clusterNames {
		if name != "" && clusterName != name {
			continue
		}
		out = append(out, api.Cluster{ID: id, Name: clusterName})
	}
	// Map iteration order is random, so sort to keep the response stable.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, api.ListClustersResponse{Clusters: out})
}

// update answers PATCH /nodePools/{id}, applying desiredNodeCount and echoing
// the stored pool back the way CWE does.
func (f *fakeCWE) update(w http.ResponseWriter, r *http.Request, id string) {
	var raw map[string]any
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, api.GenericError{Message: "malformed body: " + err.Error()})
		return
	}
	f.patches = append(f.patches, raw)

	// Recorded before failing, so a test can still assert what was sent.
	if f.patchStatus != 0 {
		writeJSON(w, f.patchStatus, json.RawMessage(f.patchBody))
		return
	}

	for i, p := range f.nodePools {
		if p.ID.String() != id {
			continue
		}
		if count, ok := raw["desiredNodeCount"].(float64); ok {
			f.nodePools[i].DesiredNodeCount = int(count)
		}
		writeJSON(w, http.StatusOK, f.nodePools[i])
		return
	}
	writeJSON(w, http.StatusNotFound, api.GenericError{Message: "node pool " + id + " not found"})
}

// create answers POST /nodePools, enforcing the required fields the way CWE
// does and storing the pool so a later list returns it.
func (f *fakeCWE) create(w http.ResponseWriter, r *http.Request) {
	var raw map[string]any
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, api.GenericError{Message: "malformed body: " + err.Error()})
		return
	}
	f.creates = append(f.creates, raw)

	// Recorded before failing, so a test can still assert what was sent.
	if f.createStatus != 0 {
		writeJSON(w, f.createStatus, json.RawMessage(f.createBody))
		return
	}
	// The generated request type has no omitempty on the required fields, so
	// an omitted one arrives as its zero value rather than absent.
	for _, field := range []string{"clusterID", "name", "nodeNamePrefix"} {
		if v, _ := raw[field].(string); v == "" || v == uuid.Nil.String() {
			writeJSON(w, http.StatusBadRequest,
				api.GenericError{Message: "missing required field: " + field})
			return
		}
	}

	id, _ := raw["clusterID"].(string)
	clusterID, err := uuid.Parse(id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, api.GenericError{Message: "clusterID is not a uuid"})
		return
	}
	name, _ := raw["name"].(string)
	prefix, _ := raw["nodeNamePrefix"].(string)
	count, _ := raw["desiredNodeCount"].(float64)
	pool := api.NodePool{
		ID:               uuid.New(),
		ClusterID:        clusterID,
		Name:             name,
		NodeNamePrefix:   prefix,
		DesiredNodeCount: int(count),
		NodeLabels:       map[string]string{},
	}
	f.nodePools = append(f.nodePools, pool)
	writeJSON(w, http.StatusCreated, pool)
}

// delete answers DELETE /nodePools/{id}, removing the pool from the store on
// success. CWE's real response carries no body on 202.
func (f *fakeCWE) delete(w http.ResponseWriter, id string) {
	if f.deleteStatus != 0 {
		writeJSON(w, f.deleteStatus, json.RawMessage(f.deleteBody))
		return
	}

	for i, p := range f.nodePools {
		if p.ID.String() != id {
			continue
		}
		f.nodePools = append(f.nodePools[:i], f.nodePools[i+1:]...)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusNotFound, api.GenericError{Message: "node pool " + id + " not found"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
