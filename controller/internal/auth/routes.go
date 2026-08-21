// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"path"
	"strings"
)

// The anonymous surface: every path not listed here needs a token.
//
// The list is maintained by hand. It has to be, because an http.ServeMux cannot
// be asked which patterns were registered on it, so there is no way to derive
// the exemptions from the routes they correspond to. It can therefore fall out
// of step with internal/server -- but only in the safe direction: a route added
// there and forgotten here starts requiring a token, rather than a protected one
// silently becoming anonymous.
const (
	// swaggerUIPrefix is the Swagger UI subtree. The handler serves its own CSS
	// and JavaScript from below its base path, so the whole subtree has to be
	// anonymous or the docs page renders unstyled and inert.
	swaggerUIPrefix = "/api/docs/"

	rootPath = "/"
)

// IsAnonymousRequestPath reports whether a request's path is exempt from
// authentication. escaped is r.URL.EscapedPath(); decoded is r.URL.Path.
//
// It exempts only if both views agree, because neither alone is sufficient.
//
// The decoded view catches an encoded dot-segment that climbs out of an exempt
// subtree: "/api/docs/%2e%2e/nodes" keeps its "/api/docs/" prefix through
// path.Clean, which leaves "%2e%2e" as an ordinary segment, so the escaped view
// alone would exempt it. Decoded, it cleans to "/api/nodes".
//
// The escaped view catches the opposite: "/api/docs%2f..%2fversion" survives
// path.Clean intact while the decoded view cleans it to "/api/version", which is
// exempt. Only the escaped view sees that the request never named an exempt path
// to begin with.
//
// path.Clean folds dot-segments and duplicate slashes, so "/api/nodes/../../docs"
// cannot dress an API path up as an exempt one. It also strips a trailing slash,
// which is why "/api/docs/" and "/api/docs" need one entry between them.
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
	// The OpenAPI document, read by the Swagger UI page and by client generators.
	case "/openapi.yaml",

		// The version endpoint. Tilt points both probes at it, and the UI reads
		// temporalUIURL and temporalNamespace from it without sending a token.
		"/api/version",

		// The Swagger UI. The mux 301s the unslashed form onto the subtree, but
		// that redirect happens downstream of this middleware, so "/api/docs"
		// would be rejected before the browser learned to add the slash.
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
