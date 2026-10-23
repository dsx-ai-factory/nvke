// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxTenantBytes caps a tenant ID, so it can be logged as is.
	maxTenantBytes = 256
)

// TenantSource says where an issuer's tokens carry the caller's tenant. Set
// exactly one field.
type TenantSource struct {
	// Claim names a top-level claim holding the tenant ID as a string.
	// "dsx.nvidia.com/tenant" is one key, not a path.
	Claim string `json:"claim,omitempty"`

	// Fixed gives every caller from this issuer the same tenant.
	Fixed string `json:"fixed,omitempty"`
}

func (s *TenantSource) validate() error {
	switch {
	case s.Claim != "" && s.Fixed != "":
		return errors.New("must set only one of claim and fixed")
	case s.Claim != "":
		return requireUnpadded("claim", s.Claim)
	case s.Fixed != "":
		if err := requireUnpadded("fixed", s.Fixed); err != nil {
			return err
		}
		if err := checkTenantID(s.Fixed); err != nil {
			return fmt.Errorf("fixed %q %w", elideForLog(s.Fixed), err)
		}
		return nil
	default:
		return errors.New("must set claim or fixed; to give the issuer's callers no tenant, remove the tenant key")
	}
}

// tenantOf returns the caller's tenant, or "" if the claim is absent, not a
// string, or not a valid tenant ID. A caller with no tenant still
// authenticates, and authorization refuses it where an action needs a tenant.
// So a misspelled claim name gives an empty tenant, not a 401.
func (s *TenantSource) tenantOf(claims map[string]any) string {
	if s == nil {
		return ""
	}
	if s.Fixed != "" {
		return s.Fixed
	}
	value, ok := claims[s.Claim].(string)
	if !ok || checkTenantID(value) != nil {
		return ""
	}
	return value
}

// checkTenantID defines a valid tenant ID. Tenant IDs are opaque and compared
// byte for byte, so the rules only keep one safe to log and distinct from
// another:
//
//   - Non-empty, at most maxTenantBytes, valid UTF-8, printable and unpadded.
//     Printable rules out zero-width and bidirectional characters.
//   - No U+FFFD. encoding/json decodes invalid UTF-8 and lone surrogates to
//     it, so two different claims would decode to one tenant.
//   - No '"' or '\', which authorization's entity IDs refuse.
//
// The error is a bare phrase, so the caller can name the field.
func checkTenantID(id string) error {
	if id == "" {
		return errors.New("is empty")
	}
	if len(id) > maxTenantBytes {
		return fmt.Errorf("is %d bytes, longer than %d", len(id), maxTenantBytes)
	}
	if !utf8.ValidString(id) {
		return errors.New("is not valid UTF-8")
	}
	if i := strings.IndexFunc(id, func(r rune) bool {
		return !unicode.IsPrint(r) || r == utf8.RuneError || r == '"' || r == '\\'
	}); i >= 0 {
		return fmt.Errorf("holds a character a tenant ID cannot contain, at byte %d", i)
	}
	if id != strings.TrimSpace(id) {
		return errors.New("has leading or trailing whitespace")
	}
	return nil
}
