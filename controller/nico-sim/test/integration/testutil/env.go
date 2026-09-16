// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides shared test helpers for nico-sim integration tests.
package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lmittmann/tint"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/nico-sim/internal/app"
	"github.com/NVIDIA/nke/controller/nico-sim/internal/seed"
)

// TestEnv wraps a running nico-sim app and httptest server for integration testing.
type TestEnv struct {
	T          *testing.T
	App        *app.App
	Cancel     context.CancelFunc
	Server     *httptest.Server
	HTTPClient *http.Client
}

// RuntimeProfile captures the backend name, parsed config, and timing parameters
// that govern how a test suite interacts with the nico-sim server.
type RuntimeProfile struct {
	Name         string
	Config       Config
	ReadyTimeout time.Duration
	PollInterval time.Duration
}

// NewTestEnv creates a TestEnv bound to the test's lifecycle (auto-closes on cleanup).
func NewTestEnv(t *testing.T, seedDoc seed.Document, profile RuntimeProfile) *TestEnv {
	t.Helper()

	env, err := NewDetachedTestEnv(seedDoc, profile)
	if err != nil {
		t.Fatalf("app.Setup failed: %v", err)
	}
	env.T = t
	t.Cleanup(env.Close)
	return env
}

// NewDetachedTestEnv creates a TestEnv not bound to any test—caller must Close() it.
func NewDetachedTestEnv(seedDoc seed.Document, profile RuntimeProfile) (*TestEnv, error) {
	cfg := profile.Config
	logger := slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{TimeFormat: time.TimeOnly}))
	runCtx, cancel := context.WithCancel(context.Background())

	application, err := app.Setup(runCtx, cfg, seedDoc, logger)
	if err != nil {
		cancel()
		return nil, err
	}

	env := &TestEnv{
		App:    application,
		Cancel: cancel,
	}
	env.Server = httptest.NewServer(application.Handler)
	env.HTTPClient = env.Server.Client()
	env.HTTPClient.Timeout = 3 * time.Second

	return env, nil
}

// WithT returns a shallow clone of the TestEnv bound to a different *testing.T.
func (e *TestEnv) WithT(t *testing.T) *TestEnv {
	cloned := *e
	cloned.T = t
	return &cloned
}

func (e *TestEnv) Close() {
	e.Server.Close()
	e.Cancel()
	e.App.Close()
}

func (e *TestEnv) Endpoint(suffix string) string {
	return fmt.Sprintf("/v2/org/novxwn552ssc/forge/%s", strings.TrimPrefix(suffix, "/"))
}

func (e *TestEnv) Do(method, endpoint string, payload interface{}) (int, []byte) {
	if e.T == nil {
		panic("TestEnv.Do called without test context; use env.WithT(t)")
	}
	e.T.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			e.T.Fatalf("json marshal failed: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.Server.URL+endpoint, body)
	if err != nil {
		e.T.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")

	res, err := e.HTTPClient.Do(req)
	if err != nil {
		e.T.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()

	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		e.T.Fatalf("read response body failed: %v", err)
	}
	return res.StatusCode, resBody
}

func (e *TestEnv) CreateInstance(req *forgesdk.InstanceCreateRequest) forgesdk.Instance {
	e.T.Helper()
	status, body := e.Do(http.MethodPost, e.Endpoint("instance"), req)
	if status != http.StatusOK {
		e.T.Fatalf("create instance %q: status = %d, want 200; body: %s", req.Name, status, string(body))
	}
	var inst forgesdk.Instance
	if err := json.Unmarshal(body, &inst); err != nil {
		e.T.Fatalf("unmarshal create response for %q: %v", req.Name, err)
	}
	return inst
}

func (e *TestEnv) DeleteInstance(instanceID string) {
	e.T.Helper()
	status, body := e.Do(http.MethodDelete, e.Endpoint("instance/"+instanceID), &forgesdk.InstanceDeleteRequest{})
	if status != http.StatusOK {
		e.T.Fatalf("delete instance %s: status = %d, want 200; body: %s", instanceID, status, string(body))
	}
}

func (e *TestEnv) PatchInstance(instanceID string, req *forgesdk.InstanceUpdateRequest) {
	e.T.Helper()
	status, body := e.Do(http.MethodPatch, e.Endpoint("instance/"+instanceID), req)
	if status != http.StatusOK {
		e.T.Fatalf("patch instance %s: status = %d, want 200; body: %s", instanceID, status, string(body))
	}
}

func (e *TestEnv) InstanceErrorReason(instanceID string) string {
	if e.T == nil {
		panic("TestEnv.InstanceErrorReason called without test context; use env.WithT(t)")
	}
	row, err := e.App.Store.GetInstance(context.Background(), instanceID)
	if err != nil {
		return fmt.Sprintf("failed to lookup instance error reason: %v", err)
	}
	if row.VM == nil || row.VM.ErrorReason == "" {
		return "<empty>"
	}
	return row.VM.ErrorReason
}
