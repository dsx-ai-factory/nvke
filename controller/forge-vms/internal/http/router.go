// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package http

import (
	"log/slog"
	nethttp "net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/sshexec"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/state"
)

// HTTPMethod is an HTTP method name (e.g. "GET", "POST").
type HTTPMethod = string

// Server holds dependencies shared by all HTTP handlers.
type Server struct {
	store          *state.Store
	logger         *slog.Logger
	logMethods     map[string]bool
	consoleSSHPort int
	filesDir       string
	instancesDir   string
	sshConfig      sshexec.Config
}

// NewHandler wires every route into an http.NewServeMux and wraps it with
// the logging and auth middleware chain.
// logMethods controls which HTTP methods produce a log line; a nil or empty
// slice disables request logging entirely.
func NewHandler(store *state.Store, logger *slog.Logger, logMethods []HTTPMethod, consoleSSHPort int, filesDir, instancesDir string, sshCfg sshexec.Config) nethttp.Handler {
	methodSet := make(map[string]bool, len(logMethods))
	for _, m := range logMethods {
		methodSet[m] = true
	}
	s := &Server{
		store:          store,
		logger:         logger.With("component", "http"),
		logMethods:     methodSet,
		consoleSSHPort: consoleSSHPort,
		filesDir:       filesDir,
		instancesDir:   instancesDir,
		sshConfig:      sshCfg,
	}

	mux := nethttp.NewServeMux()

	// Health check - no auth required (handled by authMiddleware path check).
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// ── Instance CRUD ── v1 paths ────────────────────────────────────────
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances", s.handleListInstances)
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances/{instanceId}", s.handleGetInstance)
	mux.HandleFunc("POST /v1/orgs/{orgId}/instances", s.handleCreateInstance)
	mux.HandleFunc("PATCH /v1/orgs/{orgId}/instances/{instanceId}", s.handlePatchInstance)
	mux.HandleFunc("DELETE /v1/orgs/{orgId}/instances/{instanceId}", s.handleDeleteInstance)

	// ── Instance CRUD ── v2 paths ────────────────────────────────────────
	mux.HandleFunc("GET /v2/org/{org}/forge/instance", s.handleListInstances)
	mux.HandleFunc("GET /v2/org/{org}/forge/instance/{instanceId}", s.handleGetInstance)
	mux.HandleFunc("POST /v2/org/{org}/forge/instance", s.handleCreateInstance)
	mux.HandleFunc("PATCH /v2/org/{org}/forge/instance/{instanceId}", s.handlePatchInstance)
	mux.HandleFunc("DELETE /v2/org/{org}/forge/instance/{instanceId}", s.handleDeleteInstance)

	// ── Static data endpoints ── v1 paths ────────────────────────────────
	mux.HandleFunc("GET /v1/orgs/{orgId}/sites", s.handleListSites)
	mux.HandleFunc("GET /v1/orgs/{orgId}/vpcs", s.handleListVPCs)
	mux.HandleFunc("GET /v1/orgs/{orgId}/subnets", s.handleListSubnets)
	mux.HandleFunc("GET /v1/orgs/{orgId}/vpc-prefixes", s.handleListVPCPrefixes)
	mux.HandleFunc("GET /v1/orgs/{orgId}/instance-types", s.handleListInstanceTypes)
	mux.HandleFunc("GET /v1/orgs/{orgId}/sshkeygroups", s.handleListSSHKeyGroups)
	mux.HandleFunc("GET /v1/orgs/{orgId}/ipblocks", s.handleListIPBlocks)
	mux.HandleFunc("GET /v1/orgs/{orgId}/infiniband-partitions", s.handleListInfiniBandPartitions)
	mux.HandleFunc("GET /v1/orgs/{orgId}/nvlink-logical-partitions", s.handleEmptyList)

	// ── Static data endpoints ── v2 paths ────────────────────────────────
	mux.HandleFunc("GET /v2/org/{org}/forge/site", s.handleListSites)
	mux.HandleFunc("GET /v2/org/{org}/forge/vpc", s.handleListVPCs)
	// Single VPC lookup — CAPNICo reads VPC name for machine topology. Safe on
	// the main mux: "vpc/{vpcId}" has no wildcard sibling under "vpc" (unlike
	// the instance/type/{instanceTypeId} route below), so no ServeMux conflict.
	mux.HandleFunc("GET /v2/org/{org}/forge/vpc/{vpcId}", s.handleGetVPC)
	mux.HandleFunc("GET /v2/org/{org}/forge/subnet", s.handleListSubnets)
	mux.HandleFunc("GET /v2/org/{org}/forge/vpc-prefix", s.handleListVPCPrefixes)
	mux.HandleFunc("GET /v2/org/{org}/forge/instance/type", s.handleListInstanceTypes)
	mux.HandleFunc("GET /v2/org/{org}/forge/sshkeygroup", s.handleListSSHKeyGroups)
	mux.HandleFunc("GET /v2/org/{org}/forge/ipblock", s.handleListIPBlocks)
	mux.HandleFunc("GET /v2/org/{org}/forge/infiniband-partition", s.handleListInfiniBandPartitions)
	mux.HandleFunc("GET /v2/org/{org}/forge/nvlink-logical-partition", s.handleEmptyList)

	// ── Tenant endpoints ── v2 paths ─────────────────────────────────────
	// CAPNICo resolves its tenant ID here before creating any instance.
	mux.HandleFunc("GET /v2/org/{org}/forge/tenant/current", s.handleGetCurrentTenant)

	// ── Diagnostics endpoints ───────────────────────────────────────────
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances/{instanceId}/diagnostics", s.handleListDiagnostics)
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances/{instanceId}/diagnostics/{itemId}", s.handleGetDiagnostic)
	mux.HandleFunc("GET /v2/org/{org}/forge/instance/{instanceId}/diagnostics", s.handleListDiagnostics)
	mux.HandleFunc("GET /v2/org/{org}/forge/instance/{instanceId}/diagnostics/{itemId}", s.handleGetDiagnostic)

	// ── Console log endpoints ───────────────────────────────────────────
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances/{instanceId}/console-log", s.handleConsoleLog)
	mux.HandleFunc("GET /v2/org/{org}/forge/instance/{instanceId}/console-log", s.handleConsoleLog)

	// ── Server-scoped endpoints ─────────────────────────────────────────
	mux.HandleFunc("GET /server/files-tree", s.handleFilesTree)

	// ── File server ─────────────────────────────────────────────────────
	// Serves files from the configured files directory. No auth required
	// because VM boot and cloud-init fetches cannot depend on Bearer tokens.
	mux.HandleFunc("GET /files/", s.handleServeFile)

	// ── Web UI ──────────────────────────────────────────────────────────
	// Serves the static UI entrypoints and assets. No auth required.
	mux.HandleFunc("GET /ui", s.handleUIRoot)
	mux.HandleFunc("GET /ui/{$}", s.handleUIIndex)
	mux.HandleFunc("GET /ui/instances", s.handleUIIndex)
	mux.HandleFunc("GET /ui/files", s.handleUIIndex)
	mux.Handle("GET /ui/", nethttp.StripPrefix("/ui/", nethttp.FileServer(nethttp.Dir("ui"))))

	// ── Single instance type lookup ──────────────────────────────────────
	// CAPNICo's create preflight fetches one instance type by ID with
	// allocation stats (SDK InstanceTypeAPI.GetInstanceType). The route
	// "GET /v2/org/{org}/forge/instance/type/{instanceTypeId}" cannot be
	// registered on the main mux: Go 1.22's ServeMux rejects it because it
	// overlaps the sibling 7-segment wildcard patterns
	// ".../instance/{instanceId}/diagnostics" and ".../instance/{instanceId}/console-log"
	// (e.g. both match ".../instance/type/diagnostics") with neither more
	// specific, which panics at registration. Serving it from a dedicated mux
	// consulted first sidesteps the conflict without disturbing the existing
	// instance sub-routes. A request only reaches this mux when its path is a
	// literal ".../instance/type/<id>", so real instance sub-routes still fall
	// through to the main mux.
	instanceTypeMux := nethttp.NewServeMux()
	instanceTypeMux.HandleFunc("GET /v2/org/{org}/forge/instance/type/{instanceTypeId}", s.handleGetInstanceType)

	root := nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if _, pattern := instanceTypeMux.Handler(r); pattern != "" {
			instanceTypeMux.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	return s.loggingMiddleware(s.authMiddleware(root))
}

// handleHealthz returns a simple health-check response.
func (s *Server) handleHealthz(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
}

// handleEmptyList returns an empty JSON array. Used as a stub for Forge API
// endpoints that forge-vms doesn't implement (e.g. nvlink-logical-partitions).
func (s *Server) handleEmptyList(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, nethttp.StatusOK, []struct{}{})
}

// handleServeFile serves files from the configured files directory with path
// traversal protection. Requests to /files/<path> serve the corresponding file
// from the configured files directory on disk.
func (s *Server) handleServeFile(w nethttp.ResponseWriter, r *nethttp.Request) {
	if s.filesDir == "" {
		nethttp.Error(w, "file server not configured", nethttp.StatusNotFound)
		return
	}

	// Strip the /files/ prefix and clean the path.
	relPath := strings.TrimPrefix(r.URL.Path, "/files/")
	relPath = filepath.Clean(relPath)

	// Reject directory traversal attempts.
	if relPath == "" || relPath == "." || strings.HasPrefix(relPath, "..") || strings.Contains(relPath, "/../") {
		nethttp.Error(w, "invalid path", nethttp.StatusBadRequest)
		return
	}

	absPath := filepath.Join(s.filesDir, relPath)

	// Ensure the resolved path is still within the configured files directory.
	absFilesDir, _ := filepath.Abs(s.filesDir)
	absPath, _ = filepath.Abs(absPath)
	if !strings.HasPrefix(absPath, absFilesDir+string(os.PathSeparator)) && absPath != absFilesDir {
		nethttp.Error(w, "invalid path", nethttp.StatusBadRequest)
		return
	}

	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() {
		nethttp.Error(w, "not found", nethttp.StatusNotFound)
		return
	}

	s.logger.Info("serving file", "path", relPath, "size", info.Size())
	nethttp.ServeFile(w, r, absPath)
}
