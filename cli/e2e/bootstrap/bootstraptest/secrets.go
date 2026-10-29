// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// SecretSet holds secret values a run must not print or save, to search for as
// exact strings. It is safe for concurrent use.
//
// Nothing a SecretSet returns or reports holds a value: its output goes to a
// public CI log.
type SecretSet struct {
	mu     sync.Mutex
	values []string
}

// Add adds value to the set. An empty value is ignored, because it would match
// every file, and so is one already in the set.
func (s *SecretSet) Add(value string) {
	if value == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.values {
		if v == value {
			return
		}
	}
	s.values = append(s.values, value)
}

// Len reports how many values the set holds.
func (s *SecretSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.values)
}

// FindIn returns the files under paths that hold a value of the set, sorted. A
// path may be a file or a directory, which is walked. A path that does not exist
// is skipped, and any other read error is returned, because a file that was not
// searched proves nothing.
func (s *SecretSet) FindIn(paths ...string) ([]string, error) {
	needles := s.snapshot()
	var hits []string
	for _, root := range paths {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, needle := range needles {
				if bytes.Contains(content, needle) {
					hits = append(hits, path)
					return nil
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("searching %s for the known secret values: %w", root, err)
		}
	}
	sort.Strings(hits)
	return hits, nil
}

func (s *SecretSet) snapshot() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	needles := make([][]byte, len(s.values))
	for i, v := range s.values {
		needles[i] = []byte(v)
	}
	return needles
}

// WriteFile saves the set to path, readable by its owner only, so a later
// process can search for the same values. It replaces what is there.
func (s *SecretSet) WriteFile(path string) error {
	s.mu.Lock()
	data, err := json.Marshal(s.values)
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("encoding the known secret values: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing the known secret values to %s: %w", path, err)
	}
	return nil
}

// LoadSecretSet reads a set saved by WriteFile.
func LoadSecretSet(path string) (*SecretSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the known secret values from %s: %w", path, err)
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("decoding the known secret values in %s: %w", path, err)
	}
	set := &SecretSet{}
	for _, v := range values {
		set.Add(v)
	}
	return set, nil
}

// OTLPSettings are the OTLP export settings a bootstrap runs with, for checking
// that the header credential stays out of its output.
type OTLPSettings struct {
	// Endpoint is where nvkectl sends telemetry.
	Endpoint string
	// Token is the credential in the authorization header.
	Token string
	// Tenant is the value of the x-tenant header, which holds no credential in
	// its name.
	Tenant string
}

// otlpEndpoint is a documentation address: nothing answers, and it is not
// loopback, so bootstrap site passes the headers on to the controller.
const otlpEndpoint = "http://192.0.2.1:4317"

// NewOTLPSettings returns settings with a random token and tenant, so no
// credential-shaped value sits in the repository.
func NewOTLPSettings() (OTLPSettings, error) {
	token, err := randomValue()
	if err != nil {
		return OTLPSettings{}, err
	}
	tenant, err := randomValue()
	if err != nil {
		return OTLPSettings{}, err
	}
	return OTLPSettings{Endpoint: otlpEndpoint, Token: token, Tenant: tenant}, nil
}

func randomValue() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a random value: %w", err)
	}
	return "e2e" + hex.EncodeToString(b), nil
}

// Headers returns the OTEL_EXPORTER_OTLP_HEADERS value nvkectl is given.
func (o OTLPSettings) Headers() string {
	return "authorization=Bearer " + o.Token + ",x-tenant=" + o.Tenant
}

// ControllerHeaders returns the value the controller's Deployment holds after
// nvkectl passes Headers on: the space is percent-encoded.
func (o OTLPSettings) ControllerHeaders() string {
	return "authorization=Bearer%20" + o.Token + ",x-tenant=" + o.Tenant
}

// Env returns the settings as the environment nvkectl reads them from.
func (o OTLPSettings) Env() []string {
	return []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + o.Endpoint,
		"OTEL_EXPORTER_OTLP_HEADERS=" + o.Headers(),
	}
}

// Secrets returns the values to search the output for. The token is searched
// alone, so its encoded form in the cluster is found too. nvkectl masks the
// tenant header only as the pair x-tenant=<value>, so that pair is searched.
func (o OTLPSettings) Secrets() []string {
	return []string{o.Token, "x-tenant=" + o.Tenant}
}
