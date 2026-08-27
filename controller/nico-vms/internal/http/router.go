// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"log/slog"
	nethttp "net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/nke/controller/nico-vms/internal/sshexec"
	"github.com/NVIDIA/nke/controller/nico-vms/internal/state"
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

	// registerV2 registers a handler under both "carbide" and "forge" API
	// names so that the NCX SDK's /v2/org/{org}/carbide/... paths are served
	// alongside the existing /v2/org/{org}/forge/... paths.
	registerV2 := func(mux *nethttp.ServeMux, method, suffix string, handler nethttp.HandlerFunc) {
		for _, apiName := range []string{"carbide", "forge"} {
			mux.HandleFunc(method+" /v2/org/{org}/"+apiName+"/"+suffix, handler)
		}
	}

	// Health check - no auth required (handled by authMiddleware path check).
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// ── Instance CRUD ── v1 paths ────────────────────────────────────────
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances", s.handleListInstances)
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances/{instanceId}", s.handleGetInstance)
	mux.HandleFunc("POST /v1/orgs/{orgId}/instances", s.handleCreateInstance)
	mux.HandleFunc("PATCH /v1/orgs/{orgId}/instances/{instanceId}", s.handlePatchInstance)
	mux.HandleFunc("DELETE /v1/orgs/{orgId}/instances/{instanceId}", s.handleDeleteInstance)

	// ── Instance CRUD ── v2 paths (carbide + forge) ──────────────────────
	registerV2(mux, "GET", "instance", s.handleListInstances)
	registerV2(mux, "GET", "instance/{instanceId}", s.handleGetInstance)
	registerV2(mux, "POST", "instance", s.handleCreateInstance)
	registerV2(mux, "PATCH", "instance/{instanceId}", s.handlePatchInstance)
	registerV2(mux, "DELETE", "instance/{instanceId}", s.handleDeleteInstance)

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

	// ── Static data endpoints ── v2 paths (carbide + forge) ──────────────
	registerV2(mux, "GET", "site", s.handleListSites)
	registerV2(mux, "GET", "vpc", s.handleListVPCs)
	// Single VPC lookup — CAPNICo reads VPC name for machine topology. Safe on
	// the main mux: "vpc/{vpcId}" has no wildcard sibling under "vpc" (unlike
	// the instance/type/{instanceTypeId} route below), so no ServeMux conflict.
	registerV2(mux, "GET", "vpc/{vpcId}", s.handleGetVPC)
	registerV2(mux, "GET", "subnet", s.handleListSubnets)
	registerV2(mux, "GET", "vpc-prefix", s.handleListVPCPrefixes)
	registerV2(mux, "GET", "instance/type", s.handleListInstanceTypes)
	registerV2(mux, "GET", "sshkeygroup", s.handleListSSHKeyGroups)
	registerV2(mux, "GET", "ipblock", s.handleListIPBlocks)
	registerV2(mux, "GET", "infiniband-partition", s.handleListInfiniBandPartitions)
	registerV2(mux, "GET", "nvlink-logical-partition", s.handleEmptyList)

	// ── Tenant endpoints ── v2 paths (carbide + forge) ───────────────────
	// CAPNICo resolves its tenant ID here before creating any instance.
	registerV2(mux, "GET", "tenant/current", s.handleGetCurrentTenant)

	// ── Diagnostics endpoints ───────────────────────────────────────────
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances/{instanceId}/diagnostics", s.handleListDiagnostics)
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances/{instanceId}/diagnostics/{itemId}", s.handleGetDiagnostic)
	registerV2(mux, "GET", "instance/{instanceId}/diagnostics", s.handleListDiagnostics)
	registerV2(mux, "GET", "instance/{instanceId}/diagnostics/{itemId}", s.handleGetDiagnostic)

	// ── Console log endpoints ───────────────────────────────────────────
	mux.HandleFunc("GET /v1/orgs/{orgId}/instances/{instanceId}/console-log", s.handleConsoleLog)
	registerV2(mux, "GET", "instance/{instanceId}/console-log", s.handleConsoleLog)

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
	// "GET /v2/org/{org}/.../instance/type/{instanceTypeId}" cannot be
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
	registerV2(instanceTypeMux, "GET", "instance/type/{instanceTypeId}", s.handleGetInstanceType)

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
// endpoints that nico-vms doesn't implement (e.g. nvlink-logical-partitions).
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
