// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	// NicoCredentialsEnv names the variable that holds the path to the NICo
	// client credentials. It is read on the host that runs the bootstrap, and
	// the credentials never enter the site spec.
	NicoCredentialsEnv = "NVKECTL_NICO_CREDENTIALS"

	// DefaultNicoAPIName is the API name in the NICo API path that CAPNICo is
	// given, which the credentials file does not carry today.
	DefaultNicoAPIName = "forge"
)

// structuredCredentialKey matches a key of the OAuth client-credentials form,
// in a YAML, JSON or KEY=VALUE file.
var structuredCredentialKey = regexp.MustCompile(`(?im)(^|[{,])\s*"?(tokenURL|clientID|clientSecret)"?\s*[:=]`)

// NicoCredentialsPath returns the path NVKECTL_NICO_CREDENTIALS names, or "".
func NicoCredentialsPath() string {
	return os.Getenv(NicoCredentialsEnv)
}

// ReadNicoToken returns the NICo bearer token from the credentials file at
// path. The format is told from the content: a file with tokenURL, clientID or
// clientSecret is the client-credentials form, which is refused for now because
// CAPNICo's credentials Secret takes only a bearer token; anything else is the
// token itself, and must be one value with no whitespace inside it.
func ReadNicoToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := strings.TrimSpace(string(data))
	switch {
	case content == "":
		return "", fmt.Errorf("%s is empty", path)
	case structuredCredentialKey.MatchString(content):
		return "", fmt.Errorf("%s holds tokenURL, clientID or clientSecret: the client-credentials form is not supported yet, "+
			"because the bootstrap CAPNICo's nico-credentials Secret takes only a bearer token; put the token in the file", path)
	case strings.ContainsAny(content, " \t\r\n"):
		return "", errors.New(path + " must hold only the NICo token, one value with no whitespace in it")
	}
	return content, nil
}
