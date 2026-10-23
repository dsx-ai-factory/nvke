// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

var (
	// ErrNoRoute is returned for a request no registered pattern matches: an
	// unknown path, a method not registered for a known path, or a path the mux
	// will answer with a redirect rather than a handler. The mux would answer
	// 404, 405 or 301, but the decision is taken here so that a request no
	// handler serves is denied rather than merely unreachable.
	ErrNoRoute = errors.New("no route matches this request")

	// ErrUnknownOperation is returned for a route the mux serves and the
	// generated table does not know. Regenerating actions.gen.go is the fix;
	// denying is what happens until then.
	ErrUnknownOperation = errors.New("route is not in the generated operation table")

	// ErrAmbiguousPath is returned when a path's escaped and decoded views
	// disagree about what object is named — when unescaping the matched segment
	// a second time changes it, or fails. See Resolve.
	ErrAmbiguousPath = errors.New("the request path names one object escaped and another decoded")
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
//
// The whole of this function's difficulty is that the question has to be about
// the request the *handler* will serve. Sitting outside the mux is what makes
// the middleware complete — it sees routes the generated router's per-operation
// middleware never would — and it is also what makes it possible to describe a
// different request from the one that gets served, in three ways this now
// refuses to:
//
// The mux reports a matched pattern for a request it will only redirect, so a
// path with a doubled slash was authorized as an operation that never ran.
//
// The mux serves HEAD off a GET pattern, so the method in the query was HEAD
// while the action was the GET operation's — one question, two answers about
// what it was.
//
// And the identifier is percent-decoded twice on the way to the handler, once by
// the mux and once by the parameter binding, so DELETE on a cluster id spelled
// with a %25 prefix named one entity in the policy and deleted another.
func (rv *Resolver) Resolve(r *http.Request) (Query, error) {
	// Everything below reasons about the escaped path, because that is what the
	// mux matches on. r.URL.Path is already one decode further along, and the
	// difference between the two is where a request that names two objects
	// hides.
	escaped := r.URL.EscapedPath()
	if cleanPath(escaped) != escaped {
		// The mux answers a path that does not survive cleaning with a 301 and
		// runs no handler — while still reporting the pattern the cleaned path
		// would have matched. Authorizing that would record an allow for an
		// operation nobody performed. The redirected-to request is authorized
		// on its own when it arrives.
		return Query{}, ErrNoRoute
	}

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

	method, template, ok := splitPattern(pattern)
	if !ok {
		// Every key in the generated table is "METHOD /path". A pattern without
		// a method reaching here means the table and the router disagree about
		// what a pattern is, which is not something to guess through.
		return Query{}, ErrUnknownOperation
	}

	q := Query{
		// The matched pattern's method, not r.Method. http.ServeMux serves a
		// HEAD request off a GET pattern, so the two differ for every HEAD —
		// and the action, the action's group and the schema all come from the
		// pattern. A query carrying the request's method would say "HEAD" about
		// an action that is a GET, which is one question describing two
		// different requests. The request's own method is still logged, so a
		// HEAD is not indistinguishable from a GET in the audit record.
		//
		// This is also, deliberately, what an implicit HEAD's Cedar context.method
		// reads as inside a policy: "GET", not "HEAD" — Query.Method is what
		// authorizer.go puts under ContextKeyMethod. Worth naming explicitly
		// rather than leaving a future reader to discover it: a custom policy
		// that inspects context.method directly would see the matched route's
		// method, not the literal request method. The shipped bundle does not
		// inspect context.method in any policy today, so this does not change
		// current behavior — it documents a contract, not a bug.
		Action: op.Action,
		Method: method,
	}

	if op.IDParam == "" {
		// A collection operation acts on the collection itself, so policy has
		// something to name rather than an empty resource.
		q.ResourceType = CollectionType
		q.ResourceID = op.Collection
		return q, nil
	}

	id, err := resourceID(op, template, escaped)
	if err != nil {
		return Query{}, err
	}
	q.ResourceType = op.ResourceType
	q.ResourceID = id
	return q, nil
}

// resourceID recovers the object identifier from the escaped path and returns it
// in the form the handler will act on.
//
// Two decodes stand between the URL and the handler's argument: http.ServeMux
// unescapes the matched segment to produce r.PathValue, and the generated
// wrapper unescapes it again inside BindStyledParameterWithOptions before
// parsing. Rendering the entity from anything upstream of that second decode
// names an object the handler is not acting on.
func resourceID(op Operation, template, escapedPath string) (string, error) {
	segment, ok := matchedSegment(template, escapedPath, op.IDParam)
	if !ok {
		// The pattern has a wildcard, so a match means the segment was present.
		// Reaching here means the pattern and the path disagree, which is not
		// something to guess through.
		return "", ErrNoRoute
	}

	// What r.PathValue will return.
	bound, err := url.PathUnescape(segment)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAmbiguousPath, err)
	}

	// What the parameter binding will make of it. Equal to the above for every
	// identifier this API accepts, because none of them contains a percent
	// sign; unequal exactly when a caller escaped the escaping, which is the
	// one way to make the policy and the handler disagree about the object.
	// Refused rather than followed: which of the two is "the" identifier is not
	// a question with a safe default.
	again, err := url.PathUnescape(bound)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAmbiguousPath, err)
	}
	if again != bound {
		return "", fmt.Errorf("%w: %q decodes again to %q", ErrAmbiguousPath, bound, again)
	}

	return canonicalID(op, bound)
}

// splitPattern separates "GET /api/clusters/{clusterID}" into its method and its
// path template.
func splitPattern(pattern string) (string, string, bool) {
	method, template, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || template == "" {
		return "", "", false
	}
	return method, template, true
}

// matchedSegment recovers a wildcard's value from the escaped path by pairing
// the template's segments with the path's.
//
// http.Request.PathValue would do this, and does not work here: the mux sets the
// values as it dispatches, and this runs before that. Pairing is exact for the
// patterns this API registers, which use one wildcard per segment and no
// {name...} multi-segment wildcards; a differing segment count means no pairing
// exists.
//
// The path must be the escaped one, for the same reason the mux matches on it: a
// segment holding %2F is one segment to the mux and two to anything splitting
// the decoded path, which used to make a routed request look unrouted.
func matchedSegment(template, escapedPath, param string) (string, bool) {
	templateSegments := strings.Split(strings.Trim(template, "/"), "/")
	pathSegments := strings.Split(strings.Trim(escapedPath, "/"), "/")
	if len(templateSegments) != len(pathSegments) {
		return "", false
	}
	for i, segment := range templateSegments {
		if segment == "{"+param+"}" {
			if pathSegments[i] == "" {
				return "", false
			}
			return pathSegments[i], true
		}
	}
	return "", false
}

// cleanPath is net/http's, which is what decides whether the mux serves a
// request or redirects it. Copied rather than approximated: the two have to
// agree exactly, or this either denies a request the mux would have served or
// authorizes one it will not.
//
// One deliberate divergence. The mux does not clean a CONNECT request's path, so
// for CONNECT this is stricter than the mux is. It costs nothing: this API
// registers no CONNECT route, so such a request matches no pattern and is
// refused either way.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	cleaned := path.Clean(p)
	// path.Clean strips a trailing slash; the mux keeps one, because a trailing
	// slash is how a subtree pattern is spelled.
	if p[len(p)-1] == '/' && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}
