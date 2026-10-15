// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package artifact answers "is this published", for the two kinds of location a
// plan can name.
//
// Resolution answers what version a run qualifies and never whether it can be
// fetched, so this is the one place that asks. The planner uses it only when
// asked to, and the qualification lane uses it on every run, which is why both
// reach the same answers and the same explanations.
package artifact

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Scheme prefixes a location carries. A plan records a whole location, so what
// kind it is can be read off it rather than tracked beside it.
const (
	schemeOCI   = "oci://"
	schemeHTTPS = "https://"
	schemeHTTP  = "http://"
)

// DefaultTimeout bounds a single request. A check runs once per artifact and a
// run names a handful, so this is short enough to fail a wedged registry
// quickly and long enough for a cold CDN edge.
const DefaultTimeout = 20 * time.Second

// Checker asks whether a location holds something.
//
// The zero value works and is anonymous. A registry that needs credentials
// takes them through Auth, which is consulted once per registry host.
type Checker struct {
	// Client makes the requests. Nil means one with DefaultTimeout.
	Client *http.Client

	// Auth returns the username and password for a registry host, or false to
	// stay anonymous. Nil is anonymous everywhere.
	Auth func(host string) (username, password string, ok bool)
}

// Exists reports whether location holds an artifact, using an anonymous
// checker. It is the form the resolvers use, where there is nothing to
// authenticate with.
func Exists(ctx context.Context, location string) error {
	return Checker{}.Exists(ctx, location)
}

// Exists reports whether location holds an artifact.
//
// A nil error means the artifact is there. Any other outcome is an error that
// names the location, because a caller that cannot say which artifact was
// missing has told the reader nothing.
func (c Checker) Exists(ctx context.Context, location string) error {
	switch {
	case strings.HasPrefix(location, schemeOCI):
		return c.ociExists(ctx, strings.TrimPrefix(location, schemeOCI))
	case strings.HasPrefix(location, schemeHTTPS), strings.HasPrefix(location, schemeHTTP):
		return c.httpExists(ctx, location)
	default:
		return fmt.Errorf("%q names no scheme this understands, expected %s or %s: %w",
			location, schemeOCI, schemeHTTPS, ErrSettled)
	}
}

// ErrSettled marks a failure that asking again cannot change: credentials a
// registry rejects, a reference that is not a location. A caller waiting for
// something to be published stops on one of these rather than spending its
// whole budget re-reading the same answer.
//
// The opposite is not an assertion that the artifact is absent. A private
// bucket answers the same way for a missing object and a disallowed network,
// so "keep asking" is the honest reading of anything not marked here.
var ErrSettled = errors.New("asking again will not change this")

// IsScheme reports whether location is one Exists can check, so a caller can
// skip an entry that names no location rather than failing on it.
func IsScheme(location string) bool {
	for _, scheme := range []string{schemeOCI, schemeHTTPS, schemeHTTP} {
		if strings.HasPrefix(location, scheme) {
			return true
		}
	}
	return false
}

// httpExists asks for the URL with HEAD and requires a 200.
//
// The bucket is private and does not list, so a missing object answers 403 as
// well, and so does a request from a network that is not allowed. The check can
// confirm that something exists and cannot say that it does not, so a 403 names
// both explanations. Another status means neither of those, and says only what
// it was.
func (c Checker) httpExists(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return fmt.Errorf("could not ask whether %s exists: %w", url, err)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("could not confirm %s exists: %w", url, err)
	}
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		return fmt.Errorf("could not confirm %s exists: HTTP 403. A missing artifact and a network that is "+
			"not allowed both answer 403, so check both", url)
	default:
		return fmt.Errorf("could not confirm %s exists: HTTP %d", url, resp.StatusCode)
	}
}

func (c Checker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: DefaultTimeout}
}
