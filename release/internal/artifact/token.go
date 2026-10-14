// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// maxTokenBody bounds what a token response can be. A token is a few kilobytes
// and nothing here should read an unbounded body from a remote.
const maxTokenBody = 1 << 20

// decodeToken reads a token out of a registry's reply.
//
// The field is "token" in the distribution spec and "access_token" in the OAuth2
// form, and registries differ over which they send, so both are read and
// whichever arrives is used.
func decodeToken(resp *http.Response) (string, error) {
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenBody)).Decode(&body); err != nil {
		return "", fmt.Errorf("reading the token the registry returned: %w", err)
	}

	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("the registry returned no token")
}

// Credential says how to authenticate to one registry.
//
// A registry whose username is fixed carries it in Username, because NGC's is
// the literal "$oauthtoken" and reading it from the environment would be a
// variable nobody has a reason to set. One whose username varies, such as the
// GitHub actor, names the variable instead.
type Credential struct {
	// Host is the registry this is for, such as nvcr.io.
	Host string

	// Username is the literal username, for a registry that fixes one.
	Username string

	// UsernameEnv names the variable the username arrives in, for a registry
	// that does not. Ignored when Username is set.
	UsernameEnv string

	// PasswordEnv names the variable the password or token arrives in. A token
	// is never a literal here, so that nothing can commit one.
	PasswordEnv string
}

// EnvAuth returns an Auth that reads credentials from the environment.
//
// A host with no entry, and one whose variables are unset, stays anonymous. A
// public registry needs no credentials, and failing there would make the common
// case need secrets it has no use for. A private one answers 401 or 404 on its
// own, which is a better error than one invented here about a variable.
func EnvAuth(credentials ...Credential) func(string) (string, string, bool) {
	return func(host string) (string, string, bool) {
		for _, c := range credentials {
			if !strings.EqualFold(c.Host, host) {
				continue
			}

			username := c.Username
			if username == "" {
				username = os.Getenv(c.UsernameEnv)
			}
			password := os.Getenv(c.PasswordEnv)
			if username == "" || password == "" {
				return "", "", false
			}
			return username, password, true
		}
		return "", "", false
	}
}
