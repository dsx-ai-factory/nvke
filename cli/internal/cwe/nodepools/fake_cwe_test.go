// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/cli/internal/clients"
	"github.com/NVIDIA/nke/cli/internal/config"
	api "github.com/NVIDIA/nke/controller/api"
)

// fakeCWE is an httptest server answering the node pool endpoints nkectl
// calls, backed by an in-memory slice.
//
// It exists so a test can run the real path end to end: cobra flags, the real
// CWEClient, real HTTP, real JSON decoding, and the real output formatting.
type fakeCWE struct {
	mu sync.Mutex
	// nodePools is the store. clusterNames maps each pool's cluster ID to the
	// cluster name it answers the clusterName filter under, which is all this
	// fake needs of a cluster.
	nodePools    []api.NodePool
	clusterNames map[uuid.UUID]string
	requests     []string
	patches      []map[string]any
	server       *httptest.Server
	// patchStatus and patchBody, when set, make every PATCH fail with them.
	// The list lookup keeps working, so a test can reach the update call.
	patchStatus int
	patchBody   string
}

// failPatchWith makes the fake reject every PATCH with status and body while
// still answering the list lookup normally. A fake that failed both would let
// a test pass on the lookup error and never reach the update at all.
func (f *fakeCWE) failPatchWith(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patchStatus, f.patchBody = status, body
}

// seed is a node pool to serve, together with the name of the cluster that
// owns it. Pools sharing a cluster name are served under one cluster ID.
type seed struct {
	clusterName      string
	name             string
	desiredNodeCount int
}

// newFakeCWE starts a fake CWE holding a node pool per seed, and returns it
// together with a ClientFn wired to it.
func newFakeCWE(t *testing.T, seeds ...seed) (*fakeCWE, ClientFn) {
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
			NodeLabels:        []string{},
			Conditions: api.NodePoolConditions{
				Provisioned: api.Condition{Status: api.ConditionStatusTrue},
			},
		})
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f, clientFnFor(f.server.URL)
}

// newBrokenCWE returns a ClientFn for a server that answers every request with
// status and body, for the error paths.
func newBrokenCWE(t *testing.T, status int, body string) ClientFn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return clientFnFor(srv.URL)
}

// clientFnFor builds a real CWEClient against url. Nothing here is a stub: the
// client is the one the binary uses, so a change to its URL handling, status
// checks or error parsing shows up in these tests.
func clientFnFor(url string) ClientFn {
	return func() (*clients.CWEClient, error) {
		return clients.New(config.SiteConfig{URL: url})
	}
}

// seen returns the requests the CLI made, as "METHOD /path?query".
func (f *fakeCWE) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// patchBodies returns the PATCH bodies the CLI sent, decoded as raw maps so a
// test can assert on exactly which fields went over the wire rather than on
// what the typed struct would have filled in.
func (f *fakeCWE) patchBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.patches...)
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
	case isItem && r.Method == http.MethodPatch:
		f.update(w, r, id)
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
