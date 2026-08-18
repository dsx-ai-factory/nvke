// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A path is exempt only if it is exempt under both the escaped form and the form
// http.ServeMux routes on. The attack cases below all reach a registered /api/
// handler because the mux unescapes each segment first, and checking only the
// escaped form would classify them as UI assets.
func TestIsAnonymousRequestPathAgreesWithTheMux(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		// Which letter is encoded and which asset extension it carries are both
		// inert: one encoded byte anywhere in "/api/" is the whole class.
		{"encoded a in api, .js suffix", "/%61pi/nodes/abc.js", false},
		// An encoded delimiter rather than an encoded letter: the escaped view
		// sees one opaque segment, the decoded view sees the API path.
		{"encoded slash is one segment, no route", "/api%2Fnodes/abc.js", false},

		// "%41PI" unescapes to "/API/" and ServeMux matches case-sensitively, so
		// this reaches the file server, not the API. Anonymous is correct.
		{"encoded but wrong case, so no API route", "/%41PI/nodes/abc.js", true},
		{"plain api path", "/api/nodes/abc", false},
		{"plain api path with asset suffix", "/api/nodes/abc.js", false},

		{"root", "/", true},
		{"openapi document", "/openapi.yaml", true},
		{"version", "/api/version", true},
		{"swagger subtree", "/api/docs/swagger-ui.css", true},
		{"liveness", "/healthz", true},
		{"readiness", "/readyz", true},
		{"ui asset", "/main.js", true},
		{"ui asset in a subdirectory", "/vendor/js-yaml.min.js", true},
		{"ui asset with an encoded space", "/my%20file.css", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Both views come off a real request, because net/http derives them
			// differently.
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, http.NoBody)
			got := IsAnonymousRequestPath(req.URL.EscapedPath(), req.URL.Path)
			if got != tc.want {
				t.Errorf("IsAnonymousRequestPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
