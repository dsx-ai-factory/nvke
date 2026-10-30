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
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

const (
	envDatabasePassword = "NVKECTL_DATABASE_PASSWORD" //nolint:gosec // G101: an env var name, not a credential
	envNicoToken        = "NVKECTL_NICO_TOKEN"        //nolint:gosec // G101: an env var name, not a credential
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

// CredentialsFromEnv reads the credentials for an install on the given
// target from the environment and the files it names. The seed's database password is
// generated. The error names every missing or unusable input, one per line,
// and carries no value. The secret values are registered with redact.
func CredentialsFromEnv(kind TargetKind, spire sitespec.Spire) (Credentials, error) {
	var c Credentials
	var problems []string
	switch kind {
	case Seed:
		c.DatabasePassword = rand.Text()
		path := sitespec.NicoCredentialsPath()
		if path == "" {
			problems = append(problems, sitespec.NicoCredentialsEnv+" is not set: it names the file that holds the NICo token")
		} else if token, err := sitespec.ReadNicoToken(path); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", sitespec.NicoCredentialsEnv, err))
		} else {
			c.NicoToken = token
		}
	case Site:
		c.DatabasePassword = strings.TrimSpace(envalias.Get(envDatabasePassword))
		if c.DatabasePassword == "" {
			problems = append(problems, envDatabasePassword+" is not set: PostgreSQL, Temporal and the NVKE controller read it")
		}
		c.NicoToken = strings.TrimSpace(envalias.Get(envNicoToken))
		if c.NicoToken == "" {
			problems = append(problems, envNicoToken+" is not set: CAPNICo reads it")
		}
	default:
		problems = append(problems, fmt.Sprintf("unknown install target %q", kind))
	}
	user, password, err := sitespec.NodeLoginFromEnv()
	if err != nil {
		problems = append(problems, err.Error())
	}
	c.NodeUser, c.NodePassword = user, password
	if spire.UpstreamCA {
		c.SpireCACert = readFile(&problems, sitespec.EnvSpireCACertFile, spire.CACertFile)
		c.SpireCAKey = readFile(&problems, sitespec.EnvSpireCAKeyFile, spire.CAKeyFile)
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

// readFile reads a file a variable names. A problem names the variable, never the content.
func readFile(problems *[]string, env, path string) []byte {
	if path == "" {
		*problems = append(*problems, env+" is not set")
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// The variable may hold a pasted key instead of a path; drop the path.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		*problems = append(*problems, fmt.Sprintf("%s: %v", env, err))
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		*problems = append(*problems, env+" names an empty file")
		return nil
	}
	return data
}
