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
	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/clients"
	"github.com/NVIDIA/nke/cli/internal/config"
	api "github.com/NVIDIA/nke/controller/api"
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

// clientFnErr is a clientFn that always returns an error. Commands that pass
// arg/flag validation will reach the API call and return this error — used to
// confirm a command is wired correctly without a real CWE deployment.
func clientFnErr() (*clients.CWEClient, error) {
	return nil, &notConfiguredError{}
}

type notConfiguredError struct{}

func (e *notConfiguredError) Error() string { return "no site configured" }

func isNotConfigured(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no site configured")
}

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
	server   *httptest.Server

	// lastBody captures the most recent PATCH request body, so a test can
	// assert on the literal wire bytes the CLI sent rather than a decoded
	// struct, which would hide an omitempty regression.
	lastBody []byte

	// patchFailStatus/patchFailMessage, when patchFailStatus is nonzero,
	// make the next PATCH answer with that status and message instead of
	// applying it — for testing server-side rejection without blocking the
	// preceding GET that resolves the cluster name.
	patchFailStatus  int
	patchFailMessage string
}

// newFakeCWE starts a fake CWE seeded with clusters, and returns it together
// with a CWEClientFn wired to it.
func newFakeCWE(t *testing.T, seed ...api.Cluster) (*fakeCWE, CWEClientFn) {
	t.Helper()
	f := &fakeCWE{clusters: seed}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f, clientFnFor(f.server.URL)
}

// newBrokenCWE returns a CWEClientFn for a server that answers every request with
// status and body, for the error paths.
func newBrokenCWE(t *testing.T, status int, body string) CWEClientFn {
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
func clientFnFor(url string) CWEClientFn {
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

// stored returns the clusters the fake currently holds.
func (f *fakeCWE) stored() []api.Cluster {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.Cluster(nil), f.clusters...)
}

// lastRequestBody returns the most recent PATCH request's raw body.
func (f *fakeCWE) lastRequestBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.lastBody...)
}

// failPatch makes the next PATCH answer with status and message instead of
// applying it.
func (f *fakeCWE) failPatch(status int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patchFailStatus = status
	f.patchFailMessage = message
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

	switch id, isItem := strings.CutPrefix(path, "/clusters/"); {
	case path == "/clusters" && r.Method == http.MethodGet:
		f.list(w, r.URL.Query().Get("name"))
	case path == "/clusters" && r.Method == http.MethodPost:
		f.create(w, r)
	case isItem && r.Method == http.MethodGet:
		f.get(w, id)
	case isItem && r.Method == http.MethodPatch:
		f.patch(w, r, id)
	case isItem && r.Method == http.MethodDelete:
		f.remove(w, id)
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

// patch answers PATCH /clusters/{id}, storing the raw request body for
// lastRequestBody and merging the decoded fields onto the stored cluster.
// If failPatch was called, it answers with that status/message instead.
func (f *fakeCWE) patch(w http.ResponseWriter, r *http.Request, id string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, api.GenericError{Message: "reading body: " + err.Error()})
		return
	}
	f.lastBody = body

	if f.patchFailStatus != 0 {
		status, message := f.patchFailStatus, f.patchFailMessage
		f.patchFailStatus, f.patchFailMessage = 0, ""
		writeJSON(w, status, api.GenericError{Message: message})
		return
	}

	var req api.UpdateClusterRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, api.GenericError{Message: "malformed body: " + err.Error()})
		return
	}
	for i, c := range f.clusters {
		if c.ID.String() != id {
			continue
		}
		if req.ControlPlaneKubernetesVersion != nil {
			f.clusters[i].ControlPlaneKubernetesVersion = *req.ControlPlaneKubernetesVersion
		}
		if req.TeleportEnabled != nil {
			f.clusters[i].TeleportEnabled = req.TeleportEnabled
		}
		writeJSON(w, http.StatusOK, f.clusters[i])
		return
	}
	writeJSON(w, http.StatusNotFound, api.GenericError{Message: "cluster " + id + " not found"})
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
