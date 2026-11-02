// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	k8svalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/auth/internal/strictyaml"
)

// ErrEmptyPath is returned only when path is empty. A set-but-unreadable path is
// a startup failure, never "disabled".
var ErrEmptyPath = errors.New("authentication config path is empty")

// The only apiVersion and kind this build accepts. Both are required, never
// defaulted.
const (
	ConfigAPIVersion = "auth.nvke.nvidia.com/v1alpha1"
	// ConfigAPIVersionLegacy is the spelling existing sites still mount, read
	// beside ConfigAPIVersion.
	//
	// Removed in v0.25.0. Gate: no site's nke-auth-config ConfigMap still
	// carries this apiVersion, confirmed by a fleet kubectl get cm sweep. A
	// site that still mounts it after removal fails startup with the
	// unsupported-apiVersion error below, which names what this build accepts.
	ConfigAPIVersionLegacy = "auth.nke.nvidia.com/v1alpha1"
	ConfigKind             = "AuthenticationConfiguration"
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

// Config is the issuer registry, read once at startup from the file at
// AUTH_CONFIG_PATH or from apiAuth in nvke-controller-config.
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
}

type JWKSKeys struct {
	// URI is the JWKS endpoint. Required, https only. For the local cluster:
	// https://kubernetes.default.svc/openid/v1/jwks
	URI string `json:"uri"`

	// CABundlePath is a PEM bundle for the endpoint's chain. Absent means the
	// system roots. For the local cluster this is the projected service account
	// CA at /var/run/secrets/kubernetes.io/serviceaccount/ca.crt.
	CABundlePath string `json:"caBundlePath,omitempty"`

	// CABundle is the same PEM bundle inline, for a CA the configuration can
	// carry itself instead of a file mounted beside it. A CA certificate is
	// public, so it needs no Secret. At most one of CABundle and CABundlePath.
	CABundle string `json:"caBundle,omitempty"`

	// BearerTokenPath is read on every fetch, not once, so a projected service
	// account token that rotates keeps working. The cluster's JWKS endpoint is
	// not anonymous and needs one; a public IdP usually does not.
	BearerTokenPath string `json:"bearerTokenPath,omitempty"`
}

// Admit is the per-issuer admission rule set. One of serviceAccounts, spiffe,
// or groups; see Admit.validate.
type Admit struct {
	ServiceAccounts []ServiceAccountRule `json:"serviceAccounts,omitempty"`
	SPIFFE          *SPIFFERule          `json:"spiffe,omitempty"`
	Groups          []string             `json:"groups,omitempty"`
}

// ServiceAccountRule matches Kubernetes service accounts.
type ServiceAccountRule struct {
	Namespace string `json:"namespace"`

	// Name narrows the rule to one service account. A pointer so that an absent
	// key ("any name in the namespace") stays distinct from `name: ""`, which
	// would silently widen a rule the operator meant to narrow.
	Name *string `json:"name,omitempty"`
}

// AllowsAnyName reports whether the rule matches every name in its namespace.
func (r ServiceAccountRule) AllowsAnyName() bool { return r.Name == nil }

// Matches reports whether sa is in the rule's namespace and, unless the rule
// allows any name, has the rule's name.
func (r ServiceAccountRule) Matches(sa ServiceAccount) bool {
	return sa.Namespace == r.Namespace && (r.AllowsAnyName() || *r.Name == sa.Name)
}

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
// a controller that starts cleanly and authenticates nobody. Authentication is
// disabled only when AUTH_CONFIG_PATH is unset and apiAuth is not enabled in
// nvke-controller-config.
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrEmptyPath
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open authentication config %q: %w", path, err)
	}

	var cfg Config
	if err := strictyaml.Decode(fmt.Sprintf("authentication config %q", path), raw, &cfg); err != nil {
		return nil, err
	}

	// Properties of the file rather than of the configuration, so checked here
	// and not in Validate.
	if err := cfg.validateTypeMeta(); err != nil {
		return nil, fmt.Errorf("invalid authentication config %q: %w", path, err)
	}

	if err := cfg.ValidateStartup(); err != nil {
		return nil, fmt.Errorf("invalid authentication config %q: %w", path, err)
	}
	return &cfg, nil
}

// ValidateStartup runs every check a configuration must pass before the
// controller serves with it: Validate, and an admit block on every issuer,
// which Validate alone does not require. Load calls it after the checks that
// belong to the file; nvke-controller-config's apiAuth section, which has no
// file of its own, calls it directly.
func (c *Config) ValidateStartup() error {
	if err := c.Validate(); err != nil {
		return err
	}
	for index, issuer := range c.Issuers {
		if issuer.Admit == nil {
			return fmt.Errorf(
				"issuers[%d] (%s): admit is required; an issuer with no admission rules would authenticate callers and then reject all of them",
				index,
				strings.TrimSpace(issuer.Name),
			)
		}
	}
	return nil
}

// Validate checks every constraint. A failure is a startup failure; there is no
// partially-valid state. It changes nothing.
func (c *Config) Validate() error {
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
		if err := iss.validate(issuerURLs); err != nil {
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
	return nil
}

// validateTypeMeta names the accepted value in both messages; "wrong" is usually
// the wrong ConfigMap key mounted at AUTH_CONFIG_PATH.
func (c *Config) validateTypeMeta() error {
	switch apiVersion := strings.TrimSpace(c.APIVersion); apiVersion {
	case ConfigAPIVersion, ConfigAPIVersionLegacy:
	case "":
		return fmt.Errorf("apiVersion is required; this build accepts %q or %q", ConfigAPIVersion, ConfigAPIVersionLegacy)
	default:
		return fmt.Errorf("apiVersion %q is not supported; this build accepts %q or %q", apiVersion, ConfigAPIVersion, ConfigAPIVersionLegacy)
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

func (i Issuer) validate(issuerURLs map[string]struct{}) error {
	if err := i.validateTrust(issuerURLs); err != nil {
		return err
	}

	if i.Admit == nil {
		return nil
	}
	if err := i.Admit.validate(); err != nil {
		return fmt.Errorf("admit: %w", err)
	}
	return nil
}

func (i Issuer) validateTrust(issuerURLs map[string]struct{}) error {
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
	if s.CABundle != "" {
		if s.CABundlePath != "" {
			return errors.New("caBundle and caBundlePath are both set; use one")
		}
		if !x509.NewCertPool().AppendCertsFromPEM([]byte(s.CABundle)) {
			return errors.New("caBundle contains no PEM certificates")
		}
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

// validate requires exactly one kind of rule, and checks each rule of that
// kind.
func (a *Admit) validate() error {
	kinds := 0
	if len(a.ServiceAccounts) > 0 {
		kinds++
	}
	if a.SPIFFE != nil {
		kinds++
	}
	if len(a.Groups) > 0 {
		kinds++
	}
	if kinds == 0 {
		return errors.New("must contain at least one rule: serviceAccounts, spiffe, or groups")
	}
	// Either match admits, so a set carrying more than one rule kind restricts nothing.
	if kinds > 1 {
		return errors.New(
			"must not set more than one of serviceAccounts, spiffe, and groups on one issuer: they are " +
				"alternatives and either match admits, so neither restricts the other. An issuer mints one " +
				"shape of identity; give it the one rule kind that matches",
		)
	}

	for i, g := range a.Groups {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("groups[%d] is required and must not be empty", i)
		}
		if err := requireUnpadded(fmt.Sprintf("groups[%d]", i), g); err != nil {
			return err
		}
	}

	for i, r := range a.ServiceAccounts {
		if err := r.Validate(); err != nil {
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

// Validate checks the rule's namespace and optional name. A value Kubernetes
// refuses can never match, so it fails here.
func (r ServiceAccountRule) Validate() error {
	if strings.TrimSpace(r.Namespace) == "" {
		return errors.New("namespace is required and must not be empty")
	}
	if err := requireUnpadded("namespace", r.Namespace); err != nil {
		return err
	}
	if msgs := k8svalidation.NameIsDNSLabel(r.Namespace, false); len(msgs) > 0 {
		return fmt.Errorf("namespace %q is not a valid namespace name: %s", r.Namespace, strings.Join(msgs, "; "))
	}
	if r.Name == nil {
		return nil
	}
	if strings.TrimSpace(*r.Name) == "" {
		return fmt.Errorf("name is present but empty; remove the key to match any service account in namespace %q", r.Namespace)
	}
	if err := requireUnpadded("name", *r.Name); err != nil {
		return err
	}
	if msgs := k8svalidation.NameIsDNSSubdomain(*r.Name, false); len(msgs) > 0 {
		return fmt.Errorf("name %q is not a valid service account name: %s", *r.Name, strings.Join(msgs, "; "))
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
