// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"path"
	"strings"
)

// The anonymous surface: every path not listed here needs a token.
//
// Maintained by hand, because http.ServeMux cannot be asked what was registered
// on it. The list can fall out of step with internal/server, but only safely: a
// route added there and missed here requires a token rather than skipping one.
const (
	// swaggerUIPrefix is the Swagger UI subtree. The handler serves its own CSS
	// and JavaScript from below this path, so the whole subtree must be anonymous
	// or the docs page does not render.
	swaggerUIPrefix = "/api/docs/"

	rootPath = "/"
)

// IsAnonymousRequestPath reports whether a request's path is exempt from
// authentication. escaped is r.URL.EscapedPath(); decoded is r.URL.Path.
//
// Both views must agree, because each catches what the other misses. path.Clean
// leaves "%2e%2e" alone, so "/api/docs/%2e%2e/nodes" keeps an exempt prefix
// escaped but cleans to "/api/nodes" decoded. The reverse holds for
// "/api/docs%2f..%2fversion", which reaches the exempt "/api/version" only once
// decoded.
//
// Clean also folds dot-segments and duplicate slashes and strips a trailing one,
// so "/api/docs/" and "/api/docs" need one entry between them.
func IsAnonymousRequestPath(escaped, decoded string) bool {
	return isAnonymousPath(normalizePath(escaped)) &&
		isAnonymousPath(normalizePath(decoded))
}

// normalizePath cleans one path into the form the exemption table is written in.
func normalizePath(p string) string {
	if p == "" {
		// path.Clean("") is ".", which no rule here should see.
		return rootPath
	}
	return path.Clean(p)
}

// isAnonymousPath takes a path from normalizePath. The exemption is by path, not
// by method: a POST to "/openapi.yaml" is exempt and then 404s.
func isAnonymousPath(p string) bool {
	switch p {
	// Read by the Swagger UI page and by client generators.
	case "/openapi.yaml",

		// Tilt points both probes at it, and the UI reads temporalUIURL and
		// temporalNamespace from it with no token.
		"/api/version",

		// The mux redirects this onto the subtree, but downstream of here, so
		// without an entry it is rejected before the browser is told to add the
		// slash.
		"/api/docs",

		// The note that says the Swagger UI moved.
		"/docs",

		// A probe that had to authenticate would make a key-source outage look
		// like a dead pod.
		"/healthz",
		"/readyz":
		return true
	}

	return strings.HasPrefix(p, swaggerUIPrefix)
}
