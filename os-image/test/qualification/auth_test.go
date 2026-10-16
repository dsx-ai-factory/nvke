// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/dsxauth"
)

// TestDSXAuthAdapter exercises the shared exchange through the public adapter
// from the OS-image module, with a local issuer and no live credentials.
func TestDSXAuthAdapter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		datahall string
		prefix   string
		status   int
	}{
		{name: "redstone", datahall: "redstone-dev1-dh2", prefix: "DSX_REDSTONE_DEV1_DH2", status: http.StatusOK},
		{name: "another-datahall", datahall: "az51-dev2-dh2", prefix: "DSX_AZ51_DEV2_DH2", status: http.StatusOK},
		{name: "rejected-client", datahall: "redstone-dev1-dh2", prefix: "DSX_REDSTONE_DEV1_DH2", status: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.prefix+"_SSA_CLIENT_ID", "example-client-"+tc.name)
			t.Setenv(tc.prefix+"_SSA_CLIENT_SECRET", "example-secret")
			var requests atomic.Int32
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/token" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				id, secret, ok := r.BasicAuth()
				if !ok || id != "example-client-"+tc.name || secret != "example-secret" {
					t.Error("exchange did not use the selected datahall's credentials")
				}
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse exchange form: %v", err)
				}
				if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "carbide" {
					t.Error("exchange used an unexpected grant or scope")
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, `{"access_token":"example-token"}`)
			}))
			t.Cleanup(issuer.Close)
			t.Setenv("SSA_ISSUER", issuer.URL)

			token, err := dsxauth.MintToken(context.Background(), tc.datahall)
			if requests.Load() != 1 {
				t.Errorf("exchange requests = %d, want 1", requests.Load())
			}
			if tc.status == http.StatusOK {
				if err != nil || token != "example-token" {
					t.Fatalf("exchange failed or returned an unexpected token: %v", err)
				}
				return
			}
			if err == nil || token != "" {
				t.Fatal("rejected credentials should return an error and no token")
			}
			if strings.Contains(err.Error(), "example-secret") || strings.Contains(err.Error(), "example-token") {
				t.Fatal("exchange error exposed credentials or response content")
			}
		})
	}
}
