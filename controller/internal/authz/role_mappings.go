// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	"github.com/cedar-policy/cedar-go/types"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	kjson "sigs.k8s.io/json"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

// roleMappingEntry is one entry of roleMappings.json: any caller from Issuer
// whose verified token matches the one signal set holds Grants.
//
// roleMappings.json is the only place a role is granted. roles.json declares the roles
// and how they nest; it is parsed straight into the Cedar entity store, and a
// mapping rule is not an entity. Nothing here ever reaches Cedar except the
// roles it resolves to.
type roleMappingEntry struct {
	// Issuer is the name of an issuer in AuthenticationConfiguration, the same
	// string Query.Issuer carries.
	Issuer string `json:"issuer"`

	// Exactly one of Claim, ServiceAccount and SPIFFE is set.
	Claim          *claimMatch          `json:"claim,omitempty"`
	ServiceAccount *serviceAccountMatch `json:"serviceAccount,omitempty"`
	SPIFFE         *spiffeMatch         `json:"spiffe,omitempty"`

	// Grants are the roles a matching caller holds, each written the way
	// roles.json writes a parent.
	Grants []entityID `json:"grants"`
}

// entityID is an entity reference the way roles.json writes one:
// {"type": ..., "id": ...}.
type entityID struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// claimMatch matches a top-level verified claim equal to Value, or an array of
// strings containing it.
type claimMatch struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// serviceAccountMatch is admit.serviceAccounts' selector shape. A nil Name
// matches every service account in Namespace. A present "name": null decodes
// to the same nil, so decodeRoleMappings refuses it.
type serviceAccountMatch struct {
	Namespace string  `json:"namespace"`
	Name      *string `json:"name,omitempty"`
}

// spiffeMatch matches a SPIFFE ID in TrustDomain whose path the Expression, a
// CEL bool, accepts.
type spiffeMatch struct {
	TrustDomain string `json:"trustDomain"`
	Expression  string `json:"expression"`
}

const (
	// audienceClaim is refused as a claim mapping's name. A caller chooses its
	// own audiences, through TokenRequest or a SPIFFE audience request, and a
	// token is accepted when any one of them matches, so an "aud" mapping
	// would let a caller grant itself a role by asking for one more audience.
	audienceClaim = "aud"
)

// roleMappings is roleMappings.json compiled for request time: entries grouped
// by issuer, SPIFFE expressions compiled, trust domains parsed, grants turned
// into entity UIDs. Immutable after load and safe for concurrent use. A nil
// *roleMappings is an empty one.
type roleMappings struct {
	byIssuer map[string][]compiledRoleMapping
	count    int
}

type compiledRoleMapping struct {
	// index is the entry's position in roleMappings.json, for a log line.
	index          int
	claim          *claimMatch
	serviceAccount *serviceAccountMatch
	spiffe         *compiledSPIFFEMatch
	// grants become the overlay principal's Cedar Parents when the entry matches.
	grants []types.EntityUID
}

type compiledSPIFFEMatch struct {
	trustDomain spiffeid.TrustDomain
	program     cel.Program
}

// issuerSet is the configured issuer names, from authn.Authenticator.IssuerNames,
// that a mapping entry's issuer is checked against.
type issuerSet map[string]struct{}

// newIssuerSet copies names into a set. Nil or empty is the empty set, under
// which any mapping entry fails the load.
func newIssuerSet(names []string) issuerSet {
	set := make(issuerSet, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// compileRoleMappings decodes and checks roleMappings.json against the roles
// entities declares, and compiles what request time needs. Every problem is
// reported, one error per entry-level finding, so an operator fixing the file
// sees the whole list. A decode failure is a single error and stops there.
//
// The mappings are returned whenever the file decoded, even with findings, so
// a caller can still count entries; a caller must not use them when errs is
// non-empty.
//
// Checking an entry's issuer against the configured issuers needs the
// authentication config, which this does not have; see checkIssuers.
func compileRoleMappings(raw []byte, entities types.EntityMap) (*roleMappings, []error) {
	var entries []roleMappingEntry
	if err := decodeRoleMappings(raw, &entries); err != nil {
		return nil, []error{err}
	}

	env, err := spiffeCELEnv()
	if err != nil {
		return nil, []error{fmt.Errorf("building the %s CEL environment: %w", roleMappingsFile, err)}
	}

	m := &roleMappings{byIssuer: map[string][]compiledRoleMapping{}, count: len(entries)}
	var errs []error
	for i, e := range entries {
		compiled, err := compileRoleMapping(env, e, entities)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d]: %w", roleMappingsFile, i, err))
			continue
		}
		compiled.index = i
		m.byIssuer[e.Issuer] = append(m.byIssuer[e.Issuer], compiled)
	}
	return m, errs
}

// decodeRoleMappings decodes strictly. encoding/json matches keys
// case-insensitively and lets a later duplicate win, so
// {"namespace": "ci", "name": "builder", "NAME": null} would load as the whole
// namespace. UnmarshalStrict reports unknown, case-mismatched and duplicate
// keys, and every one of those is fatal here, as in authn.Load: it still keeps
// the winning value, so ignoring the report would leave the widened rule in
// place.
func decodeRoleMappings(raw []byte, entries *[]roleMappingEntry) error {
	if trimmed := bytes.TrimSpace(raw); !bytes.HasPrefix(trimmed, []byte("[")) {
		return fmt.Errorf("parsing %s: the file must be a JSON array of mapping entries", roleMappingsFile)
	}
	strictErrs, err := kjson.UnmarshalStrict(raw, entries)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", roleMappingsFile, err)
	}
	if len(strictErrs) > 0 {
		for i, e := range strictErrs {
			strictErrs[i] = withGrantsHint(e)
		}
		return fmt.Errorf("parsing %s: %w", roleMappingsFile, errors.Join(strictErrs...))
	}
	return refuseNullServiceAccountNames(raw)
}

// withGrantsHint points an unknown parents key at grants, the key a mapping
// uses where roles.json writes "parents".
func withGrantsHint(err error) error {
	msg := err.Error()
	if strings.HasPrefix(msg, "unknown field ") && strings.HasSuffix(msg, `.parents"`) {
		return fmt.Errorf(`%w; the field is "grants"`, err)
	}
	return err
}

// refuseNullServiceAccountNames refuses a serviceAccount selector whose name
// is present but null, which would otherwise load as the whole namespace. The
// strict decode has already run, so every key here is spelled exactly and
// appears once.
func refuseNullServiceAccountNames(raw []byte) error {
	var keys []struct {
		ServiceAccount map[string]json.RawMessage `json:"serviceAccount"`
	}
	if err := kjson.UnmarshalCaseSensitivePreserveInts(raw, &keys); err != nil {
		return fmt.Errorf("parsing %s: %w", roleMappingsFile, err)
	}
	for i, k := range keys {
		if err := authn.RefuseNullServiceAccountName(k.ServiceAccount); err != nil {
			return fmt.Errorf("parsing %s: [%d]: serviceAccount: %w", roleMappingsFile, i, err)
		}
	}
	return nil
}

func compileRoleMapping(env *cel.Env, e roleMappingEntry, entities types.EntityMap) (compiledRoleMapping, error) {
	if strings.TrimSpace(e.Issuer) == "" {
		return compiledRoleMapping{}, errors.New("issuer is required and must not be empty")
	}
	if strings.TrimSpace(e.Issuer) != e.Issuer {
		return compiledRoleMapping{}, fmt.Errorf("issuer %q has leading or trailing whitespace", e.Issuer)
	}

	set := 0
	for _, present := range []bool{e.Claim != nil, e.ServiceAccount != nil, e.SPIFFE != nil} {
		if present {
			set++
		}
	}
	if set != 1 {
		return compiledRoleMapping{}, fmt.Errorf(
			"exactly one of claim, serviceAccount and spiffe must be set, found %d; write one entry per grant", set,
		)
	}

	var compiled compiledRoleMapping
	switch {
	case e.Claim != nil:
		if err := validateClaimMatch(e.Claim); err != nil {
			return compiledRoleMapping{}, fmt.Errorf("claim: %w", err)
		}
		compiled.claim = e.Claim
	case e.ServiceAccount != nil:
		if err := authn.ValidateServiceAccountSelector(e.ServiceAccount.Namespace, e.ServiceAccount.Name); err != nil {
			return compiledRoleMapping{}, fmt.Errorf("serviceAccount: %w", err)
		}
		compiled.serviceAccount = e.ServiceAccount
	default:
		spiffe, err := compileSPIFFEMatch(env, e.SPIFFE)
		if err != nil {
			return compiledRoleMapping{}, fmt.Errorf("spiffe: %w", err)
		}
		compiled.spiffe = spiffe
	}

	grants, err := declaredGrants(e.Grants, entities)
	if err != nil {
		return compiledRoleMapping{}, err
	}
	compiled.grants = grants
	return compiled, nil
}

func validateClaimMatch(c *claimMatch) error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name is required and must not be empty")
	}
	if strings.TrimSpace(c.Name) != c.Name {
		return fmt.Errorf("name %q has leading or trailing whitespace; it is used verbatim, so it would match nothing", c.Name)
	}
	if c.Name == audienceClaim {
		return errors.New(`name may not be "aud": a caller chooses its own audiences and a token is accepted ` +
			"when any one of them matches, so an aud mapping would let a caller grant itself a role by " +
			"requesting an extra audience")
	}
	if c.Value == "" {
		return errors.New("value is required and must not be empty; an empty value can never grant a role")
	}
	return nil
}

// compileSPIFFEMatch applies admission's exact trust-domain rules, then parses
// the domain and compiles the expression.
func compileSPIFFEMatch(env *cel.Env, s *spiffeMatch) (*compiledSPIFFEMatch, error) {
	if err := authn.ValidateTrustDomain(s.TrustDomain); err != nil {
		return nil, err
	}
	td, err := spiffeid.TrustDomainFromString(s.TrustDomain)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.Expression) == "" {
		return nil, errors.New("expression is required and must not be empty")
	}
	program, err := compileSPIFFEExpression(env, s.Expression)
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", s.Expression, err)
	}
	return &compiledSPIFFEMatch{trustDomain: td, program: program}, nil
}

// declaredGrants checks grants the way checkEntityStore checks a role's
// parents: non-empty, every one an NKE::Role that roles.json declares. A grant
// of an undeclared role fails the load, at startup and on reload, rather than
// matching and then being dropped at request time.
func declaredGrants(grants []entityID, entities types.EntityMap) ([]types.EntityUID, error) {
	if len(grants) == 0 {
		return nil, errors.New("grants is required and must name at least one role")
	}
	uids := make([]types.EntityUID, 0, len(grants))
	for i, g := range grants {
		if g.Type != RoleType {
			return nil, fmt.Errorf("grants[%d] has type %q; a mapping grants roles only, so it must be %s",
				i, g.Type, RoleType)
		}
		uid := types.NewEntityUID(RoleType, types.String(g.ID))
		if _, declared := entities.Get(uid); !declared {
			return nil, fmt.Errorf("grants[%d] names %s::%q, which %s does not declare", i, RoleType, g.ID, rolesFile)
		}
		uids = append(uids, uid)
	}
	return uids, nil
}

// checkIssuers refuses an entry whose issuer is not configured. It is the one
// mapping check that needs the authentication config, so it runs where the
// Local is built, at startup and on every reload, and not in ValidateBundle.
// An entry keyed on an unconfigured issuer would match nobody, silently, so it
// fails the load instead.
func (m *roleMappings) checkIssuers(issuers issuerSet) error {
	if m == nil {
		return nil
	}
	var unknown []string
	for name := range m.byIssuer {
		if _, ok := issuers[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	if len(issuers) == 0 {
		return fmt.Errorf("%s names issuer(s) %q, but authentication is disabled, so no issuer is configured; "+
			"configure authentication: authorization needs at least one mapping, and every mapping names a configured issuer",
			roleMappingsFile, unknown)
	}
	configured := make([]string, 0, len(issuers))
	for name := range issuers {
		configured = append(configured, name)
	}
	slices.Sort(configured)
	return fmt.Errorf("%s names issuer(s) %q, which the authentication config does not configure (configured: %q); "+
		"fix the name or add the issuer to AuthenticationConfiguration", roleMappingsFile, unknown, configured)
}

// len reports how many entries the file held. Nil-safe.
func (m *roleMappings) len() int {
	if m == nil {
		return 0
	}
	return m.count
}

// parentsFor resolves q's verified identity to the roles its issuer's mappings
// grant, unioned across every entry that matches. Nil-safe: no mappings grant
// nothing. A spiffe expression that fails to evaluate does not match, and is
// logged at debug level to logger when it is non-nil.
//
// Spiffe evaluation for one request shares spiffeCELRequestBudget. Once the
// actual cost spent reaches it, every remaining spiffe entry does not match,
// failing closed, and one warning names the issuer and the entry where
// evaluation stopped. Claim and service-account entries run no CEL and are
// always checked.
//
// It is the only reader of Query.Claims. It does string equality against
// values an operator wrote and nothing else; CEL never sees a claim, only the
// SPIFFE path. What it returns becomes principal parents and nothing more, so
// a claim never reaches the Cedar context or an entity attribute.
func (m *roleMappings) parentsFor(q Query, logger *slog.Logger) []types.EntityUID {
	if m == nil {
		return nil
	}
	var (
		parents   []types.EntityUID
		spent     uint64
		exhausted bool
	)
	for _, e := range m.byIssuer[q.Issuer] {
		if e.spiffe != nil && spent >= spiffeCELRequestBudget {
			if !exhausted && logger != nil {
				logger.Warn("the request exhausted its spiffe expression budget, so this and later spiffe entries do not match",
					"file", roleMappingsFile, "entry", e.index, "issuer", q.Issuer,
					"spent", spent, "budget", spiffeCELRequestBudget)
			}
			exhausted = true
			continue
		}
		matched, cost, err := e.matches(q)
		spent += cost
		if err != nil && logger != nil {
			logger.Debug("a role mapping's spiffe expression failed to evaluate, so the entry does not match",
				"file", roleMappingsFile, "entry", e.index, "issuer", q.Issuer, "error", err)
		}
		if matched {
			parents = append(parents, e.grants...)
		}
	}
	return parents
}

// matches reports whether q matches the entry, and the CEL cost spent finding
// out, which is zero for everything but a spiffe entry. The error is a spiffe
// expression's evaluation error, returned with false.
func (e compiledRoleMapping) matches(q Query) (bool, uint64, error) {
	switch {
	case e.claim != nil:
		return claimMatches(q.Claims, e.claim), 0, nil
	case e.serviceAccount != nil:
		return q.ServiceAccount != nil && q.ServiceAccount.Matches(e.serviceAccount.Namespace, e.serviceAccount.Name), 0, nil
	case e.spiffe != nil:
		return e.spiffe.matches(q.Principal)
	default:
		return false, 0, nil
	}
}

// claimMatches reports whether the claim is c.Value, or an array of strings
// containing it. An absent claim, any other JSON type, and an array holding a
// non-string element all fail to match; the mixed array fails as a whole
// rather than matching on the elements that happened to be strings.
func claimMatches(claims map[string]any, c *claimMatch) bool {
	value, ok := claims[c.Name]
	if !ok {
		return false
	}
	switch v := value.(type) {
	case string:
		return v == c.Value
	case []string:
		return slices.Contains(v, c.Value)
	case []any:
		found := false
		for _, elem := range v {
			s, ok := elem.(string)
			if !ok {
				return false
			}
			if s == c.Value {
				found = true
			}
		}
		return found
	default:
		return false
	}
}

// matches parses subject, requires it to be a member of the entry's trust
// domain, and only then runs the expression against its path. MemberOf
// compares parsed domains exactly, so spiffe://example.org.attacker.test/...
// is not in example.org; the path the expression sees has the domain
// stripped, so no expression can get this wrong. A subject that is not a
// member runs no CEL and costs nothing.
func (s *compiledSPIFFEMatch) matches(subject string) (bool, uint64, error) {
	id, err := spiffeid.FromString(subject)
	if err != nil {
		return false, 0, nil
	}
	if !id.MemberOf(s.trustDomain) {
		return false, 0, nil
	}
	return evalSPIFFEExpression(s.program, id.Path())
}
