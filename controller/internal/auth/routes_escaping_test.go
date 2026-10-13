// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A path is exempt only if it is exempt under both the escaped form and the form
// http.ServeMux routes on. Each view catches a class the other misses, so both
// classes are represented below; drop either half of the check and one of these
// cases goes red.
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
		// this reaches no route at all: it authenticates and then 404s.
		{"encoded but wrong case, so no API route", "/%41PI/nodes/abc.js", false},
		{"plain api path", "/api/nodes/abc", false},
		{"plain api path with asset suffix", "/api/nodes/abc.js", false},

		// Nothing is registered at "/".
		{"root", "/", false},
		{"openapi document", "/openapi.yaml", true},
		{"public openapi document", "/openapi-public.yaml", true},
		{"version", "/api/version", true},
		{"swagger subtree", "/api/docs/swagger-ui.css", true},
		// The escaped view alone would exempt this: path.Clean leaves "%2e%2e" as
		// an ordinary segment, so the "/api/docs/" prefix survives. Only the
		// decoded view sees that it cleans to "/api/nodes". The mirror case, which
		// only the escaped view catches, is in middleware_test.go.
		{"encoded dot-segment climbing out of the swagger subtree", "/api/docs/%2e%2e/nodes", false},
		{"liveness", "/healthz", true},
		{"readiness", "/readyz", true},
		// No UI is served here, so an asset-looking path is just an unrouted path:
		// it needs a token and then 404s, rather than being exempt.
		{"asset-looking path", "/main.js", false},
		{"asset-looking path in a subdirectory", "/vendor/js-yaml.min.js", false},
		{"asset-looking path with an encoded space", "/my%20file.css", false},
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
