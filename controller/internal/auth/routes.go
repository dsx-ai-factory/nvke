// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package auth

import (
	"path"
	"strings"
)

// The anonymous surface. Deny by default: every path not named here needs a
// token. The "/" entry means exactly "/" and the UI's own assets, never a prefix
// of "/", because the server registers http.FileServer under "/" and treating it
// as a prefix would make the whole API anonymous. The table is written out
// because http.ServeMux is not enumerable.
const (
	// apiPrefix is the base of every generated operation, so nothing under it can
	// qualify as a UI asset.
	apiPrefix = "/api/"

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
// http.ServeMux unescapes each segment before matching, so "/%61pi/nodes/x.js"
// reaches the "/api/nodes/{id}" handler while the escaped view sees no "/api/"
// prefix and exempts it as a UI asset. The decoded view catches that.
//
// The escaped view catches the opposite: an encoded dot-segment such as
// "/api/docs%2f..%2fversion" survives path.Clean intact while the decoded view
// cleans it to "/api/version", which is exempt. Only the escaped view sees that
// the request never named an exempt path to begin with.
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
		"/readyz",

		// The UI index. Exactly this path, see above.
		rootPath:
		return true
	}

	if strings.HasPrefix(p, swaggerUIPrefix) {
		return true
	}

	return isUIAsset(p)
}

// isUIAsset reports whether a path is a static asset of the bundled UI, which
// sends no token. Matched by extension rather than by file name, so adding a
// stylesheet does not break the console; an API path can never acquire one,
// because this refuses apiPrefix outright. A match makes the path anonymous, not
// existent: the file server still 404s.
func isUIAsset(p string) bool {
	if strings.HasPrefix(p, apiPrefix) {
		return false
	}

	switch path.Ext(p) {
	case ".css", ".html", ".ico", ".js", ".map", ".png", ".svg", ".woff", ".woff2":
		return true
	default:
		return false
	}
}
