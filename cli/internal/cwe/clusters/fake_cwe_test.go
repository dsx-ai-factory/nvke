// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/cli/internal/clients"
	"github.com/NVIDIA/nke/cli/internal/config"
	api "github.com/NVIDIA/nke/controller/api"
)

// fakeCWE is an httptest server answering the cluster endpoints nkectl calls,
// backed by an in-memory slice.
//
// It exists so a test can run the real path end to end: cobra flags, the real
// CWEClient, real HTTP, real JSON decoding, and the real output formatting.
// The only double before this was clientFnErr, which fails on the first API
// call, so nothing downstream of that call had ever been executed by a test.
type fakeCWE struct {
	mu       sync.Mutex
	clusters []api.Cluster
	requests []string
	lastBody []byte
	server   *httptest.Server

	// updateFailStatus, when non-zero, makes update answer with this status
	// and updateFailMessage instead of applying the patch.
	updateFailStatus  int
	updateFailMessage string
}

// newFakeCWE starts a fake CWE seeded with clusters, and returns it together
// with a ClientFn wired to it.
func newFakeCWE(t *testing.T, seed ...api.Cluster) (*fakeCWE, ClientFn) {
	t.Helper()
	f := &fakeCWE{clusters: seed}
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

// failUpdate makes the next PATCH /clusters/{id} request fail with status
// and message instead of applying the patch.
func (f *fakeCWE) failUpdate(status int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateFailStatus = status
	f.updateFailMessage = message
}

// stored returns the clusters the fake currently holds.
func (f *fakeCWE) stored() []api.Cluster {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.Cluster(nil), f.clusters...)
}

// lastRequestBody returns the raw body of the most recent request, exactly
// as the client sent it.
func (f *fakeCWE) lastRequestBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.lastBody...)
}

func (f *fakeCWE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	// Reading r.Body drains it, so replace it with a fresh reader over the
	// same bytes for the route handlers below to decode.
	body, _ := io.ReadAll(r.Body)
	f.lastBody = body
	r.Body = io.NopCloser(bytes.NewReader(body))

	// The client appends /api to the configured site URL. Matching on that
	// prefix rather than stripping it silently means a client that stops
	// sending it fails these tests instead of passing them.
	path, hasPrefix := strings.CutPrefix(r.URL.Path, "/api")
	if !hasPrefix {
		writeJSON(w, http.StatusNotFound,
			api.GenericError{Message: "request did not carry the /api prefix: " + r.URL.Path})
		return
	}

	switch id, isItem := strings.CutPrefix(path, "/clusters/"); {
	case path == "/clusters" && r.Method == http.MethodGet:
		f.list(w, r.URL.Query().Get("name"))
	case path == "/clusters" && r.Method == http.MethodPost:
		f.create(w, r)
	case isItem && r.Method == http.MethodGet:
		f.get(w, id)
	case isItem && r.Method == http.MethodDelete:
		f.remove(w, id)
	case isItem && r.Method == http.MethodPatch:
		f.update(w, id, r)
	default:
		writeJSON(w, http.StatusNotFound,
			api.GenericError{Message: "no route for " + r.Method + " " + path})
	}
}

// list answers GET /clusters, filtered by name when the query carries one.
func (f *fakeCWE) list(w http.ResponseWriter, name string) {
	out := []api.Cluster{}
	for _, c := range f.clusters {
		if name == "" || c.Name == name {
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, api.ListClustersResponse{Clusters: out})
}

// create answers POST /clusters, storing what it was sent so a test can assert
// the CLI serialised the spec it was given.
func (f *fakeCWE) create(w http.ResponseWriter, r *http.Request) {
	var req api.CreateClusterJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, api.GenericError{Message: "malformed body: " + err.Error()})
		return
	}
	var cniVersion string
	if req.CniVersion != nil {
		cniVersion = *req.CniVersion
	}
	cl := api.Cluster{
		ID:                            uuid.New(),
		Name:                          req.Name,
		ControlPlaneKubernetesVersion: req.ControlPlaneKubernetesVersion,
		CniPlugin:                     req.CniPlugin,
		CniVersion:                    cniVersion,
		EtcdEncrypted:                 req.EtcdEncrypted,
		CreatedAt:                     time.Now(),
		Conditions: api.ClusterConditions{
			Provisioning: api.Condition{Status: api.ConditionStatusTrue},
		},
	}
	f.clusters = append(f.clusters, cl)
	writeJSON(w, http.StatusCreated, cl)
}

func (f *fakeCWE) get(w http.ResponseWriter, id string) {
	for _, c := range f.clusters {
		if c.ID.String() == id {
			writeJSON(w, http.StatusOK, c)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, api.GenericError{Message: "cluster " + id + " not found"})
}

// remove answers DELETE /clusters/{id} with 202, which is the status the client
// requires; anything else is reported as an error.
func (f *fakeCWE) remove(w http.ResponseWriter, id string) {
	for i, c := range f.clusters {
		if c.ID.String() == id {
			f.clusters = append(f.clusters[:i], f.clusters[i+1:]...)
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, api.GenericError{Message: "cluster " + id + " not found"})
}

// update answers PATCH /clusters/{id}, applying the subset of fields this
// fake understands and returning the updated record.
func (f *fakeCWE) update(w http.ResponseWriter, id string, r *http.Request) {
	if f.updateFailStatus != 0 {
		writeJSON(w, f.updateFailStatus, api.GenericError{Message: f.updateFailMessage})
		return
	}
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// seedCluster builds a provisioned cluster for the fake to serve.
func seedCluster(name string) api.Cluster {
	return api.Cluster{
		ID:                            uuid.New(),
		Name:                          name,
		ControlPlaneKubernetesVersion: "1.30.0",
		CniPlugin:                     "calico",
		CniVersion:                    "3.30.1",
		CreatedAt:                     time.Now().Add(-2 * time.Hour),
		Conditions: api.ClusterConditions{
			Provisioned: api.Condition{Status: api.ConditionStatusTrue},
		},
	}
}
