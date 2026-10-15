// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import "testing"

func TestEnvAuth(t *testing.T) {
	t.Setenv("TEST_ACTOR", "a-user")
	t.Setenv("TEST_TOKEN", "a-token")
	t.Setenv("TEST_EMPTY", "")

	auth := EnvAuth(
		// NGC fixes the username, so only the password comes from the
		// environment.
		Credential{Host: "nvcr.io", Username: "$oauthtoken", PasswordEnv: "TEST_TOKEN"},
		Credential{Host: "ghcr.io", UsernameEnv: "TEST_ACTOR", PasswordEnv: "TEST_TOKEN"},
		Credential{Host: "unset.example", UsernameEnv: "TEST_ACTOR", PasswordEnv: "TEST_EMPTY"},
	)

	for _, tc := range []struct {
		name               string
		host               string
		wantUser, wantPass string
		wantOK             bool
	}{
		{"a fixed username", "nvcr.io", "$oauthtoken", "a-token", true},
		{"a username from the environment", "ghcr.io", "a-user", "a-token", true},
		{"the host match ignores case", "GHCR.IO", "a-user", "a-token", true},
		{"a host with no entry is anonymous", "docker.io", "", "", false},
		{"an unset password is anonymous rather than half a credential", "unset.example", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, pass, ok := auth(tc.host)
			if ok != tc.wantOK || user != tc.wantUser || pass != tc.wantPass {
				t.Errorf("= %q, %q, %v, want %q, %q, %v", user, pass, ok, tc.wantUser, tc.wantPass, tc.wantOK)
			}
		})
	}
}
