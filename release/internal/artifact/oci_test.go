// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitReference(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		reference                 string
		host, repository, version string
		wantEr                    string
	}{
		{
			name: "a tag", reference: "nvcr.io/org/nvke-controller:v0.2.0-rc.1",
			host: "nvcr.io", repository: "org/nvke-controller", version: "v0.2.0-rc.1",
		},
		{
			name:       "a describe string, whose dashes are not separators",
			reference:  "nvcr.io/org/team/nvke-controller:v0.2.0-rc.1-7-g21afa494",
			host:       "nvcr.io",
			repository: "org/team/nvke-controller",
			version:    "v0.2.0-rc.1-7-g21afa494",
		},
		{
			name: "a digest", reference: "ghcr.io/dsx-ai-factory/nvke/nvkectl@sha256:abc",
			host: "ghcr.io", repository: "dsx-ai-factory/nvke/nvkectl", version: "sha256:abc",
		},
		{
			name:      "a registry with a port, whose colon is not the tag's",
			reference: "localhost:5000/nvke-controller:v1",
			host:      "localhost:5000", repository: "nvke-controller", version: "v1",
		},
		{name: "no repository", reference: "nvcr.io", wantEr: "names no registry host and repository"},
		{name: "no tag", reference: "nvcr.io/org/nvke-controller", wantEr: "names no tag or digest"},
		{name: "an empty tag", reference: "nvcr.io/org/nvke-controller:", wantEr: "names no tag or digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, repository, version, err := splitReference(tc.reference)
			if tc.wantEr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantEr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantEr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if host != tc.host || repository != tc.repository || version != tc.version {
				t.Errorf("= %q, %q, %q, want %q, %q, %q", host, repository, version, tc.host, tc.repository, tc.version)
			}
		})
	}
}

// A scope lists its actions separated by commas inside the quotes, so splitting
// the challenge on every comma would hand the token endpoint half a scope.
func TestParseChallenge(t *testing.T) {
	realm, params := parseChallenge(
		`Bearer realm="https://nvcr.io/proxy_auth",service="nvcr.io",scope="repository:org/nvke-controller:pull,push"`,
	)

	if realm != "https://nvcr.io/proxy_auth" {
		t.Errorf("realm = %q", realm)
	}
	if params["service"] != "nvcr.io" {
		t.Errorf("service = %q", params["service"])
	}
	if want := "repository:org/nvke-controller:pull,push"; params["scope"] != want {
		t.Errorf("scope = %q, want %q", params["scope"], want)
	}
}

// A challenge this does not understand yields no realm, and the caller says so
// rather than inventing a token endpoint.
func TestParseChallengeIgnoresOtherSchemes(t *testing.T) {
	if realm, _ := parseChallenge(`Basic realm="nvcr.io"`); realm != "" {
		t.Errorf("realm = %q, want none for a Basic challenge", realm)
	}
}

// registryStub answers like a registry that issues tokens: it challenges an
// unauthenticated manifest request, hands out a token, and then serves. It is
// TLS because a registry is reached over https and nothing here should make
// that configurable just to be testable.
func registryStub(t *testing.T, repository, published string) (host string, checker Checker, auth *string) {
	t.Helper()

	var base, carried string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			carried = r.Header.Get("Authorization")
			fmt.Fprint(w, `{"token":"a-token"}`)
		case r.Header.Get("Authorization") != "Bearer a-token":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer realm="%s/token",service="registry",scope="repository:%s:pull"`, base, repository,
			))
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/v2/"+repository+"/manifests/"+published:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	base = srv.URL
	return strings.TrimPrefix(srv.URL, "https://"), Checker{Client: srv.Client()}, &carried
}

func TestOCIExists(t *testing.T) {
	const repository = "org/nvke-controller"
	const published = "v0.2.0-rc.1-7-g21afa494"

	host, checker, _ := registryStub(t, repository, published)

	t.Run("it is published", func(t *testing.T) {
		if err := checker.Exists(context.Background(), schemeOCI+host+"/"+repository+":"+published); err != nil {
			t.Errorf("the manifest is there: %v", err)
		}
	})

	t.Run("a tag that was never pushed says so plainly", func(t *testing.T) {
		err := checker.Exists(context.Background(), schemeOCI+host+"/"+repository+":v9.9.9")
		if err == nil || !strings.Contains(err.Error(), "is not published") {
			t.Errorf("err = %v, want it to say the tag is not published", err)
		}
	})
}

// A public registry needs none, so credentials are offered and not required.
func TestOCIExistsOffersCredentials(t *testing.T) {
	const repository = "org/nvke-controller"
	const published = "v0.2.0-rc.1-7-g21afa494"

	host, anonymous, carried := registryStub(t, repository, published)
	reference := schemeOCI + host + "/" + repository + ":" + published

	if err := anonymous.Exists(context.Background(), reference); err != nil {
		t.Fatalf("anonymous: %v", err)
	}
	if *carried != "" {
		t.Errorf("the anonymous token request carried %q", *carried)
	}

	withAuth := anonymous
	withAuth.Auth = func(string) (string, string, bool) { return "user", "secret", true }
	if err := withAuth.Exists(context.Background(), reference); err != nil {
		t.Fatalf("with credentials: %v", err)
	}
	if !strings.HasPrefix(*carried, "Basic ") {
		t.Errorf("the token request carried %q, want basic credentials", *carried)
	}
}
