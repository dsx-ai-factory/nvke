// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The HTTP fixture backend exercises the production client without a provisioning service.

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/cluster-api-provider-nico/pkg/test/fixture"
	"github.com/dsx-ai-factory/nke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nke/management-engine/internal/cwe"
)

var fixtureRemoteID = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

// Keep persisted endpoints stable in goldens while exercising the real HTTP client.
type fixtureCWEClient struct{ *cwe.Client }

func (*fixtureCWEClient) Endpoint() string { return "https://cwe.example.com/api" }

type cweFixtureConfig struct {
	RecoveryEdited bool `json:"recoveryEdited"`
	Ready          bool `json:"ready"`
	Recover        bool `json:"recover"`
	DeletePending  bool `json:"deletePending"`
}

type cweFixture struct {
	mu              sync.Mutex
	config          cweFixtureConfig
	request         *api.CreateClusterRequest
	observedCluster *api.Cluster
	calls           map[string]int
	kube            client.Client
	source          client.ObjectKey
}

func setupCWEFixture(ctx SpecContext, tc *fixture.Case, reconciler *NKEClusterReconciler) {
	input, exists := tc.Input("input_cwe.yaml")
	if !exists {
		return
	}
	fake := &cweFixture{calls: map[string]int{}, kube: tc.Client, source: client.ObjectKey{Namespace: testNamespace, Name: testCluster}}
	Expect(yaml.UnmarshalStrict([]byte(input), &fake.config)).To(Succeed())
	bindings, exists := tc.Input("input_bindings.yaml")
	Expect(exists).To(BeTrue())
	Expect(yaml.UnmarshalStrict([]byte(bindings), &reconciler.Bindings)).To(Succeed())
	var source internalv1.NKECluster
	Expect(tc.Client.Get(ctx, fake.source, &source)).To(Succeed())
	request, err := ResolveCWERequest(&source, reconciler.Bindings)
	Expect(err).NotTo(HaveOccurred())
	if fake.config.Recover {
		fake.observedCluster = fake.cluster(request)
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	DeferCleanup(server.Close)
	httpClient, err := cwe.New(cwe.Config{Endpoint: server.URL})
	Expect(err).NotTo(HaveOccurred())
	reconciler.CWE = &fixtureCWEClient{Client: httpClient}
	if fake.config.Recover {
		controllerutil.AddFinalizer(&source, nkeClusterFinalizer)
		Expect(tc.Client.Update(ctx, &source)).To(Succeed())
		source.Status.Provisioning = &internalv1.ClusterProvisioningStatus{
			Endpoint: reconciler.CWE.Endpoint(), Generation: source.Generation,
		}
		Expect(tc.Client.Status().Update(ctx, &source)).To(Succeed())
		if fake.config.RecoveryEdited {
			source.Spec.ControlPlaneClass = "changed"
			Expect(tc.Client.Update(ctx, &source)).To(Succeed())
		}
	}
	tc.AddGolden("expected_cwe.yaml", fake.golden)
}

func (f *cweFixture) cluster(request api.CreateClusterRequest) *api.Cluster {
	cluster := api.Cluster{ID: fixtureRemoteID, Name: request.Name}
	cluster.Conditions.Deleting.Status = "False"
	cluster.Conditions.Provisioning.Status = "True"
	cluster.Conditions.Provisioned.Status = "False"
	if f.config.Ready {
		cluster.Conditions.Provisioning.Status = "False"
		cluster.Conditions.Provisioned.Status = "True"
	}
	return &cluster
}

func (f *cweFixture) serveHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodGet {
		if req.URL.Path == "/api/clusters" {
			clusters := []api.Cluster{}
			if f.observedCluster != nil && f.observedCluster.Name == req.URL.Query().Get("name") {
				clusters = append(clusters, *f.observedCluster)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"clusters": clusters})
			return
		}
		if f.observedCluster == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(f.observedCluster)
		return
	}
	var source internalv1.NKECluster
	if err := f.kube.Get(req.Context(), f.source, &source); err != nil || !controllerutil.ContainsFinalizer(&source, nkeClusterFinalizer) {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if source.Status.Provisioning == nil || source.Status.Provisioning.Endpoint != "https://cwe.example.com/api" {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if req.Method == http.MethodPost && source.Status.Provisioning.Generation != source.Generation {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	key := req.Method + " " + strings.ReplaceAll(req.URL.Path, fixtureRemoteID.String(), "REMOTE-ID")
	f.calls[key]++
	switch req.Method {
	case http.MethodPost:
		var request api.CreateClusterRequest
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.request = &request
		if f.observedCluster != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.observedCluster = f.cluster(request)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(f.observedCluster)
	case http.MethodDelete:
		if f.observedCluster == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.config.DeletePending {
			f.observedCluster.Conditions.Deleting.Status = "True"
		} else {
			f.observedCluster = nil
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *cweFixture) golden(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := struct {
		Calls         []string                  `json:"calls"`
		CreateRequest *api.CreateClusterRequest `json:"createRequest,omitempty"`
	}{Calls: make([]string, 0, len(f.calls))}
	for request, count := range f.calls {
		result.Calls = append(result.Calls, fmt.Sprintf("%s (%d calls; finalizer persisted)", request, count))
	}
	sort.Strings(result.Calls)
	if f.request != nil {
		request := *f.request
		request.Name = "nke-CLUSTER-UID"
		result.CreateRequest = &request
	}
	data, err := yaml.Marshal(result)
	return string(data), err
}
