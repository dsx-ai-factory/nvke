// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package localbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cedar-policy/cedar-go/types"
	kjson "sigs.k8s.io/json"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

const (
	// audienceClaim can't be a role mapping's claim. A caller chooses the
	// audiences it asks for, and a token passes when any one of them
	// matches, so an aud mapping would let a caller grant itself a role.
	audienceClaim = "aud"
)

// roleMapping is one entry of roleMappingsFile. A caller from Issuer whose
// verified token matches the entry's Claim or ServiceAccount holds Grants.
type roleMapping struct {
	// Issuer is the name of an issuer in the authN config.
	Issuer string `json:"issuer"`

	// Exactly one of Claim and ServiceAccount is set.
	Claim          *claimMatch               `json:"claim,omitempty"`
	ServiceAccount *authn.ServiceAccountRule `json:"serviceAccount,omitempty"`

	// Grants are the roles a matching caller holds.
	Grants []entityRef `json:"grants"`
}

// claimMatch matches when the top-level claim Name equals Value, or is a
// string array holding it.
type claimMatch struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// roleMappings is roleMappingsFile compiled for request time, by issuer. It
// never changes after load.
type roleMappings map[string][]compiledMapping

type compiledMapping struct {
	claim          *claimMatch
	serviceAccount *authn.ServiceAccountRule
	grants         []types.EntityUID
}

// compileRoleMappings decodes raw and checks each role mapping against the
// declared roles and the authN config's issuers. The error joins every role
// mapping's problem.
func compileRoleMappings(raw []byte, entities types.EntityMap, issuers []string) (roleMappings, error) {
	var entries []roleMapping
	if err := decodeRoleMappings(raw, &entries); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s has no role mappings, so no caller would hold a role", roleMappingsFile)
	}
	m := roleMappings{}
	var errs []error
	for i, e := range entries {
		compiled, err := compileMapping(e, entities, issuers)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d]: %w", roleMappingsFile, i, err))
			continue
		}
		m[e.Issuer] = append(m[e.Issuer], compiled)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return m, nil
}

// decodeRoleMappings decodes strictly. encoding/json matches keys without
// regard to case and keeps the last of two duplicates, so
// {"name": "builder", "NAME": null} would match a whole namespace.
// UnmarshalStrict reports unknown, case-mismatched and duplicate keys.
func decodeRoleMappings(raw []byte, entries *[]roleMapping) error {
	strictErrs, err := kjson.UnmarshalStrict(raw, entries)
	if err == nil {
		err = errors.Join(strictErrs...)
	}
	if err != nil {
		return fmt.Errorf("parsing %s: %w", roleMappingsFile, err)
	}
	return refuseNullNames(raw)
}

// refuseNullNames refuses a serviceAccount whose name is present but null. A
// null name decodes to nil, which matches every service account in the
// namespace, so a null name would widen the role mapping to the whole
// namespace.
func refuseNullNames(raw []byte) error {
	var entries []struct {
		ServiceAccount map[string]json.RawMessage `json:"serviceAccount"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("parsing %s: %w", roleMappingsFile, err)
	}
	for i, e := range entries {
		if name, ok := e.ServiceAccount["name"]; ok && bytes.Equal(bytes.TrimSpace(name), []byte("null")) {
			return fmt.Errorf("%s[%d]: serviceAccount: name is null; remove the key to match any service account "+
				"in the namespace, or set a name", roleMappingsFile, i)
		}
	}
	return nil
}

// compileMapping checks one role mapping and compiles its match: the issuer
// is in the authN config, exactly one match is set and valid, and every grant
// is a declared role.
func compileMapping(e roleMapping, entities types.EntityMap, issuers []string) (compiledMapping, error) {
	if !slices.Contains(issuers, e.Issuer) {
		return compiledMapping{}, fmt.Errorf("issuer %q isn't in the authN config (%q)", e.Issuer, issuers)
	}

	var compiled compiledMapping
	switch {
	case (e.Claim == nil) == (e.ServiceAccount == nil):
		return compiledMapping{}, errors.New("set exactly one of claim and serviceAccount")
	case e.Claim != nil:
		if err := checkClaimMatch(e.Claim); err != nil {
			return compiledMapping{}, fmt.Errorf("claim: %w", err)
		}
		compiled.claim = e.Claim
	default:
		if err := e.ServiceAccount.Validate(); err != nil {
			return compiledMapping{}, fmt.Errorf("serviceAccount: %w", err)
		}
		compiled.serviceAccount = e.ServiceAccount
	}

	if len(e.Grants) == 0 {
		return compiledMapping{}, errors.New("grants names no role")
	}
	for i, g := range e.Grants {
		uid := g.uid()
		if _, declared := entities[uid]; !declared || uid.Type != roleType {
			return compiledMapping{}, fmt.Errorf("grants[%d] is %s, which isn't a declared %s", i, uid, roleType)
		}
		compiled.grants = append(compiled.grants, uid)
	}
	return compiled, nil
}

// checkClaimMatch refuses a claim match that would match no token, or that
// the caller controls.
func checkClaimMatch(c *claimMatch) error {
	switch {
	case c.Name == "":
		return errors.New("name is empty")
	case strings.TrimSpace(c.Name) != c.Name:
		return fmt.Errorf("name %q has leading or trailing whitespace, so it would match no claim", c.Name)
	case c.Name == audienceClaim:
		return errors.New(`name can't be "aud": the caller chooses its audiences`)
	case c.Value == "":
		return errors.New("value is empty")
	default:
		return nil
	}
}

// rolesFor returns the roles that every matching role mapping for p's issuer
// grants. Claims decide which roles p holds, and never reach Cedar.
func (m roleMappings) rolesFor(p authn.Principal) []types.EntityUID {
	var roles []types.EntityUID
	// A token that isn't a service account's gives the zero ServiceAccount,
	// which no rule matches: Validate refuses an empty namespace.
	sa, _ := p.ServiceAccount()
	for _, e := range m[p.IssuerName] {
		if e.claim != nil && claimMatches(p.Claims, e.claim) ||
			e.serviceAccount != nil && e.serviceAccount.Matches(sa) {
			roles = append(roles, e.grants...)
		}
	}
	return roles
}

// claimMatches reports whether the claim equals c.Value, or is a string array
// holding it. Verified claims decode JSON arrays to []any, and an array
// holding anything but strings doesn't match.
func claimMatches(claims map[string]any, c *claimMatch) bool {
	switch v := claims[c.Name].(type) {
	case string:
		return v == c.Value
	case []any:
		found := false
		for _, elem := range v {
			s, ok := elem.(string)
			if !ok {
				return false
			}
			found = found || s == c.Value
		}
		return found
	default:
		return false
	}
}
