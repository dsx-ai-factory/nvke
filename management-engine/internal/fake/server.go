// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package fake provides a seeded NVKE controller API for controller fixtures.
package fake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

type resourceCollection[T any] struct {
	StatusCode int `json:"statusCode,omitempty"`
	Resources  T   `json:"resources,omitempty"`
}

type requestRecord struct {
	Method         string                     `json:"method"`
	Path           string                     `json:"path"`
	Create         *api.CreateClusterRequest  `json:"create,omitempty"`
	CreateNodePool *api.CreateNodePoolRequest `json:"createNodePool,omitempty"`
	UpdateNodePool *api.UpdateNodePoolRequest `json:"updateNodePool,omitempty"`
	Update         json.RawMessage            `json:"update,omitempty"`
}

type nodePoolSeed struct {
	resourceCollection[[]api.NodePool]
	Create           json.RawMessage `json:"create,omitempty"`
	CreateStatusCode int             `json:"createStatusCode,omitempty"`
}

type serverSeed struct {
	Clusters         resourceCollection[[]api.Cluster] `json:"clusters"`
	Create           json.RawMessage                   `json:"create,omitempty"`
	NodePools        nodePoolSeed                      `json:"nodePools,omitempty"`
	UpdateStatusCode int                               `json:"updateStatusCode,omitempty"`
}

type serverDump struct {
	Clusters  []api.Cluster   `json:"clusters"`
	Requests  []requestRecord `json:"requests"`
	NodePools []api.NodePool  `json:"nodePools"`
}

// Server owns one fixture's external records and write requests.
type Server struct {
	mu               sync.Mutex
	clusters         resourceCollection[map[uuid.UUID]api.Cluster]
	create           json.RawMessage
	requests         []requestRecord
	nodePools        resourceCollection[map[uuid.UUID]api.NodePool]
	createPool       json.RawMessage
	createPoolStatus int
	updateStatusCode int
}

// New returns an empty fake API with no lifecycle simulation.
func New() *Server {
	return &Server{
		clusters:  resourceCollection[map[uuid.UUID]api.Cluster]{Resources: map[uuid.UUID]api.Cluster{}},
		nodePools: resourceCollection[map[uuid.UUID]api.NodePool]{Resources: map[uuid.UUID]api.NodePool{}},
	}
}

// Handler returns the HTTP surface used by controller envtests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/clusters", handle(s, &s.clusters, s.listClusters))
	mux.HandleFunc("GET /api/clusters/{clusterID}", handle(s, &s.clusters, s.getCluster))
	mux.HandleFunc("POST /api/clusters", handle(s, &s.clusters, s.createCluster))
	mux.HandleFunc("PATCH /api/clusters/{clusterID}", handle(s, &s.clusters, s.updateCluster))
	mux.HandleFunc("DELETE /api/clusters/{clusterID}", handle(s, &s.clusters, s.deleteCluster))
	mux.HandleFunc("GET /api/nodePools", handle(s, &s.nodePools, s.listNodePools))
	mux.HandleFunc("GET /api/nodePools/{nodePoolID}", handle(s, &s.nodePools, s.getNodePool))
	mux.HandleFunc("POST /api/nodePools", handle(s, &s.nodePools, s.createNodePool))
	mux.HandleFunc("PATCH /api/nodePools/{nodePoolID}", handle(s, &s.nodePools, s.updateNodePool))
	mux.HandleFunc("DELETE /api/nodePools/{nodePoolID}", handle(s, &s.nodePools, s.deleteNodePool))
	return mux
}

func handle[T any](s *Server, collection *resourceCollection[T], handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		status := collection.StatusCode
		s.mu.Unlock()
		if status != 0 {
			writeError(w, status, http.StatusText(status))
			return
		}
		handler(w, r)
	}
}

// SeedFromYAML loads existing records and the declared defaults for newly created records.
func (s *Server) SeedFromYAML(input string) error {
	var seed serverSeed
	if err := yaml.UnmarshalStrict([]byte(input), &seed); err != nil {
		return fmt.Errorf("decode fake NVKE controller resources: %w", err)
	}
	if seed.Clusters.StatusCode != 0 && (seed.Clusters.StatusCode < 400 || seed.Clusters.StatusCode > 599) {
		return fmt.Errorf("clusters.statusCode must be an HTTP error status or zero")
	}
	for _, cluster := range seed.Clusters.Resources {
		if cluster.ID == uuid.Nil || cluster.Name == "" {
			return fmt.Errorf("seeded cluster requires a nonzero id and name")
		}
	}
	if seed.NodePools.StatusCode != 0 && (seed.NodePools.StatusCode < 400 || seed.NodePools.StatusCode > 599) {
		return fmt.Errorf("nodePools.statusCode must be an HTTP error status or zero")
	}
	for _, pool := range seed.NodePools.Resources {
		if pool.ID == uuid.Nil || pool.ClusterID == uuid.Nil || pool.Name == "" {
			return fmt.Errorf("seeded node pool requires nonzero id, clusterID and name")
		}
	}
	if len(seed.Create) != 0 {
		var cluster api.Cluster
		if err := json.Unmarshal(seed.Create, &cluster); err != nil {
			return fmt.Errorf("decode create defaults: %w", err)
		}
		if cluster.ID == uuid.Nil {
			return fmt.Errorf("create defaults require a nonzero id")
		}
	}
	if len(seed.NodePools.Create) != 0 {
		var pool api.NodePool
		if err := json.Unmarshal(seed.NodePools.Create, &pool); err != nil {
			return fmt.Errorf("decode node pool create defaults: %w", err)
		}
		if pool.ID == uuid.Nil || pool.ClusterID == uuid.Nil {
			return fmt.Errorf("node pool create defaults require nonzero id and clusterID")
		}
	}
	if seed.NodePools.CreateStatusCode != 0 && (seed.NodePools.CreateStatusCode < 400 || seed.NodePools.CreateStatusCode > 599) {
		return fmt.Errorf("nodePools.createStatusCode must be an HTTP error status or zero")
	}
	if seed.UpdateStatusCode != 0 && (seed.UpdateStatusCode < 400 || seed.UpdateStatusCode > 599) {
		return fmt.Errorf("updateStatusCode must be an HTTP error status or zero")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusters.StatusCode = seed.Clusters.StatusCode
	for _, cluster := range seed.Clusters.Resources {
		s.clusters.Resources[cluster.ID] = cluster
	}
	s.create = seed.Create
	s.nodePools.StatusCode = seed.NodePools.StatusCode
	for _, pool := range seed.NodePools.Resources {
		s.nodePools.Resources[pool.ID] = pool
	}
	s.createPool = seed.NodePools.Create
	s.createPoolStatus = seed.NodePools.CreateStatusCode
	s.updateStatusCode = seed.UpdateStatusCode
	return nil
}

// Dump snapshots stored records and deduplicated writes without counting read polling.
func (s *Server) Dump() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dump := serverDump{Clusters: s.sortedClusters(), Requests: []requestRecord{}, NodePools: s.sortedNodePools()}
	for _, request := range s.requests {
		if !slices.ContainsFunc(dump.Requests, func(existing requestRecord) bool {
			return reflect.DeepEqual(existing, request)
		}) {
			dump.Requests = append(dump.Requests, request)
		}
	}
	content, err := yaml.Marshal(dump)
	if err != nil {
		return "", fmt.Errorf("marshal fake NVKE controller state: %w", err)
	}
	return string(content), nil
}

func (s *Server) sortedClusters() []api.Cluster {
	clusters := make([]api.Cluster, 0, len(s.clusters.Resources))
	for _, cluster := range s.clusters.Resources {
		clusters = append(clusters, cluster)
	}
	slices.SortFunc(clusters, func(a, b api.Cluster) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return clusters
}

func (s *Server) sortedNodePools() []api.NodePool {
	pools := make([]api.NodePool, 0, len(s.nodePools.Resources))
	for _, pool := range s.nodePools.Resources {
		pools = append(pools, pool)
	}
	slices.SortFunc(pools, func(a, b api.NodePool) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return pools
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clusters := s.sortedClusters()
	if name := r.URL.Query().Get("name"); name != "" {
		clusters = slices.DeleteFunc(clusters, func(cluster api.Cluster) bool { return cluster.Name != name })
	}
	writeJSON(w, http.StatusOK, api.ListClustersResponse{Clusters: clusters})
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := uuid.Parse(r.PathValue("clusterID"))
	cluster, exists := s.clusters.Resources[id]
	if err != nil || !exists {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	writeJSON(w, http.StatusOK, cluster)
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	var request api.CreateClusterRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid create request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, requestRecord{Method: r.Method, Path: r.URL.Path, Create: &request})
	for _, cluster := range s.clusters.Resources {
		if cluster.Name == request.Name {
			writeError(w, http.StatusBadRequest, "cluster name already exists")
			return
		}
	}
	var cluster api.Cluster
	if err := json.Unmarshal(s.create, &cluster); err != nil {
		writeError(w, http.StatusInternalServerError, "fixture has no valid create defaults")
		return
	}
	if _, exists := s.clusters.Resources[cluster.ID]; exists {
		writeError(w, http.StatusConflict, "create defaults reuse an existing cluster ID")
		return
	}
	// Request fields update the record while conditions remain exactly as declared in the seed.
	body, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot encode create request")
		return
	}
	if err := json.Unmarshal(body, &cluster); err != nil {
		writeError(w, http.StatusBadRequest, "cannot apply create request")
		return
	}
	s.clusters.Resources[cluster.ID] = cluster
	writeJSON(w, http.StatusCreated, cluster)
}

func (s *Server) updateCluster(w http.ResponseWriter, r *http.Request) {
	var body json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid cluster update request")
		return
	}
	var request api.UpdateClusterRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid cluster update request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, requestRecord{Method: r.Method, Path: r.URL.Path, Update: body})
	id, err := uuid.Parse(r.PathValue("clusterID"))
	cluster, exists := s.clusters.Resources[id]
	if err != nil || !exists {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	if s.updateStatusCode != 0 {
		writeError(w, s.updateStatusCode, http.StatusText(s.updateStatusCode))
		return
	}
	if request.ControlPlaneKubernetesVersion != nil {
		cluster.ControlPlaneKubernetesVersion = *request.ControlPlaneKubernetesVersion
	}
	if config := request.ControlPlaneConfig; config != nil {
		if cluster.ControlPlaneConfig == nil {
			cluster.ControlPlaneConfig = &api.ControlPlaneConfig{}
		}
		if config.DesiredCount != nil {
			cluster.ControlPlaneConfig.DesiredCount = *config.DesiredCount
		}
		if config.SystemReservedMemory != nil {
			cluster.ControlPlaneConfig.SystemReservedMemory = *config.SystemReservedMemory
		}
		if config.MaxSurge != nil {
			cluster.ControlPlaneConfig.MaxSurge = config.MaxSurge
		}
		if infra := config.Infra; infra != nil {
			if cluster.ControlPlaneConfig.Infra == nil {
				cluster.ControlPlaneConfig.Infra = &api.InfraControlPlaneProvider{}
			}
			if infra.MachineTemplate != nil {
				cluster.ControlPlaneConfig.Infra.MachineTemplate = infra.MachineTemplate
			}
			if infra.BootstrapFiles != nil {
				cluster.ControlPlaneConfig.Infra.BootstrapFiles = infra.BootstrapFiles
			}
			if infra.PreKubeadmCommands != nil {
				cluster.ControlPlaneConfig.Infra.PreKubeadmCommands = infra.PreKubeadmCommands
			}
		}
	}
	s.clusters.Resources[id] = cluster
	writeJSON(w, http.StatusOK, cluster)
}

func (s *Server) listNodePools(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pools := s.sortedNodePools()
	if clusterName := r.URL.Query().Get("clusterName"); clusterName != "" {
		pools = slices.DeleteFunc(pools, func(pool api.NodePool) bool {
			cluster, exists := s.clusters.Resources[pool.ClusterID]
			return !exists || cluster.Name != clusterName
		})
	}
	if clusterID := r.URL.Query().Get("clusterID"); clusterID != "" {
		pools = slices.DeleteFunc(pools, func(pool api.NodePool) bool { return pool.ClusterID.String() != clusterID })
	}
	if name := r.URL.Query().Get("name"); name != "" {
		pools = slices.DeleteFunc(pools, func(pool api.NodePool) bool { return pool.Name != name })
	}
	writeJSON(w, http.StatusOK, api.ListNodePoolsResponse{NodePools: pools})
}

func (s *Server) getNodePool(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := uuid.Parse(r.PathValue("nodePoolID"))
	pool, exists := s.nodePools.Resources[id]
	if err != nil || !exists {
		writeError(w, http.StatusNotFound, "node pool not found")
		return
	}
	writeJSON(w, http.StatusOK, pool)
}

func (s *Server) createNodePool(w http.ResponseWriter, r *http.Request) {
	var request api.CreateNodePoolRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Name == "" || request.ClusterID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "invalid node pool create request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, requestRecord{Method: r.Method, Path: r.URL.Path, CreateNodePool: &request})
	if s.createPoolStatus != 0 {
		writeError(w, s.createPoolStatus, http.StatusText(s.createPoolStatus))
		return
	}
	if _, exists := s.clusters.Resources[request.ClusterID]; !exists {
		writeError(w, http.StatusNotFound, "parent cluster not found")
		return
	}
	for _, pool := range s.nodePools.Resources {
		if pool.ClusterID == request.ClusterID && pool.Name == request.Name {
			writeError(w, http.StatusBadRequest, "node pool name already exists")
			return
		}
	}
	var pool api.NodePool
	if err := json.Unmarshal(s.createPool, &pool); err != nil {
		writeError(w, http.StatusInternalServerError, "fixture has no valid node pool create defaults")
		return
	}
	if pool.ClusterID != request.ClusterID {
		writeError(w, http.StatusInternalServerError, "node pool create defaults belong to a different cluster")
		return
	}
	if _, exists := s.nodePools.Resources[pool.ID]; exists {
		writeError(w, http.StatusConflict, "node pool create defaults reuse an existing id")
		return
	}
	body, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot encode node pool create request")
		return
	}
	if err := json.Unmarshal(body, &pool); err != nil {
		writeError(w, http.StatusBadRequest, "cannot apply node pool create request")
		return
	}
	s.nodePools.Resources[pool.ID] = pool
	writeJSON(w, http.StatusCreated, pool)
}

func (s *Server) updateNodePool(w http.ResponseWriter, r *http.Request) {
	var request api.UpdateNodePoolRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || (request.DesiredNodeCount == nil && request.Infra == nil) ||
		(request.DesiredNodeCount != nil && *request.DesiredNodeCount < 0) {
		writeError(w, http.StatusBadRequest, "invalid node pool scale request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, requestRecord{Method: r.Method, Path: r.URL.Path, UpdateNodePool: &request})
	id, err := uuid.Parse(r.PathValue("nodePoolID"))
	pool, exists := s.nodePools.Resources[id]
	if err != nil || !exists {
		writeError(w, http.StatusNotFound, "node pool not found")
		return
	}
	if request.DesiredNodeCount != nil {
		pool.DesiredNodeCount = *request.DesiredNodeCount
	}
	if request.Infra != nil {
		pool.Infra = &api.InfraNodePoolProvider{
			BootstrapFiles:     request.Infra.BootstrapFiles,
			MachineTemplate:    request.Infra.MachineTemplate,
			PreKubeadmCommands: request.Infra.PreKubeadmCommands,
		}
	}
	s.nodePools.Resources[id] = pool
	writeJSON(w, http.StatusOK, pool)
}

func (s *Server) deleteNodePool(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, requestRecord{Method: r.Method, Path: r.URL.Path})
	id, err := uuid.Parse(r.PathValue("nodePoolID"))
	pool, exists := s.nodePools.Resources[id]
	if err != nil || !exists {
		writeError(w, http.StatusNotFound, "node pool not found")
		return
	}
	// Accepted deletion stays pending; completed deletion is seeded as an absent pool.
	pool.Conditions.Deleting.Status = api.ConditionStatus("True")
	s.nodePools.Resources[id] = pool
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, requestRecord{Method: r.Method, Path: r.URL.Path})
	id, err := uuid.Parse(r.PathValue("clusterID"))
	if _, exists := s.clusters.Resources[id]; err != nil || !exists {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	delete(s.clusters.Resources, id)
	for poolID, pool := range s.nodePools.Resources {
		if pool.ClusterID == id {
			delete(s.nodePools.Resources, poolID)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, api.GenericError{Message: message})
}
