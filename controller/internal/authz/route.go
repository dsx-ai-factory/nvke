// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"errors"
	"net/http"
	"strings"
)

var (
	// ErrNoRoute is returned for a request no registered pattern matches: an
	// unknown path, or a method not registered for a known path. The mux would
	// answer 404 or 405, but the decision is taken here so that an unrouted
	// request is denied rather than merely unreachable.
	ErrNoRoute = errors.New("no route matches this request")

	// ErrUnknownOperation is returned for a route the mux serves and the
	// generated table does not know. Regenerating actions.gen.go is the fix;
	// denying is what happens until then.
	ErrUnknownOperation = errors.New("route is not in the generated operation table")
)

// Resolver turns a request into the action and resource it authorizes.
//
// It holds the mux the server built, and asks it to match. That is what lets the
// middleware sit outside the mux — where it sees every request, including routes
// registered directly rather than through the generated router — and still name
// the operation. http.Request.Pattern is not an alternative: it is empty until
// the mux dispatches, which is after this middleware has had to decide.
type Resolver struct {
	mux *http.ServeMux
}

// NewResolver returns a Resolver over mux. A nil mux is a wiring bug rather than
// a configuration one, so it is an error at construction and not a denial per
// request.
func NewResolver(mux *http.ServeMux) (*Resolver, error) {
	if mux == nil {
		return nil, errors.New("resolver requires the server mux")
	}
	return &Resolver{mux: mux}, nil
}

// Resolve returns the Query for r, minus the caller's identity, which the
// middleware fills in from the Principal.
func (rv *Resolver) Resolve(r *http.Request) (Query, error) {
	// Handler runs the match without serving, and returns the registered
	// pattern. The handler it also returns is deliberately discarded: this is a
	// lookup, not a dispatch.
	_, pattern := rv.mux.Handler(r)
	if pattern == "" {
		return Query{}, ErrNoRoute
	}

	op, ok := operations[pattern]
	if !ok {
		return Query{}, ErrUnknownOperation
	}

	q := Query{
		Action: op.Action,
		Method: r.Method,
	}

	if op.IDParam == "" {
		// A collection operation acts on the collection itself, so policy has
		// something to name rather than an empty resource.
		q.ResourceType = CollectionType
		q.ResourceID = op.Collection
		return q, nil
	}

	id := pathValue(pattern, r.URL.Path, op.IDParam)
	if id == "" {
		// The pattern has a wildcard, so a match means the segment was present
		// and non-empty. Reaching here means the pattern and the path disagree,
		// which is not something to guess through.
		return Query{}, ErrNoRoute
	}
	q.ResourceType = op.ResourceType
	q.ResourceID = id
	return q, nil
}

// pathValue recovers a wildcard's value by pairing the pattern's segments with
// the path's.
//
// http.Request.PathValue would do this, and does not work here: the mux sets the
// values as it dispatches, and this runs before that. Pairing is exact for the
// patterns this API registers, which use one wildcard per segment and no {name...}
// multi-segment wildcards; a differing segment count means no pairing exists.
func pathValue(pattern, path, param string) string {
	_, rest, ok := strings.Cut(pattern, " ")
	if !ok {
		return ""
	}
	patternSegments := strings.Split(strings.Trim(rest, "/"), "/")
	pathSegments := strings.Split(strings.Trim(path, "/"), "/")
	if len(patternSegments) != len(pathSegments) {
		return ""
	}
	for i, segment := range patternSegments {
		if segment == "{"+param+"}" {
			return pathSegments[i]
		}
	}
	return ""
}
