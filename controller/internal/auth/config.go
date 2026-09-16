// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	yamlv3 "gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// ErrEmptyPath is returned only when path is empty. A set-but-unreadable path is
// a startup failure, never "disabled".
var ErrEmptyPath = errors.New("authentication config path is empty")

// The only apiVersion and kind this build accepts. Both are required, never
// defaulted.
const (
	ConfigAPIVersion = "auth.nke.nvidia.com/v1alpha1"
	ConfigKind       = "AuthenticationConfiguration"
)

// allowedAlgorithms returns the permitted JWS algorithms, asymmetric only. Taken
// from golang-jwt rather than written out: jwt.WithValidMethods matches these
// byte-for-byte against the token's "alg" header, so the library doing the
// matching is the right source for the names.
func allowedAlgorithms() []string {
	methods := []jwt.SigningMethod{
		jwt.SigningMethodES256, jwt.SigningMethodES384, jwt.SigningMethodES512,
		jwt.SigningMethodEdDSA,
		jwt.SigningMethodPS256, jwt.SigningMethodPS384, jwt.SigningMethodPS512,
		jwt.SigningMethodRS256, jwt.SigningMethodRS384, jwt.SigningMethodRS512,
	}
	algs := make([]string, 0, len(methods))
	for _, m := range methods {
		algs = append(algs, m.Alg())
	}
	return algs
}

// Config is the file at AUTH_CONFIG_PATH, loaded once at startup.
type Config struct {
	metav1.TypeMeta `json:",inline"`

	// Audiences is the deployment-wide "aud" allow-list, required and non-empty
	// even when every issuer overrides it. Matched any-of.
	Audiences []string `json:"audiences"`

	// Issuers requires at least one entry.
	Issuers []Issuer `json:"issuers"`
}

// Issuer is one trusted token issuer.
type Issuer struct {
	// Name is the operator-chosen label used in logs and metrics. Name and Issuer
	// are each unique within the file.
	Name   string `json:"name"`
	Issuer string `json:"issuer"`

	// Audiences overrides the deployment-wide allow-list for this issuer. Absent
	// inherits; `audiences: []` is a configuration error, because golang-jwt
	// skips the audience check outright when handed no audience to expect.
	Audiences []string `json:"audiences,omitempty"`

	// JWKS says where to fetch this issuer's signing keys. Required; it is the
	// only key source.
	JWKS *JWKSKeys `json:"jwks"`

	// Algorithms is the per-issuer allow-list. Required, asymmetric only.
	Algorithms []string `json:"algorithms"`

	// Admit is the admission rule set. Required, and must carry at least one
	// rule.
	Admit *Admit `json:"admit"`

	// Roles configures dynamic Cedar role assignment for this issuer, on top of
	// roles.json's static list (design-authz-claim-based-roles.md, §3.1).
	// Optional; an issuer with no Roles config grants no claim-derived role,
	// matching today's behavior exactly.
	Roles *RoleMapping `json:"roles,omitempty"`
}

// RoleMapping derives Cedar role names from an issuer's verified token, on top
// of roles.json's static parents (§4). Exactly one of Claim, SPIFFE, or
// ServiceAccount may be set -- three structurally distinct ways of reading a
// role out of a token, mirroring Admit's own ServiceAccounts/SPIFFE
// exclusivity one level up. See RoleMapping.validate.
type RoleMapping struct {
	// Claim maps one claim's value(s) to Cedar roles, for OIDC-shaped issuers.
	Claim *ClaimRoleMapping `json:"claim,omitempty"`

	// SPIFFE derives a Cedar role from the verified SPIFFE ID via CEL, for
	// SPIRE-issued subjects with no usable claim (design §3.4).
	//
	// Each rule's Expression is compiled, type-checked to string, and
	// programmed against cel.dev/cel-go (the CEL evaluation environment
	// spiffeCELEnv builds, roles.go) -- once at Config.Validate time,
	// discarding the program, and again at RoleMapping.resolve time, keeping
	// the program roles.go's rolesFor SPIFFE branch runs per request. An
	// expression that fails to compile, references an undeclared function or
	// variable, or type-checks to anything other than string refuses startup
	// (validateSPIFFERoleRules, below).
	SPIFFE []SPIFFERoleRule `json:"spiffe,omitempty"`

	// ServiceAccount derives a Cedar role from a bound Kubernetes service
	// account token's nested "kubernetes.io" claim -- the same shape
	// admit.go's serviceAccountFromClaims reads for admission (design §3.1a).
	// Tagged "serviceAccounts" (plural), matching Admit.ServiceAccounts' own
	// key: the whole rationale for this rule kind is that it mirrors
	// admission's shape.
	ServiceAccount []ServiceAccountRoleRule `json:"serviceAccounts,omitempty"`
}

// ClaimRoleMapping maps an OIDC-shaped claim's value(s) to Cedar roles.
type ClaimRoleMapping struct {
	// Name is the top-level claim key. Nested claims (the "kubernetes.io" shape
	// admit.go's serviceAccountFromClaims reads) are out of scope here -- see
	// ServiceAccountRoleRule -- so this is a flat key, not a path.
	Name string `json:"name"`

	// ValueToRole maps one claim value to one Cedar role. The claim may be a
	// JSON string or a JSON array of strings (Entra's "groups" shape); every
	// matching value contributes its role. A value with no entry contributes
	// nothing -- not an error, see roles.go's rolesFor.
	ValueToRole map[string]string `json:"valueToRole"`
}

// SPIFFERoleRule derives a Cedar role from the verified SPIFFE ID's path via a
// CEL expression (design §3.4). See RoleMapping.SPIFFE's doc comment for the
// compile/type-check/program lifecycle.
type SPIFFERoleRule struct {
	// Expression is a CEL expression evaluated against the verified SPIFFE
	// ID's path -- never the raw subject, never the trust domain (design
	// §3.4). It must evaluate to a string: a non-empty result is the Cedar
	// role name this rule contributes; an empty string, a non-string result,
	// or an evaluation error all mean this rule contributes nothing -- never
	// a panic, never a denied request, never a grant (design §3.4, §5).
	Expression string `json:"expression"`
}

// ServiceAccountRoleRule derives a Cedar role from a PSAT's nested
// "kubernetes.io" claim. Matching shape mirrors ServiceAccountRule exactly:
// (Namespace, optional Name), because that pair -- not two independent flat
// values -- is what a PSAT actually identifies (design §3.1a).
type ServiceAccountRoleRule struct {
	Namespace string `json:"namespace"`

	// Name narrows the rule to one service account. A pointer so an absent key
	// ("any name in this namespace") stays distinct from `name: ""`, exactly
	// like ServiceAccountRule.Name.
	Name *string `json:"name,omitempty"`

	// Role is the Cedar role this rule contributes when it matches. One rule,
	// one role -- unlike ClaimRoleMapping's ValueToRole table, a PSAT names
	// exactly one namespace and one service account, not a set of them.
	Role string `json:"role"`
}

// AllowsAnyName reports whether the rule matches every service account name in
// its namespace. Mirrors ServiceAccountRule.AllowsAnyName exactly.
func (r ServiceAccountRoleRule) AllowsAnyName() bool { return r.Name == nil }

type JWKSKeys struct {
	// URI is the JWKS endpoint. Required, https only. For the local cluster:
	// https://kubernetes.default.svc/openid/v1/jwks
	URI string `json:"uri"`

	// CABundlePath is a PEM bundle for the endpoint's chain. Absent means the
	// system roots. For the local cluster this is the projected service account
	// CA at /var/run/secrets/kubernetes.io/serviceaccount/ca.crt.
	CABundlePath string `json:"caBundlePath,omitempty"`

	// BearerTokenPath is read on every fetch, not once, so a projected service
	// account token that rotates keeps working. The cluster's JWKS endpoint is
	// not anonymous and needs one; a public IdP usually does not.
	BearerTokenPath string `json:"bearerTokenPath,omitempty"`
}

// Admit is the per-issuer admission rule set. Exactly one rule kind, never both;
// see Admit.validate.
type Admit struct {
	ServiceAccounts []ServiceAccountRule `json:"serviceAccounts,omitempty"`
	SPIFFE          *SPIFFERule          `json:"spiffe,omitempty"`
}

// ServiceAccountRule admits Kubernetes service accounts.
type ServiceAccountRule struct {
	Namespace string `json:"namespace"`

	// Name narrows the rule to one service account. A pointer so that an absent
	// key ("any name in the namespace") stays distinct from `name: ""`, which
	// would silently widen a rule the operator meant to narrow.
	Name *string `json:"name,omitempty"`
}

// AllowsAnyName reports whether the rule matches every name in its namespace.
func (r ServiceAccountRule) AllowsAnyName() bool { return r.Name == nil }

// SPIFFERule admits SPIFFE IDs. Both fields are required: a trust domain alone
// admits every workload in it, and prefixes alone admit them from any domain.
type SPIFFERule struct {
	// TrustDomain is a bare host such as "example.org", no scheme and no path.
	TrustDomain string `json:"trustDomain"`

	// PathPrefixes each start with "/" and match segment-wise, so
	// "/nke-service/" does not match "/nke-service-evil/". A trailing slash
	// excludes the bare segment itself. See pathHasPrefix.
	PathPrefixes []string `json:"pathPrefixes"`
}

// EffectiveAudiences returns this issuer's override when it has one, otherwise
// the deployment-wide list. Presence, not non-emptiness: `audiences: []` returns
// empty rather than inheriting, which is what lets Config.Validate refuse it.
func (i Issuer) EffectiveAudiences(deploymentAudiences []string) []string {
	if i.Audiences != nil {
		return i.Audiences
	}
	return deploymentAudiences
}

// Load reads and validates the configuration at path. An unreadable file is an
// error, never "authentication is disabled", or a typo'd mountPath would produce
// a CWE that starts cleanly and authenticates nobody. Only an unset
// AUTH_CONFIG_PATH disables authentication.
//
// knownRoles is the declared NKE::Role set a Roles/RoleMapping entry's target
// is checked against (design-authz-claim-based-roles.md, §7) -- see
// Config.Validate for what nil means here.
func Load(path string, knownRoles map[string]struct{}) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrEmptyPath
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open authentication config %q: %w", path, err)
	}

	if err := requireSingleDocument(raw, path); err != nil {
		return nil, err
	}

	// Decoded with sigs.k8s.io/json, NOT sigs.k8s.io/yaml.UnmarshalStrict, which
	// ends in encoding/json and matches keys case-insensitively. There a rule
	// carrying both pathPrefixes and pathprefixes is not a duplicate: the
	// lowercase spelling wins and the file loads clean while admitting the whole
	// trust domain.
	jsonBytes, err := yaml.YAMLToJSONStrict(raw)
	if err != nil {
		return nil, fmt.Errorf("parse authentication config %q: %w", path, err)
	}

	var cfg Config
	strictErrs, err := kjson.UnmarshalStrict(jsonBytes, &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse authentication config %q: %w", path, err)
	}
	// Fatal, not advisory: UnmarshalStrict reports a duplicate and still keeps the
	// winning value, so ignoring these would leave the widened rule in place.
	if len(strictErrs) > 0 {
		return nil, fmt.Errorf("parse authentication config %q: %w", path, errors.Join(strictErrs...))
	}

	// Properties of the file rather than of the configuration, so checked here
	// and not in Validate.
	if err := cfg.validateTypeMeta(); err != nil {
		return nil, fmt.Errorf("invalid authentication config %q: %w", path, err)
	}

	if err := cfg.Validate(knownRoles); err != nil {
		return nil, fmt.Errorf("invalid authentication config %q: %w", path, err)
	}
	return &cfg, nil
}

// requireSingleDocument refuses a file that is empty or holds more than one YAML
// document. sigs.k8s.io/yaml has no streaming decoder and silently honors only
// the first, so an appended stanza would have no error and no effect.
func requireSingleDocument(raw []byte, path string) error {
	dec := yamlv3.NewDecoder(bytes.NewReader(raw))

	var first yamlv3.Node
	if err := dec.Decode(&first); err != nil {
		if errors.Is(err, io.EOF) {
			// Named here because Validate would report it as a missing apiVersion
			// and send the operator looking elsewhere.
			return fmt.Errorf("authentication config %q is empty", path)
		}
		return fmt.Errorf("parse authentication config %q: %w", path, err)
	}

	var extra yamlv3.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("parse authentication config %q: %w", path, err)
		}
		return fmt.Errorf("authentication config %q contains more than one YAML document", path)
	}
	return nil
}

// Validate checks every constraint. A failure is a startup failure; there is no
// partially-valid state. It changes nothing.
//
// knownRoles is the set of NKE::Role names the loaded authz bundle declares
// (authz.DeclaredRoleNames), checked against every Roles/RoleMapping entry
// that names a role literally (ClaimRoleMapping.ValueToRole's values,
// ServiceAccountRoleRule.Role). nil means authz is disabled for this
// deployment -- every other Roles constraint still runs and still fails
// startup, but the declared-role membership check specifically is skipped, so
// an unrelated AuthenticationConfiguration edit on a deployment that has never
// turned authz on does not fail startup over a role name authz will never
// check (design-authz-claim-based-roles.md, §7).
func (c *Config) Validate(knownRoles map[string]struct{}) error {
	if c == nil {
		return errors.New("authentication config is nil")
	}

	if len(c.Issuers) == 0 {
		return errors.New("issuers is required and must contain at least one issuer")
	}

	// Collected first because the audience checks below compare against every
	// issuer, including ones declared after the audience.
	issuerURLs := make(map[string]struct{}, len(c.Issuers))
	seenNames := make(map[string]int, len(c.Issuers))
	seenIssuers := make(map[string]int, len(c.Issuers))
	for i, iss := range c.Issuers {
		name := strings.TrimSpace(iss.Name)
		if name == "" {
			return fmt.Errorf("issuers[%d]: name is required and must not be empty", i)
		}
		if err := requireUnpadded("name", iss.Name); err != nil {
			return fmt.Errorf("issuers[%d]: %w", i, err)
		}
		if prev, dup := seenNames[name]; dup {
			return fmt.Errorf("issuers[%d]: duplicate name %q, already used by issuers[%d]", i, name, prev)
		}
		seenNames[name] = i

		issURL := strings.TrimSpace(iss.Issuer)
		if issURL == "" {
			return fmt.Errorf("issuers[%d] (%s): issuer is required and must not be empty", i, name)
		}
		if err := requireUnpadded("issuer", iss.Issuer); err != nil {
			return fmt.Errorf("issuers[%d] (%s): %w", i, name, err)
		}
		if prev, dup := seenIssuers[issURL]; dup {
			return fmt.Errorf("issuers[%d] (%s): duplicate issuer %q, already used by issuers[%d]", i, name, issURL, prev)
		}
		seenIssuers[issURL] = i
		issuerURLs[foldForClashCheck(issURL)] = struct{}{}
	}

	if len(c.Audiences) == 0 {
		return errors.New("audiences is required and must contain at least one audience")
	}
	if err := validateAudiences(c.Audiences, issuerURLs); err != nil {
		return err
	}

	for i, iss := range c.Issuers {
		if err := iss.validate(issuerURLs, knownRoles); err != nil {
			return fmt.Errorf("issuers[%d] (%s): %w", i, strings.TrimSpace(iss.Name), err)
		}

		// golang-jwt wraps the audience check in `if len(v.expectedAud) > 0`, so an
		// empty list is a check that does not happen. The deployment-wide list is
		// non-empty by now, so the only way here is `audiences: []`.
		if len(iss.EffectiveAudiences(c.Audiences)) == 0 {
			return fmt.Errorf(
				"issuers[%d] (%s): issuer override audiences is present but empty; remove the key to use "+
					"the deployment-wide audiences. An empty list disables the audience check rather than failing it",
				i, strings.TrimSpace(iss.Name),
			)
		}
	}

	return validateSubjectsAreUnambiguous(c.Issuers)
}

// validateSubjectsAreUnambiguous refuses a configuration in which two issuers
// can admit one subject.
//
// Nothing downstream carries the issuer: authorization keys its grants on the
// bare "sub", so two issuers whose admission rules meet on a subject are not two
// identities that happen to look alike, they are one identity with two key
// sources, and a token from either arrives holding the other's grants. Two sites
// sharing a SPIRE trust domain, or two clusters with the same
// namespace/service-account pair, are the shapes this actually takes.
//
// Refusing the overlap is what turns "the subject identifies the caller" from an
// assumption into an invariant, so it is checked here rather than left to the
// operator to notice.
func validateSubjectsAreUnambiguous(issuers []Issuer) error {
	for j, later := range issuers {
		for i := range j {
			earlier := issuers[i]
			laterRule, earlierRule, overlap := admitOverlap(later.Admit, earlier.Admit)
			if !overlap {
				continue
			}
			// Reported against the later issuer and pointing back at the earlier
			// one, the way the duplicate name and issuer checks above do.
			return fmt.Errorf(
				"issuers[%d] (%s): admit %s can admit the same subject as issuers[%d] (%s) admit %s; "+
					"authorization keys grants on the subject alone and never consults the issuer, so one caller "+
					"would hold both issuers' grants. Narrow one of the two rules so no subject matches both",
				j, strings.TrimSpace(later.Name), laterRule,
				i, strings.TrimSpace(earlier.Name), earlierRule,
			)
		}
	}
	return nil
}

// admitOverlap returns the first pair of rules from a and b that can admit one
// subject, described so an operator knows which two lines to change.
//
// A ServiceAccounts set and a SPIFFE set are compared to nothing, because the
// subjects they match cannot be the same string: one is
// "system:serviceaccount:<namespace>:<name>" and the other a "spiffe://" URI. A
// cross-kind check could never fire, so none is written.
func admitOverlap(a, b *Admit) (string, string, bool) {
	if a == nil || b == nil {
		return "", "", false
	}

	for i, ra := range a.ServiceAccounts {
		for j, rb := range b.ServiceAccounts {
			if serviceAccountRulesOverlap(ra, rb) {
				return fmt.Sprintf("serviceAccounts[%d] (%s)", i, ra.describe()),
					fmt.Sprintf("serviceAccounts[%d] (%s)", j, rb.describe()), true
			}
		}
	}

	if a.SPIFFE == nil || b.SPIFFE == nil {
		return "", "", false
	}
	// Compared as admission compares them: parsed, so any normalization the
	// SPIFFE library applies is applied to both sides. A trust domain that does
	// not parse admits nobody and so overlaps nothing; SPIFFERule.validate has
	// already refused one anyway.
	tdA, errA := spiffeid.TrustDomainFromString(strings.TrimSpace(a.SPIFFE.TrustDomain))
	tdB, errB := spiffeid.TrustDomainFromString(strings.TrimSpace(b.SPIFFE.TrustDomain))
	if errA != nil || errB != nil || tdA != tdB {
		return "", "", false
	}
	for i, pa := range a.SPIFFE.PathPrefixes {
		for j, pb := range b.SPIFFE.PathPrefixes {
			if pathPrefixesOverlap(pa, pb) {
				return fmt.Sprintf("spiffe trustDomain %q pathPrefixes[%d] %q", tdA, i, pa),
					fmt.Sprintf("spiffe trustDomain %q pathPrefixes[%d] %q", tdB, j, pb), true
			}
		}
	}
	return "", "", false
}

// serviceAccountRulesOverlap reports whether one service account can match both
// rules. Namespace and name are compared verbatim, the way
// admitRules.admitsServiceAccount matches them; an absent name means every name
// in the namespace, so it meets any rule sharing that namespace.
func serviceAccountRulesOverlap(a, b ServiceAccountRule) bool {
	if a.Namespace != b.Namespace {
		return false
	}
	if a.AllowsAnyName() || b.AllowsAnyName() {
		return true
	}
	return *a.Name == *b.Name
}

// describe renders a rule the way the operator wrote it, for an error message.
func (r ServiceAccountRule) describe() string {
	if r.AllowsAnyName() {
		return fmt.Sprintf("namespace %q, any name", r.Namespace)
	}
	return fmt.Sprintf("namespace %q, name %q", r.Namespace, *r.Name)
}

// pathPrefixesOverlap reports whether some SPIFFE path is admitted by both
// prefixes. That happens exactly when one prefix's own segments sit at or above
// the other's, so "/a/" meets "/a/b/" while "/nke-service/" and
// "/nke-service-evil/" stay apart -- the same segment-wise reading admission
// uses, which is why pathHasPrefix does the comparing.
func pathPrefixesOverlap(a, b string) bool {
	const separator = "/"
	trimmedA := strings.TrimRight(a, separator)
	trimmedB := strings.TrimRight(b, separator)
	// Equal prefixes admit the same subtree whatever their trailing slashes, and
	// pathHasPrefix alone would not say so: "/a/" deliberately excludes the bare
	// "/a" it trims down to, so neither direction would match itself.
	if trimmedA == trimmedB {
		return true
	}
	return pathHasPrefix(trimmedA, b) || pathHasPrefix(trimmedB, a)
}

// validateTypeMeta names the accepted value in both messages; "wrong" is usually
// the wrong ConfigMap key mounted at AUTH_CONFIG_PATH.
func (c *Config) validateTypeMeta() error {
	switch apiVersion := strings.TrimSpace(c.APIVersion); apiVersion {
	case ConfigAPIVersion:
	case "":
		return fmt.Errorf("apiVersion is required; this build accepts %q", ConfigAPIVersion)
	default:
		return fmt.Errorf("apiVersion %q is not supported; this build accepts %q", apiVersion, ConfigAPIVersion)
	}

	switch kind := strings.TrimSpace(c.Kind); kind {
	case ConfigKind:
	case "":
		return fmt.Errorf("kind is required; this build accepts %q", ConfigKind)
	default:
		return fmt.Errorf("kind %q is not supported; this build accepts %q", kind, ConfigKind)
	}

	return nil
}

func (i Issuer) validate(issuerURLs, knownRoles map[string]struct{}) error {
	// A present-but-empty override is caught by Config.Validate.
	if err := validateAudiences(i.Audiences, issuerURLs); err != nil {
		return err
	}

	if i.JWKS == nil {
		return errors.New("jwks is required; an issuer whose keys cannot be fetched would reject every caller")
	}
	if err := i.JWKS.validate(); err != nil {
		return fmt.Errorf("jwks: %w", err)
	}

	if err := validateAlgorithms(i.Algorithms); err != nil {
		return fmt.Errorf("algorithms: %w", err)
	}

	if i.Admit == nil {
		return errors.New("admit is required; an issuer with no admission rules would authenticate callers and then reject all of them")
	}
	if err := i.Admit.validate(); err != nil {
		return fmt.Errorf("admit: %w", err)
	}

	if i.Roles != nil {
		if err := i.Roles.validate(knownRoles, i.Admit); err != nil {
			return fmt.Errorf("roles: %w", err)
		}
	}
	return nil
}

// validate checks every Roles constraint. See Config.Validate for what a nil
// knownRoles means. admit is this issuer's own Admit, consulted only for the
// SPIFFE branch's "requires Admit.SPIFFE on the same issuer" rule -- the Claim
// and ServiceAccount branches borrow nothing from admission (design §3.1,
// §3.1a).
func (m *RoleMapping) validate(knownRoles map[string]struct{}, admit *Admit) error {
	// Presence, not length, for all three fields -- including the two
	// slice-typed ones. A length check would read an explicit `serviceAccounts:
	// []` as "unset", which is exactly the shape the empty-list checks below
	// exist to catch; using len(...) > 0 here would let that shape sail past
	// this check and be silently misclassified as "only one mechanism is
	// configured" (design §3.1a's "Shared validation and matching" bullet).
	set := 0
	if m.Claim != nil {
		set++
	}
	if m.SPIFFE != nil {
		set++
	}
	if m.ServiceAccount != nil {
		set++
	}
	switch set {
	case 0:
		return errors.New("must set exactly one of claim, spiffe, or serviceAccounts")
	case 1:
	default:
		return errors.New(
			"must not set more than one of claim, spiffe, serviceAccounts: each names a different issuer " +
				"shape, and an issuer mints one shape of token",
		)
	}

	switch {
	case m.Claim != nil:
		return m.Claim.validate(knownRoles)
	case m.SPIFFE != nil:
		if admit == nil || admit.SPIFFE == nil {
			return errors.New(
				"spiffe requires admit.spiffe on the same issuer: a role rule with no trust domain to check " +
					"paths against would have no structural check to protect it",
			)
		}
		return validateSPIFFERoleRules(m.SPIFFE)
	default:
		return validateServiceAccountRoleRules(m.ServiceAccount, knownRoles)
	}
}

func (c *ClaimRoleMapping) validate(knownRoles map[string]struct{}) error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("claim.name is required and must not be empty")
	}
	if len(c.ValueToRole) == 0 {
		return errors.New(
			"claim.valueToRole must contain at least one entry; an issuer that declares roles and maps " +
				"nothing is very likely a mistake -- omit roles entirely for a deliberate no-op",
		)
	}
	for value, role := range c.ValueToRole {
		// An empty-string claim value must never be able to grant a role through
		// an accidentally-""-keyed entry -- refused here, the one place this
		// needs enforcing, rather than also special-cased at lookup time.
		if value == "" {
			return errors.New("claim.valueToRole must not contain the empty string as a key")
		}
		if strings.TrimSpace(role) == "" {
			return fmt.Errorf("claim.valueToRole[%q] is empty; every value must name a role", value)
		}
		if err := checkKnownRole(role, knownRoles); err != nil {
			return fmt.Errorf("claim.valueToRole[%q]: %w", value, err)
		}
	}
	return nil
}

// validateSPIFFERoleRules checks presence, that every Expression is non-empty
// text, and -- design §3.1's three checks, in order -- that each Expression
// compiles against the SPIFFE role CEL environment (spiffeCELEnv, roles.go),
// type-checks to string (ast.OutputType() != cel.StringType is refused), and
// programs cleanly. The compiled cel.Program built here is discarded:
// RoleMapping.resolve compiles the same, by-now-proven-well-formed Expression
// a second time, for real, when NewVerifier builds the roleRules it actually
// runs at request time (design §3.1's "compilation lifecycle" split).
func validateSPIFFERoleRules(rules []SPIFFERoleRule) error {
	if len(rules) == 0 {
		return errors.New(
			"spiffe must contain at least one rule; an issuer that declares roles and maps nothing is " +
				"very likely a mistake -- omit roles entirely for a deliberate no-op",
		)
	}
	env, err := spiffeCELEnv()
	if err != nil {
		return fmt.Errorf("building the SPIFFE role CEL environment: %w", err)
	}
	for i, r := range rules {
		if strings.TrimSpace(r.Expression) == "" {
			return fmt.Errorf("spiffe[%d]: expression is required and must not be empty", i)
		}
		if _, err := compileSPIFFEExpression(env, r.Expression); err != nil {
			return fmt.Errorf("spiffe[%d]: expression %q: %w", i, r.Expression, err)
		}
	}
	return nil
}

func validateServiceAccountRoleRules(rules []ServiceAccountRoleRule, knownRoles map[string]struct{}) error {
	if len(rules) == 0 {
		return errors.New(
			"serviceAccounts must contain at least one rule; an issuer that declares roles and maps " +
				"nothing is very likely a mistake -- omit roles entirely for a deliberate no-op",
		)
	}
	for i, r := range rules {
		if err := validateServiceAccountSelector(r.Namespace, r.Name); err != nil {
			return fmt.Errorf("serviceAccounts[%d]: %w", i, err)
		}
		if strings.TrimSpace(r.Role) == "" {
			return fmt.Errorf("serviceAccounts[%d]: role is required and must not be empty", i)
		}
		if err := checkKnownRole(r.Role, knownRoles); err != nil {
			return fmt.Errorf("serviceAccounts[%d]: %w", i, err)
		}
	}
	return nil
}

// checkKnownRole refuses a role name the loaded authz bundle doesn't declare
// -- the same "fail loud at load" bar checkEntityStore already holds
// roles.json to (design §3.1, §5). A nil knownRoles means authz is disabled
// for this deployment, so there is nothing to check a role name against; see
// Config.Validate.
func checkKnownRole(role string, knownRoles map[string]struct{}) error {
	if knownRoles == nil {
		return nil
	}
	if _, ok := knownRoles[role]; !ok {
		return fmt.Errorf("role %q is not declared by the loaded authz bundle", role)
	}
	return nil
}

func (s *JWKSKeys) validate() error {
	uri := strings.TrimSpace(s.URI)
	if uri == "" {
		return errors.New("uri is required and must not be empty")
	}
	if err := requireUnpadded("uri", s.URI); err != nil {
		return err
	}
	if err := requireUnpadded("caBundlePath", s.CABundlePath); err != nil {
		return err
	}
	if err := requireUnpadded("bearerTokenPath", s.BearerTokenPath); err != nil {
		return err
	}
	u, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("uri %q is not a valid URL: %w", uri, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("uri %q must use https, got scheme %q", uri, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("uri %q must be absolute", uri)
	}
	return nil
}

func (a *Admit) validate() error {
	if len(a.ServiceAccounts) == 0 && a.SPIFFE == nil {
		return errors.New("must contain at least one rule: serviceAccounts or spiffe")
	}
	// Either match admits, so a set carrying both rule kinds restricts nothing.
	if len(a.ServiceAccounts) > 0 && a.SPIFFE != nil {
		return errors.New(
			"must not set both serviceAccounts and spiffe on one issuer: the two are alternatives and either " +
				"match admits, so neither restricts the other. An issuer mints one shape of identity; " +
				"give it the one rule kind that matches",
		)
	}
	for i, r := range a.ServiceAccounts {
		if err := validateServiceAccountSelector(r.Namespace, r.Name); err != nil {
			return fmt.Errorf("serviceAccounts[%d]: %w", i, err)
		}
	}
	if a.SPIFFE != nil {
		if err := a.SPIFFE.validate(); err != nil {
			return fmt.Errorf("spiffe: %w", err)
		}
	}
	return nil
}

func (s *SPIFFERule) validate() error {
	td := strings.TrimSpace(s.TrustDomain)
	if td == "" {
		return errors.New("trustDomain is required and must not be empty")
	}
	if err := requireUnpadded("trustDomain", s.TrustDomain); err != nil {
		return err
	}
	// Checked before parsing, because spiffeid.TrustDomainFromString ACCEPTS a
	// full SPIFFE ID and silently discards the path:
	// "spiffe://example.org/nke-service" parses to "example.org". An operator who
	// wrote that would get a rule covering the whole trust domain instead of one
	// path, with no error. Refuse it rather than widen it.
	if strings.Contains(td, "/") {
		return fmt.Errorf("trustDomain %q must be a bare trust domain such as \"example.org\", with no spiffe:// scheme and no path", td)
	}
	// Parsed, not merely inspected: admission's only way to report a parse failure
	// is to admit nobody, so a bad value here would be a silent deny-all.
	if _, err := spiffeid.TrustDomainFromString(td); err != nil {
		return fmt.Errorf(
			"trustDomain %q is not a valid SPIFFE trust domain, so it would admit nobody: %w", td, err,
		)
	}

	if len(s.PathPrefixes) == 0 {
		return errors.New("pathPrefixes is required and must contain at least one prefix; a trust domain alone would admit every workload in it")
	}
	for i, p := range s.PathPrefixes {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("pathPrefixes[%d] must not be empty", i)
		}
		if err := requireUnpadded(fmt.Sprintf("pathPrefixes[%d]", i), p); err != nil {
			return err
		}
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("pathPrefixes[%d] %q must start with a slash; SPIFFE ID paths are absolute, so a prefix without one can never match", i, p)
		}
		// pathHasPrefix trims trailing slashes and treats an empty result as
		// "admit everything", so "//" admits the whole trust domain while looking
		// like a restriction. Only the deliberate "/" is allowed to mean that.
		if p != "/" && strings.TrimRight(p, "/") == "" {
			return fmt.Errorf(
				"pathPrefixes[%d] %q is nothing but slashes, which would admit every workload in trust domain %q; "+
					"write exactly \"/\" if admitting the whole trust domain is what you mean", i, p, td,
			)
		}
	}
	return nil
}

func validateAlgorithms(algs []string) error {
	if len(algs) == 0 {
		return errors.New("is required and must contain at least one algorithm")
	}
	allowed := allowedAlgorithms()
	for i, a := range algs {
		alg := strings.TrimSpace(a)
		if alg == "" {
			return fmt.Errorf("[%d] must not be empty", i)
		}
		if err := requireUnpadded(fmt.Sprintf("[%d]", i), a); err != nil {
			return err
		}
		switch upper := strings.ToUpper(alg); {
		case upper == "NONE":
			return fmt.Errorf("[%d] %q is not permitted: an unsigned token would be accepted as authentic", i, a)
		case strings.HasPrefix(upper, "HS"):
			return fmt.Errorf("[%d] %q is not permitted: HMAC is symmetric, so anything able to verify a token can also mint one", i, a)
		}
		if !slices.Contains(allowed, alg) {
			return fmt.Errorf("[%d] %q is not a permitted asymmetric algorithm (allowed, case-sensitively: %s)",
				i, a, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// requireUnpadded refuses leading or trailing whitespace. Nothing trims before
// use, so `algorithms: ["RS256 "]` would validate and then match no token, with a
// 401 that says nothing about whitespace. Refused rather than trimmed, because
// trimming silently changes what the operator wrote.
func requireUnpadded(field, value string) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s %q has leading or trailing whitespace; it is used verbatim, so it would match nothing", field, value)
	}
	return nil
}

// validateAudiences names the index at fault.
func validateAudiences(auds []string, issuerURLs map[string]struct{}) error {
	for i, aud := range auds {
		if strings.TrimSpace(aud) == "" {
			return fmt.Errorf("audiences[%d] must not be empty", i)
		}
		if err := requireUnpadded(fmt.Sprintf("audiences[%d]", i), aud); err != nil {
			return err
		}
		if err := validateAudience(aud, issuerURLs); err != nil {
			return fmt.Errorf("audiences[%d]: %w", i, err)
		}
	}
	return nil
}

// validateAudience rejects the values that turn the audience check into a no-op
// or a silent deny-all.
func validateAudience(aud string, issuerURLs map[string]struct{}) error {
	// Audiences are compared literally, so "*" matches no token at all. Refused
	// rather than interpreted, so no configuration reads as "any audience".
	if strings.Contains(aud, "*") {
		return fmt.Errorf(
			"%q contains a wildcard; audiences are compared literally, so this would match no token at all. "+
				"Name each accepted audience in full", aud,
		)
	}

	norm := foldForClashCheck(aud)
	if _, clash := issuerURLs[norm]; clash {
		return fmt.Errorf("%q equals a configured issuer; a kubeadm cluster defaults --api-audiences to the service account issuer, so every pod token in the cluster already carries it and the audience check would admit all of them", aud)
	}
	if isKubernetesDefaultSvcAudience(aud) {
		return fmt.Errorf("%q is a kubernetes.default.svc form, which is the default --api-audiences value on a kubeadm cluster; every automounted pod token already carries it, so the audience check would be a cluster-wide no-op", aud)
	}
	return nil
}

// isKubernetesDefaultSvcAudience matches the in-cluster apiserver Service in any
// of its usual spellings.
func isKubernetesDefaultSvcAudience(aud string) bool {
	host := strings.ToLower(strings.TrimSpace(aud))
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	const svc = "kubernetes.default.svc"
	return host == svc || strings.HasPrefix(host, svc+".")
}

// foldForClashCheck folds case and a trailing slash for the issuer-clash
// guardrail, and for nothing else.
//
// It deliberately over-approximates. The guardrail cannot read the cluster's
// --api-audiences, so it infers it from the configured issuer, which kubeadm
// defaults them to. Folding makes near-spellings of that issuer collide too.
// Both sides are folded, so it can only widen a refusal, never admit: a false
// positive costs an operator one error that names the fix, a false negative
// admits every pod token in the cluster.
//
// Never reuse it in a matching path. There, widening is the fail-open direction:
// "iss" and "aud" are matched byte-for-byte at runtime, and duplicate-issuer
// detection compares them exactly for that reason.
func foldForClashCheck(s string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/"))
}
