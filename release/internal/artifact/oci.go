// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// manifestTypes are the media types a manifest request accepts. An image
// pushed for two architectures answers with an index and a single-architecture
// one answers with a manifest, and a registry that is offered neither replies
// 404 to something that is there.
var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.artifact.manifest.v1+json",
}

// ociExists asks the registry for a manifest and requires a 200.
//
// reference is a whole one with its scheme already removed, such as
// "nvcr.io/org/nvke-controller:v0.2.0-rc.1-7-g21afa494".
func (c Checker) ociExists(ctx context.Context, reference string) error {
	host, repository, version, err := splitReference(reference)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repository, version)
	resp, err := c.manifest(ctx, endpoint, "")
	if err != nil {
		return fmt.Errorf("could not confirm %s exists: %w", reference, err)
	}

	// A registry that wants a token says so rather than refusing outright, and
	// the challenge names where to get one. Anonymous pulls take this path too,
	// so it is the usual case and not an error path.
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()

		token, err := c.token(ctx, host, challenge)
		if err != nil {
			return fmt.Errorf("could not confirm %s exists: %w", reference, err)
		}
		if resp, err = c.manifest(ctx, endpoint, token); err != nil {
			return fmt.Errorf("could not confirm %s exists: %w", reference, err)
		}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%s is not published: the registry has no %s under %s", reference, version, repository)
	case http.StatusUnauthorized, http.StatusForbidden:
		// Settled: the registry answers 404 for an artifact it does not have,
		// so being refused here is about the credentials rather than about
		// whether the publisher has finished.
		return fmt.Errorf("could not confirm %s exists: HTTP %d. Credentials that cannot read %s and an "+
			"artifact that is not there answer the same way, so check both: %w",
			reference, resp.StatusCode, repository, ErrSettled)
	default:
		return fmt.Errorf("could not confirm %s exists: HTTP %d", reference, resp.StatusCode)
	}
}

// manifest asks for one manifest, with a bearer token when there is one.
func (c Checker) manifest(ctx context.Context, endpoint, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", strings.Join(manifestTypes, ", "))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.client().Do(req)
}

// token answers a registry's challenge, with credentials when Auth has them for
// the host and anonymously when it does not. A registry that allows anonymous
// pulls still issues a token, so the anonymous path is not a fallback.
func (c Checker) token(ctx context.Context, host, challenge string) (string, error) {
	realm, params := parseChallenge(challenge)
	if realm == "" {
		return "", fmt.Errorf("the registry asked for authentication without saying where to get a token")
	}

	endpoint, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("the registry named %q as its token endpoint, which is not a URL", realm)
	}
	query := endpoint.Query()
	for _, key := range []string{"service", "scope"} {
		if value := params[key]; value != "" {
			query.Set(key, value)
		}
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	if c.Auth != nil {
		if username, password, ok := c.Auth(host); ok {
			req.SetBasicAuth(username, password)
		}
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("asking %s for a token: %w", endpoint.Host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("asking %s for a token: HTTP %d. A registry that needs credentials for %s "+
			"answers this way when none were given", endpoint.Host, resp.StatusCode, host)
	}
	return decodeToken(resp)
}

// parseChallenge reads realm, service and scope out of a Bearer challenge. A
// scheme other than Bearer yields no realm, and the caller reports that rather
// than guessing at an endpoint.
func parseChallenge(challenge string) (realm string, params map[string]string) {
	rest, ok := strings.CutPrefix(challenge, "Bearer ")
	if !ok {
		return "", nil
	}

	params = map[string]string{}
	for _, field := range splitFields(rest) {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		params[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return params["realm"], params
}

// splitFields splits a challenge on the commas that separate its fields,
// leaving the ones inside a quoted value alone. A scope lists several actions
// separated by commas, so splitting on every comma loses half of it.
func splitFields(s string) []string {
	var fields []string
	var start int
	var quoted bool

	for i, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			fields = append(fields, s[start:i])
			start = i + 1
		}
	}
	return append(fields, s[start:])
}

// splitReference takes a reference apart into the host, the repository and the
// version, which are the three the registry API asks for separately.
func splitReference(reference string) (host, repository, version string, err error) {
	host, rest, found := strings.Cut(reference, "/")
	if !found || host == "" || rest == "" {
		return "", "", "", fmt.Errorf("%q names no registry host and repository", reference)
	}

	// A digest separates with @ and a tag with the last colon, which cannot be
	// confused with a port because the host is already off the front.
	if repository, version, found = strings.Cut(rest, "@"); found {
		return host, repository, version, nil
	}
	index := strings.LastIndex(rest, ":")
	if index < 0 {
		return "", "", "", fmt.Errorf("%q names no tag or digest, so there is nothing to look for", reference)
	}
	repository, version = rest[:index], rest[index+1:]
	if repository == "" || version == "" {
		return "", "", "", fmt.Errorf("%q names no tag or digest, so there is nothing to look for", reference)
	}
	return host, repository, version, nil
}
