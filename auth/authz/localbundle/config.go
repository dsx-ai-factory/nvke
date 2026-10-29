// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package localbundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/cedar-policy/cedar-go/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
	"github.com/dsx-ai-factory/nvke/auth/internal/strictyaml"
)

// The only apiVersion and kind configFile may have. Both are required.
const (
	configAPIVersion = "auth.nvke.nvidia.com/v1alpha1"
	configKind       = "AuthorizationConfiguration"
)

// authorizationConfig is the decoded configFile. It never changes after load.
type authorizationConfig struct {
	metav1.TypeMeta `json:",inline"`

	// OperatorTenant is the operators' tenant. A caller in it is an operator
	// if its issuer sets Operators, and an impostor otherwise. Set while no
	// issuer sets Operators, it reserves the ID: every caller in it is refused.
	OperatorTenant string `json:"operatorTenant"`

	// Issuers maps an issuer name from the authN config to its settings. A
	// caller from an issuer not listed has no tenant and no roles.
	Issuers map[string]issuerConfig `json:"issuers"`
}

// issuerConfig says where one issuer's callers get their tenant, whether they
// can be operators, and which roles they hold.
type issuerConfig struct {
	Tenant tenantSource `json:"tenant"`

	// Operators makes this issuer's callers in OperatorTenant operators. Set
	// it only for an issuer whose callers can't choose their tenant.
	Operators bool `json:"operators"`

	// Claims and ServiceAccounts grant roles. A caller holds the roles of
	// every mapping it matches.
	Claims          []claimMapping          `json:"claims"`
	ServiceAccounts []serviceAccountMapping `json:"serviceAccounts"`
}

// tenantSource says where a caller's tenant comes from. Exactly one of Claim
// and Fixed is set.
type tenantSource struct {
	// Claim names a top-level claim holding the tenant ID as a string.
	// "dsx.nvidia.com/tenant" is one key, not a path.
	Claim string `json:"claim"`

	// Fixed gives every caller from the issuer this tenant.
	Fixed string `json:"fixed"`
}

// claimMapping matches a caller whose top-level claim Name equals Value, or
// is a string array holding it, and grants it Grants.
type claimMapping struct {
	Name   string   `json:"name"`
	Value  string   `json:"value"`
	Grants []string `json:"grants"`
}

// serviceAccountMapping matches a Kubernetes service account in Namespace,
// with Name if it's set, and grants it Grants.
type serviceAccountMapping struct {
	Namespace string             `json:"namespace"`
	Name      serviceAccountName `json:"name"`
	Grants    []string           `json:"grants"`
}

// serviceAccountName is a service account mapping's optional name. A *string
// can't tell an absent name from a null one, and both match every service
// account in the namespace. Null records a null name, so the load can refuse
// it: in YAML, `name:` with no value is null.
type serviceAccountName struct {
	// Value is nil for both an absent and a null name, so Null is the test.
	Value *string
	Null  bool
}

// UnmarshalJSON records whether the name is null, and decodes it as a string.
func (n *serviceAccountName) UnmarshalJSON(raw []byte) error {
	n.Null = string(raw) == "null"
	return json.Unmarshal(raw, &n.Value)
}

// rule returns m's match as the authN library's service account rule.
func (m serviceAccountMapping) rule() authn.ServiceAccountRule {
	return authn.ServiceAccountRule{Namespace: m.Namespace, Name: m.Name.Value}
}

// compiledConfig is a checked configFile in the form Authorize reads, built
// once at load. It never changes after load.
type compiledConfig struct {
	operatorTenant string
	issuers        map[string]compiledIssuer
}

// compiledIssuer is one issuer's settings, with each mapping's grants as role
// UIDs.
type compiledIssuer struct {
	tenant          tenantSource
	operators       bool
	claims          []claimMatch
	serviceAccounts []serviceAccountMatch
}

// claimMatch grants its roles to a caller whose claim name equals value, or is
// a string array holding it.
type claimMatch struct {
	name   string
	value  string
	grants []types.EntityUID
}

// serviceAccountMatch grants its roles to a service account that rule matches.
type serviceAccountMatch struct {
	rule   authn.ServiceAccountRule
	grants []types.EntityUID
}

// loadConfig decodes raw strictly, then checks it against the authN config's
// issuers and, unless declared is nil, the declared roles. It returns the
// config compiled for Authorize. Unless raw doesn't parse or has the wrong
// apiVersion or kind, the error joins every problem.
func loadConfig(raw []byte, issuers []string, declared types.EntityMap) (compiledConfig, error) {
	var c authorizationConfig
	if err := strictyaml.Decode(configFile, raw, &c); err != nil {
		return compiledConfig{}, err
	}
	// Another apiVersion or kind can give these keys other meanings, so
	// checking them against this schema would report misleading problems.
	if errs := c.checkTypeMeta(); len(errs) > 0 {
		return compiledConfig{}, prefixErrors(errs)
	}

	var errs []error
	if len(c.Issuers) == 0 {
		errs = append(errs, errors.New("issuers is empty, so no caller would have a tenant or a role"))
	}
	if c.OperatorTenant != "" {
		if err := authz.CheckTenantID(c.OperatorTenant); err != nil {
			errs = append(errs, fmt.Errorf("operatorTenant %w", err))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Issuers)) {
		errs = append(errs, c.checkIssuer(name, issuers, declared)...)
	}
	if len(errs) > 0 {
		return compiledConfig{}, prefixErrors(errs)
	}
	return c.compile(), nil
}

// compile returns c in the form Authorize reads. c must pass loadConfig's
// checks.
func (c authorizationConfig) compile() compiledConfig {
	compiled := compiledConfig{operatorTenant: c.OperatorTenant, issuers: make(map[string]compiledIssuer, len(c.Issuers))}
	for name, i := range c.Issuers {
		issuer := compiledIssuer{tenant: i.Tenant, operators: i.Operators}
		for _, m := range i.Claims {
			issuer.claims = append(issuer.claims, claimMatch{name: m.Name, value: m.Value, grants: roleUIDs(m.Grants)})
		}
		for _, m := range i.ServiceAccounts {
			issuer.serviceAccounts = append(issuer.serviceAccounts, serviceAccountMatch{rule: m.rule(), grants: roleUIDs(m.Grants)})
		}
		compiled.issuers[name] = issuer
	}
	return compiled
}

// prefixErrors joins errs, each prefixed with configFile.
func prefixErrors(errs []error) error {
	for i, err := range errs {
		errs[i] = fmt.Errorf("%s: %w", configFile, err)
	}
	return errors.Join(errs...)
}

// checkTypeMeta refuses any apiVersion or kind but this build's, and names the
// accepted value in each message.
func (c authorizationConfig) checkTypeMeta() []error {
	var errs []error
	switch c.APIVersion {
	case configAPIVersion:
	case "":
		errs = append(errs, fmt.Errorf("apiVersion is required; this build accepts %q", configAPIVersion))
	default:
		errs = append(errs, fmt.Errorf("apiVersion %q is not supported; this build accepts %q", c.APIVersion, configAPIVersion))
	}
	switch c.Kind {
	case configKind:
	case "":
		errs = append(errs, fmt.Errorf("kind is required; this build accepts %q", configKind))
	default:
		errs = append(errs, fmt.Errorf("kind %q is not supported; this build accepts %q", c.Kind, configKind))
	}
	return errs
}

// checkIssuer checks one issuer's settings: the issuer is in the authN config,
// its tenant source is valid, its operator setting can take effect, and each
// claim and service account mapping is valid. Each error names its path.
func (c authorizationConfig) checkIssuer(name string, issuers []string, declared types.EntityMap) []error {
	i := c.Issuers[name]
	path := "issuers." + name
	var errs []error
	if !slices.Contains(issuers, name) {
		errs = append(errs, fmt.Errorf("%s: isn't an issuer in the authN config (%q)", path, issuers))
	}
	if err := checkTenantSource(i.Tenant); err != nil {
		errs = append(errs, fmt.Errorf("%s.tenant: %w", path, err))
	} else if err := checkOperators(i, c.OperatorTenant); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
	}
	for j, m := range i.Claims {
		mapping := fmt.Sprintf("%s.claims[%d]", path, j)
		if err := checkClaimMapping(m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", mapping, err))
		}
		errs = append(errs, checkGrants(mapping, m.Grants, declared)...)
	}
	for j, m := range i.ServiceAccounts {
		mapping := fmt.Sprintf("%s.serviceAccounts[%d]", path, j)
		if m.Name.Null {
			errs = append(errs, fmt.Errorf("%s: name is null; remove the key to match any service account "+
				"in the namespace, or set a name", mapping))
		} else if err := m.rule().Validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", mapping, err))
		}
		errs = append(errs, checkGrants(mapping, m.Grants, declared)...)
	}
	return errs
}

// checkTenantSource refuses a tenant source that would give no caller a
// tenant, or that the caller controls.
func checkTenantSource(src tenantSource) error {
	switch {
	case (src.Claim == "") == (src.Fixed == ""):
		return errors.New("set exactly one of claim and fixed")
	case src.Claim != "":
		if err := checkClaimName(src.Claim); err != nil {
			return fmt.Errorf("claim %w", err)
		}
	default:
		if err := authz.CheckTenantID(src.Fixed); err != nil {
			return fmt.Errorf("fixed %w", err)
		}
	}
	return nil
}

// checkOperators refuses an operator setting that makes every caller from the
// issuer an impostor, or no caller an operator.
func checkOperators(i issuerConfig, operatorTenant string) error {
	fixed := i.Tenant.Fixed
	if !i.Operators {
		if fixed != "" && fixed == operatorTenant {
			return errors.New("tenant.fixed is operatorTenant, but operators isn't set, so every caller would be an impostor")
		}
		return nil
	}
	switch {
	case operatorTenant == "":
		return errors.New("operators is set, but operatorTenant isn't, so no caller would be an operator")
	case fixed != "" && fixed != operatorTenant:
		return fmt.Errorf("operators is set, but tenant.fixed %q isn't operatorTenant %q, so no caller would be an operator",
			fixed, operatorTenant)
	default:
		return nil
	}
}

// checkClaimMapping refuses a claim mapping that would match no token, or
// whose claim the caller controls.
func checkClaimMapping(m claimMapping) error {
	if err := checkClaimName(m.Name); err != nil {
		return fmt.Errorf("name %w", err)
	}
	if err := checkTrimmed(m.Value); err != nil {
		return fmt.Errorf("value %w", err)
	}
	return nil
}

// checkClaimName refuses a claim name that would match no claim, or whose
// claim the caller chooses. The caller prefixes the error with the field. A
// caller chooses the audiences it asks for, and a token passes when any one
// matches. An OIDC caller chooses the nonce, and the issuer signs it
// unchanged. So either claim would let a caller grant itself a role or choose
// its tenant.
func checkClaimName(name string) error {
	switch {
	case name == "":
		return errors.New("is empty")
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("%q has leading or trailing whitespace, so it would match no claim", name)
	case name == "aud" || name == "nonce":
		return fmt.Errorf("can't be %q, because the caller chooses its value", name)
	default:
		return nil
	}
}

// checkTrimmed refuses an empty s, or one with leading or trailing whitespace.
// The caller prefixes the error with the field.
func checkTrimmed(s string) error {
	switch {
	case s == "":
		return errors.New("is empty")
	case strings.TrimSpace(s) != s:
		return fmt.Errorf("%q has leading or trailing whitespace", s)
	default:
		return nil
	}
}

// checkGrants refuses an empty grants list, an empty or padded grant, and a
// grant that isn't a declared role. It skips the last check when declared is
// nil, because roles.json didn't load. mapping is the mapping's path.
func checkGrants(mapping string, grants []string, declared types.EntityMap) []error {
	if len(grants) == 0 {
		return []error{fmt.Errorf("%s: grants is empty", mapping)}
	}
	var errs []error
	for i, g := range grants {
		if err := checkTrimmed(g); err != nil {
			errs = append(errs, fmt.Errorf("%s: grants[%d] %w", mapping, i, err))
			continue
		}
		if _, ok := declared[roleUID(g)]; declared != nil && !ok {
			errs = append(errs, fmt.Errorf("%s: grants[%d] %q isn't a role %s declares", mapping, i, g, rolesFile))
		}
	}
	return errs
}

// roleUID returns the Cedar entity UID of the role named name.
func roleUID(name string) types.EntityUID {
	return types.NewEntityUID(roleType, types.String(name))
}

// roleUIDs returns the Cedar entity UIDs of the roles named names.
func roleUIDs(names []string) []types.EntityUID {
	uids := make([]types.EntityUID, 0, len(names))
	for _, name := range names {
		uids = append(uids, roleUID(name))
	}
	return uids
}

// callerOf returns p's tenant and operator status, and OutcomeDeny if p is an
// impostor. Left to the isolation check, an impostor would reach the operator
// tenant's objects as a member. A claim that isn't a string, such as a list,
// gives no tenant: a caller has one tenant. A malformed tenant is returned
// raw, so the isolation check refuses it even for a global resource.
func (c compiledConfig) callerOf(p authn.Principal) (authz.Caller, authz.Outcome) {
	i := c.issuers[p.IssuerName] // An unlisted issuer has no tenant source.
	tenant := i.tenant.Fixed
	if i.tenant.Claim != "" {
		tenant, _ = p.Claims[i.tenant.Claim].(string)
	}
	switch {
	case tenant == "" || tenant != c.operatorTenant:
		return authz.Caller{Tenant: tenant}, ""
	case !i.operators:
		return authz.Caller{Tenant: tenant}, authz.OutcomeDeny
	default:
		return authz.Caller{Tenant: tenant, Operator: true}, ""
	}
}

// rolesFor returns the roles that every matching claim and service account
// mapping of p's issuer grants. Claims decide which roles p holds, and never
// reach Cedar.
func (c compiledConfig) rolesFor(p authn.Principal) []types.EntityUID {
	var roles []types.EntityUID
	i := c.issuers[p.IssuerName]
	for _, m := range i.claims {
		if claimMatches(p.Claims, m.name, m.value) {
			roles = append(roles, m.grants...)
		}
	}
	// A token that isn't a service account's gives the zero ServiceAccount,
	// which no rule matches: Validate refuses an empty namespace.
	sa, _ := p.ServiceAccount()
	for _, m := range i.serviceAccounts {
		if m.rule.Matches(sa) {
			roles = append(roles, m.grants...)
		}
	}
	return roles
}

// claimMatches reports whether the claim name equals value, or is a string
// array holding it. Verified claims decode JSON arrays to []any, and an array
// holding anything but strings doesn't match.
func claimMatches(claims map[string]any, name, value string) bool {
	switch v := claims[name].(type) {
	case string:
		return v == value
	case []any:
		found := false
		for _, elem := range v {
			s, ok := elem.(string)
			if !ok {
				return false
			}
			found = found || s == value
		}
		return found
	default:
		return false
	}
}
