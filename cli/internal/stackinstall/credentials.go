// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

// Credentials are the values the stack's Secrets carry.
type Credentials struct {
	DatabasePassword string
	NicoToken        string
	NodeUser         string
	NodePassword     string
	// SpireCACert and SpireCAKey are nil unless the site spec sets spire.upstreamCA.
	SpireCACert []byte
	SpireCAKey  []byte
}

// CredentialSources names where the credentials are read from. The stack
// config carries it. The node user is the only value in it; the rest are
// paths of files that hold one value each.
type CredentialSources struct {
	// DatabasePasswordFile is read on the site. The seed generates its password.
	DatabasePasswordFile string
	NicoTokenFile        string
	NodeUser             string
	NodePasswordFile     string
	// SpireCACertFile and SpireCAKeyFile are read when the site spec sets
	// spire.upstreamCA, and must be unset otherwise.
	SpireCACertFile string
	SpireCAKeyFile  string
}

// ReadCredentials reads the files src names for an install on the given
// target. The error names every missing or unusable input by its stack config
// key, one per line, and carries no value or path. The secret values are
// registered with redact.
func ReadCredentials(kind TargetKind, upstreamCA bool, src CredentialSources) (Credentials, error) {
	var c Credentials
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	switch kind {
	case Seed:
		c.DatabasePassword = rand.Text()
		if src.DatabasePasswordFile != "" {
			add("credentials.databasePasswordFile is not read on the seed; the seed generates its password")
		}
	case Site:
		c.DatabasePassword = readSecret(add, "credentials.databasePasswordFile", src.DatabasePasswordFile)
	default:
		add("unknown install target %q", kind)
	}
	if src.NicoTokenFile == "" {
		add("credentials.nicoTokenFile is not set: CAPNICo reads the token")
	} else if token, err := sitespec.ReadNicoToken(src.NicoTokenFile); err != nil {
		add("credentials.nicoTokenFile: %v", err)
	} else {
		c.NicoToken = token
	}
	// The controller renders the node login into every node's user data.
	c.NodeUser = strings.TrimSpace(src.NodeUser)
	switch {
	case c.NodeUser == "":
		add("credentials.nodeUser is not set")
	case !templateSafe(c.NodeUser):
		add("credentials.nodeUser %s", unsafeChar)
	}
	c.NodePassword = readSecret(add, "credentials.nodePasswordFile", src.NodePasswordFile)
	if c.NodePassword != "" && !templateSafe(c.NodePassword) {
		add("credentials.nodePasswordFile: the password %s", unsafeChar)
	}
	switch {
	case upstreamCA:
		c.SpireCACert = readFile(add, "credentials.spireCACertFile", src.SpireCACertFile)
		c.SpireCAKey = readFile(add, "credentials.spireCAKeyFile", src.SpireCAKeyFile)
	case src.SpireCACertFile != "" || src.SpireCAKeyFile != "":
		add("credentials.spireCACertFile and spireCAKeyFile are set, but the site spec does not set spire.upstreamCA")
	}
	if len(problems) > 0 {
		return Credentials{}, errors.New(strings.Join(problems, "\n"))
	}
	// Secret data is base64, so both forms are masked.
	for _, v := range []string{c.DatabasePassword, c.NicoToken, c.NodePassword, string(c.SpireCAKey)} {
		redact.RegisterSecret(v)
		redact.RegisterSecret(base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return c, nil
}

// readFile reads the file a stack config key names. A problem names the key,
// never the path or the content.
func readFile(add func(string, ...any), key, path string) []byte {
	if path == "" {
		add("%s is not set", key)
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// The key may hold a pasted secret instead of a path; drop the path.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		add("%s: %v", key, err)
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		add("%s names an empty file", key)
		return nil
	}
	return data
}

func readSecret(add func(string, ...any), key, path string) string {
	return strings.TrimSpace(string(readFile(add, key, path)))
}
