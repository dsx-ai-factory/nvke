// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

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

	// maxNameBytes caps a name in a Query. A SPIFFE ID, the longest accepted
	// subject, is at most 2048 bytes.
	maxNameBytes = 2048
)

// Caller is a caller's tenancy, as a backend resolves it.
type Caller struct {
	// Tenant is the caller's tenant ID. Empty if the caller has none.
	Tenant string

	// Operator lets the caller act in any tenant. The caller still needs a
	// valid tenant of its own.
	Operator bool
}

// CheckIsolation is the isolation check every backend shares. A backend calls
// it before any policy, so no policy can allow a cross-tenant request. It
// returns the refusing outcome, or "" if the request may go on to policy:
//
//   - A caller whose tenant ID is set and invalid gets OutcomeNoTenant, even
//     for a global resource, so a malformed ID never reaches policy.
//   - ScopeGlobal needs no tenant.
//   - A caller with no tenant gets OutcomeNoTenant.
//   - An operator may act in any tenant.
//   - Anyone else gets OutcomeNotFound for an object or create in another
//     tenant, compared byte for byte.
//
// It also returns OutcomeNotFound for a Resource whose Tenant doesn't fit its
// Scope, so a backend called without the Client still refuses it.
func CheckIsolation(c Caller, r Resource) Outcome {
	switch {
	case r.validateScope() != nil:
		return OutcomeNotFound
	case c.Tenant != "" && CheckTenantID(c.Tenant) != nil:
		return OutcomeNoTenant
	case r.Scope == ScopeGlobal:
		return ""
	case c.Tenant == "":
		return OutcomeNoTenant
	case c.Operator:
		return ""
	case r.Tenant != "" && r.Tenant != c.Tenant:
		return OutcomeNotFound
	default:
		return ""
	}
}

// CheckTenantID holds the tenant ID rules every backend shares. Tenant IDs are
// opaque and compared byte for byte, so the rules only keep an ID safe to log.
// The rules don't catch every look-alike, such as a Cyrillic letter in place
// of a Latin one. Two such IDs stay distinct tenants; only a log reader can
// confuse them.
//
// The error is a bare phrase, so the caller can name the field.
func CheckTenantID(id string) error {
	return checkString(id, maxTenantBytes)
}

// checkString holds the rules for a tenant ID or a name in a Query:
//
//   - Non-empty, at most maxBytes and valid UTF-8.
//   - Printable (unicode.IsPrint), which rules out control and format
//     characters, such as zero-width spaces and bidi overrides, and every
//     space but U+0020.
//   - No U+FFFD. encoding/json decodes invalid UTF-8 and lone surrogates to
//     it, so two different claims decode to one value.
//   - No '"' or '\', so a value can sit unescaped in a quoted log field or a
//     backend's entity ID.
//   - No leading or trailing whitespace.
func checkString(s string, maxBytes int) error {
	if s == "" {
		return errors.New("is empty")
	}
	if len(s) > maxBytes {
		return fmt.Errorf("is %d bytes, longer than %d", len(s), maxBytes)
	}
	// Check UTF-8 before scanning runes: IndexFunc decodes an invalid byte to
	// U+FFFD, and the scan would report the wrong cause.
	if !utf8.ValidString(s) {
		return errors.New("is not valid UTF-8")
	}
	if i := strings.IndexFunc(s, func(r rune) bool {
		return !unicode.IsPrint(r) || r == utf8.RuneError || r == '"' || r == '\\'
	}); i >= 0 {
		r, _ := utf8.DecodeRuneInString(s[i:])
		return fmt.Errorf("holds %U at byte %d", r, i)
	}
	if s != strings.TrimSpace(s) {
		return errors.New("has leading or trailing whitespace")
	}
	return nil
}
