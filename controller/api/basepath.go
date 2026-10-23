// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

const (
	// BasePath is the StdHTTPServerOptions.BaseURL this service mounts the
	// generated router at, and so the prefix of every pattern the router
	// registers.
	//
	// It lives here, beside the router it prefixes, because more than one thing
	// has to spell it identically: internal/server passes it to
	// HandlerWithOptions, hack/gen-authz-actions builds the authorization
	// table's patterns from it, and the tests that build their own mux have to
	// match. Those agreed by copying the literal until this existed, and a
	// change to one of them turned every route into an unknown operation — a 403
	// on every request, with the tests still green because they carried the same
	// stale copy.
	BasePath = "/api"
)
