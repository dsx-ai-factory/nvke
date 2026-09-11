// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// mockLookup implements InstanceLookup for tests without a real database.
type mockLookup struct {
	ipToInstance  map[string]string
	macToInstance map[string]string
	phonedHome    map[string]bool
}

func (m *mockLookup) LookupInstanceByIP(_ context.Context, ip string) (string, bool) {
	id, ok := m.ipToInstance[ip]
	return id, ok
}

func (m *mockLookup) LookupInstanceByMAC(_ context.Context, mac string) (string, bool) {
	id, ok := m.macToInstance[mac]
	return id, ok
}

func (m *mockLookup) MarkVMPhonedHome(_ context.Context, instanceID string) error {
	m.phonedHome[instanceID] = true
	return nil
}

func TestMetadataServer(t *testing.T) {
	// Create temp directory with cloud-init files.
	dir := t.TempDir()
	instanceID := "test-instance-123"

	metaData := "instance-id: test-instance-123\nlocal-hostname: test-host\n"
	userData := "#cloud-config\npackages:\n  - curl\n"

	// Create instance subdirectory (DB-backed server uses workingDirBase/instanceID)
	instanceDir := filepath.Join(dir, instanceID)
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "meta-data"), []byte(metaData), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "user-data"), []byte(userData), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	lookup := &mockLookup{
		ipToInstance:  map[string]string{"10.0.0.1": instanceID},
		macToInstance: map[string]string{"02:00:00:00:00:01": instanceID},
		phonedHome:    map[string]bool{},
	}
	srv := NewMetadataServer(logger, lookup, dir)

	handler := srv.Handler()

	tests := []struct {
		name       string
		path       string
		remoteAddr string
		wantCode   int
		wantBody   string
	}{
		{
			name:       "instance-id via 2009-04-04",
			path:       "/2009-04-04/meta-data/instance-id",
			remoteAddr: "10.0.0.1:12345",
			wantCode:   http.StatusOK,
			wantBody:   "test-instance-123",
		},
		{
			name:       "local-hostname via 2009-04-04",
			path:       "/2009-04-04/meta-data/local-hostname",
			remoteAddr: "10.0.0.1:12345",
			wantCode:   http.StatusOK,
			wantBody:   "test-host",
		},
		{
			name:       "user-data via 2009-04-04",
			path:       "/2009-04-04/user-data",
			remoteAddr: "10.0.0.1:12345",
			wantCode:   http.StatusOK,
			wantBody:   userData,
		},
		{
			name:       "instance-id via latest",
			path:       "/latest/meta-data/instance-id",
			remoteAddr: "10.0.0.1:54321",
			wantCode:   http.StatusOK,
			wantBody:   "test-instance-123",
		},
		{
			name:       "local-hostname via latest",
			path:       "/latest/meta-data/local-hostname",
			remoteAddr: "10.0.0.1:54321",
			wantCode:   http.StatusOK,
			wantBody:   "test-host",
		},
		{
			name:       "user-data via latest",
			path:       "/latest/user-data",
			remoteAddr: "10.0.0.1:54321",
			wantCode:   http.StatusOK,
			wantBody:   userData,
		},
		{
			name:       "unknown IP returns 404",
			path:       "/latest/meta-data/instance-id",
			remoteAddr: "10.0.0.99:12345",
			wantCode:   http.StatusNotFound,
			wantBody:   "not found\n",
		},
		{
			name:       "unknown path returns 404",
			path:       "/latest/meta-data/unknown-field",
			remoteAddr: "10.0.0.1:12345",
			wantCode:   http.StatusNotFound,
			wantBody:   "not found\n",
		},
		// Forge-compatible NoCloud endpoints (source IP lookup).
		{
			name:       "forge nocloud meta-data",
			path:       "/api/v0/cloud-init/meta-data",
			remoteAddr: "10.0.0.1:12345",
			wantCode:   http.StatusOK,
			wantBody:   metaData,
		},
		{
			name:       "forge nocloud user-data",
			path:       "/api/v0/cloud-init/user-data",
			remoteAddr: "10.0.0.1:12345",
			wantCode:   http.StatusOK,
			wantBody:   userData,
		},
		{
			name:       "forge nocloud vendor-data (empty)",
			path:       "/api/v0/cloud-init/vendor-data",
			remoteAddr: "10.0.0.1:12345",
			wantCode:   http.StatusOK,
			wantBody:   "",
		},
		{
			name:       "forge nocloud directory listing",
			path:       "/api/v0/cloud-init/",
			remoteAddr: "10.0.0.1:12345",
			wantCode:   http.StatusOK,
			wantBody:   "meta-data\nuser-data\nvendor-data\n",
		},
		{
			name:       "forge nocloud unknown IP returns 404",
			path:       "/api/v0/cloud-init/meta-data",
			remoteAddr: "10.0.0.99:12345",
			wantCode:   http.StatusNotFound,
			wantBody:   "not found\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.RemoteAddr = tt.remoteAddr
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status code: got %d, want %d", rec.Code, tt.wantCode)
			}
			if rec.Body.String() != tt.wantBody {
				t.Errorf("body: got %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestMetadataServerPhoneHome(t *testing.T) {
	dir := t.TempDir()
	instanceID := "i-phone"

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	lookup := &mockLookup{
		ipToInstance:  map[string]string{"10.0.0.5": instanceID},
		macToInstance: map[string]string{},
		phonedHome:    map[string]bool{},
	}
	srv := NewMetadataServer(logger, lookup, dir)
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/latest/meta-data/phone_home", nil)
	req.RemoteAddr = "10.0.0.5:9999"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("phone_home: got status %d, want 200", rec.Code)
	}
	if !lookup.phonedHome[instanceID] {
		t.Fatal("phone_home did not mark instance as phoned home")
	}
}

func TestMetadataServerFileReadError(t *testing.T) {
	// Register with a directory that has no files.
	dir := t.TempDir()
	instanceID := "i-missing"

	// Create instance subdirectory (empty — no meta-data/user-data files)
	if err := os.MkdirAll(filepath.Join(dir, instanceID), 0o755); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	lookup := &mockLookup{
		ipToInstance:  map[string]string{"10.0.0.10": instanceID},
		macToInstance: map[string]string{},
		phonedHome:    map[string]bool{},
	}
	srv := NewMetadataServer(logger, lookup, dir)
	handler := srv.Handler()

	// meta-data file missing should return 500.
	req := httptest.NewRequest(http.MethodGet, "/latest/meta-data/instance-id", nil)
	req.RemoteAddr = "10.0.0.10:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("missing meta-data: got status %d, want 500", rec.Code)
	}

	// user-data file missing should return 500.
	req = httptest.NewRequest(http.MethodGet, "/latest/user-data", nil)
	req.RemoteAddr = "10.0.0.10:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("missing user-data: got status %d, want 500", rec.Code)
	}
}
