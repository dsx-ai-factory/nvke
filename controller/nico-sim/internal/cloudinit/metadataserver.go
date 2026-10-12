// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

// InstanceLookup is the subset of state.Store needed by MetadataServer.
type InstanceLookup interface {
	LookupInstanceByIP(ctx context.Context, ip string) (instanceID string, found bool)
	LookupInstanceByMAC(ctx context.Context, mac string) (instanceID string, found bool)
	MarkVMPhonedHome(ctx context.Context, instanceID string) error
}

// MetadataServer serves cloud-init metadata for VM instances.
// All instance data is read from the database — no in-memory state.
// Supports EC2-style, NoCloud-net, and Forge-compatible NoCloud endpoints.
type MetadataServer struct {
	logger         *slog.Logger
	store          InstanceLookup
	workingDirBase string // base path; working dir = workingDirBase/instanceID
}

// NewMetadataServer creates a MetadataServer that reads instance data from the database.
func NewMetadataServer(logger *slog.Logger, store InstanceLookup, workingDirBase string) *MetadataServer {
	return &MetadataServer{
		logger:         logger,
		store:          store,
		workingDirBase: workingDirBase,
	}
}

// Handler returns an http.Handler serving cloud-init metadata.
func (s *MetadataServer) Handler() http.Handler {
	mux := http.NewServeMux()

	// EC2-style metadata endpoints for both versioned and "latest" paths.
	for _, prefix := range []string{"/2009-04-04", "/latest"} {
		mux.HandleFunc(prefix+"/meta-data/", s.handleMetaDataDir)
		mux.HandleFunc(prefix+"/meta-data/instance-id", s.handleMetaDataField("instance-id"))
		mux.HandleFunc(prefix+"/meta-data/local-hostname", s.handleMetaDataField("local-hostname"))
		mux.HandleFunc(prefix+"/meta-data/asn", s.handleMetaDataField("asn"))
		mux.HandleFunc(prefix+"/user-data", s.handleUserData)
		mux.HandleFunc(prefix+"/meta-data/phone_home", s.handlePhoneHome)
	}

	// NoCloud-net endpoints: /nocloud/{instanceID}/{file}
	mux.HandleFunc("/nocloud/{instanceID}/meta-data", s.handleNoCloudFile("meta-data"))
	mux.HandleFunc("/nocloud/{instanceID}/user-data", s.handleNoCloudFile("user-data"))
	mux.HandleFunc("/nocloud/{instanceID}/network-config", s.handleNoCloudFile("network-config"))

	// Forge-compatible NoCloud endpoints: /api/v0/cloud-init/{file}
	mux.HandleFunc("/api/v0/cloud-init/meta-data", s.handleForgeNoCloudFile("meta-data"))
	mux.HandleFunc("/api/v0/cloud-init/user-data", s.handleForgeNoCloudFile("user-data"))
	mux.HandleFunc("/api/v0/cloud-init/vendor-data", s.handleForgeVendorData)
	mux.HandleFunc("/api/v0/cloud-init/phone_home", s.handlePhoneHome)
	mux.HandleFunc("/api/v0/cloud-init/", s.handleForgeNoCloudDir)

	return mux
}

// lookupInstance resolves a source IP to an instance ID and working directory.
// First queries the database by IP, then falls back to ARP → MAC → DB lookup.
func (s *MetadataServer) lookupInstance(ip string) (dir, instanceID string, ok bool) {
	ctx := context.Background()

	instanceID, ok = s.store.LookupInstanceByIP(ctx, ip)
	if ok {
		return filepath.Join(s.workingDirBase, instanceID), instanceID, true
	}

	// Fallback: resolve IP → MAC via ARP table, then look up by MAC.
	mac := arpLookup(ip)
	if mac == "" {
		return "", "", false
	}

	instanceID, ok = s.store.LookupInstanceByMAC(ctx, mac)
	if !ok {
		return "", "", false
	}

	s.logger.Info("resolved instance via ARP lookup", "instanceID", instanceID, "ip", ip, "mac", mac)
	return filepath.Join(s.workingDirBase, instanceID), instanceID, true
}

// handleMetaDataDir serves the EC2-style metadata directory listing.
func (s *MetadataServer) handleMetaDataDir(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/meta-data/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	ip := sourceIP(r.RemoteAddr)
	_, _, ok := s.lookupInstance(ip)
	s.logger.Info("cloud-init metadata dir request", "ip", ip, "path", r.URL.Path)

	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, "instance-id\nlocal-hostname\nasn\n")
}

// handleMetaDataField returns a handler that serves a specific field from the meta-data file.
func (s *MetadataServer) handleMetaDataField(field string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := sourceIP(r.RemoteAddr)
		dir, instanceID, ok := s.lookupInstance(ip)

		s.logger.Info("cloud-init metadata request", "instanceID", instanceID, "ip", ip, "path", r.URL.Path)

		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		data, err := os.ReadFile(filepath.Join(dir, "meta-data"))
		if err != nil {
			s.logger.Error("failed to read meta-data file", "instanceID", instanceID, "error", err)
			http.Error(w, fmt.Sprintf("internal server error: %v", err), http.StatusInternalServerError)
			return
		}

		value, ok := parseMetaDataField(string(data), field)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, value)
	}
}

// handleUserData serves the user-data file for the requesting instance.
func (s *MetadataServer) handleUserData(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r.RemoteAddr)
	dir, instanceID, ok := s.lookupInstance(ip)

	s.logger.Info("cloud-init user-data request", "instanceID", instanceID, "ip", ip, "path", r.URL.Path)

	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	data, err := os.ReadFile(filepath.Join(dir, "user-data"))
	if err != nil {
		s.logger.Error("failed to read user-data file", "instanceID", instanceID, "error", err)
		http.Error(w, fmt.Sprintf("internal server error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write(data)
}

// handleNoCloudFile returns a handler that serves a specific NoCloud-net seed file.
func (s *MetadataServer) handleNoCloudFile(filename string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		instanceID := r.PathValue("instanceID")
		dir := filepath.Join(s.workingDirBase, instanceID)

		s.logger.Info("nocloud-net request", "instanceID", instanceID, "file", filename)

		data, err := os.ReadFile(filepath.Join(dir, filename))
		if err != nil {
			s.logger.Error("failed to read nocloud file", "instanceID", instanceID, "file", filename, "error", err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write(data)
	}
}

// handleForgeNoCloudFile returns a handler that serves a NoCloud seed file
// from the Forge-compatible /api/v0/cloud-init/ path. Instance identified by source IP.
func (s *MetadataServer) handleForgeNoCloudFile(filename string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := sourceIP(r.RemoteAddr)
		dir, instanceID, ok := s.lookupInstance(ip)

		s.logger.Info("forge nocloud request", "instanceID", instanceID, "ip", ip, "file", filename, "found", ok)

		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		data, err := os.ReadFile(filepath.Join(dir, filename))
		if err != nil {
			s.logger.Error("failed to read forge nocloud file", "instanceID", instanceID, "file", filename, "error", err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write(data)
	}
}

// handleForgeVendorData returns an empty response for vendor-data.
func (s *MetadataServer) handleForgeVendorData(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r.RemoteAddr)
	s.logger.Info("forge nocloud vendor-data request (returning empty)", "ip", ip)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
}

// handlePhoneHome accepts cloud-init phone_home POST callbacks and records
// the event in the database. When PhoneHomeEnabled is set on the instance,
// the engine uses this to gate readiness instead of SSH probe.
func (s *MetadataServer) handlePhoneHome(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r.RemoteAddr)
	_, instanceID, ok := s.lookupInstance(ip)
	s.logger.Info("cloud-init phone_home received", "instanceID", instanceID, "ip", ip, "method", r.Method)

	if ok && instanceID != "" {
		if err := s.store.MarkVMPhonedHome(context.Background(), instanceID); err != nil {
			s.logger.Error("failed to mark phone_home in database", "instanceID", instanceID, "error", err)
		}
	}

	w.WriteHeader(http.StatusOK)
}

// handleForgeNoCloudDir serves the directory listing for /api/v0/cloud-init/.
func (s *MetadataServer) handleForgeNoCloudDir(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v0/cloud-init/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, "meta-data\nuser-data\nvendor-data\n")
}

// sourceIP extracts the IP address from a RemoteAddr string, stripping the port.
func sourceIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// parseMetaDataField parses simple "key: value" YAML and returns the value for the given key.
func parseMetaDataField(data, field string) (string, bool) {
	for line := range strings.SplitSeq(data, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if ok && strings.TrimSpace(key) == field {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// arpLookup reads /proc/net/arp to find the MAC address for an IP.
func arpLookup(ip string) string {
	data, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == ip {
			mac := strings.ToLower(fields[3])
			if mac != "00:00:00:00:00:00" {
				return mac
			}
		}
	}
	return ""
}

// ensure MetadataServer's store dependency is satisfied by *state.Store.
var _ InstanceLookup = (*state.Store)(nil)
