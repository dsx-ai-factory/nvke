// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package authtoken mints the bearer token the e2e suite presents to the NVKE
// API. Tokens come from the TokenRequest API, not a Secret, because NVKE accepts
// only a bound audience-scoped token. It refreshes, since the suite's waits
// outlast a token lifetime, and discovers the audience from the ConfigMap.
package authtoken

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
)

// Where the authentication configuration lives, in controller.Namespace().
const (
	ConfigMapName = "nke-auth-config"
	ConfigMapKey  = "auth.yaml"
)

// The identity the suite presents; deploy/k8s/nke-auth-dev.yaml creates it.
const (
	DefaultNamespace      = "nke-clients"
	DefaultServiceAccount = "nke-client"
)

// Lifetime and the margin before expiry at which a new token is minted, far
// shorter than the suite's runtime so the refresh is exercised.
const (
	DefaultTTL           = 15 * time.Minute
	DefaultRefreshBefore = 5 * time.Minute

	// MinTTL is the Kubernetes floor on ExpirationSeconds. The apiserver
	// rejects a shorter request rather than raising it, so New refuses it.
	MinTTL = 10 * time.Minute
)

// Environment overrides. All optional; the defaults above are what CI uses.
const (
	// EnvDisable sends no Authorization header; an enforcing NVKE then 401s.
	EnvDisable  = "NKE_E2E_AUTH_DISABLE"
	EnvAudience = "NKE_E2E_AUDIENCE"
	// EnvNamespace and EnvServiceAccount move the suite's identity; point them
	// at nke-outsiders/nke-outsider for the 403 case.
	EnvNamespace      = "NKE_E2E_TOKEN_NAMESPACE"
	EnvServiceAccount = "NKE_E2E_TOKEN_SERVICE_ACCOUNT"
	EnvTTL            = "NKE_E2E_TOKEN_TTL"
)

// Options configures a Source; a zero field falls back to the constant above.
type Options struct {
	Namespace      string
	ServiceAccount string
	Audience       string
	TTL            time.Duration
	RefreshBefore  time.Duration

	// Now is the clock; nil means time.Now. Exported so a test can cross the
	// refresh boundary without waiting out a token lifetime.
	Now func() time.Time
}

// Source mints and caches a service account token for one identity and
// audience. The mutex is held across CreateToken, so parallel requests on an
// expired token produce one new token, not one each.
type Source struct {
	client         kubernetes.Interface
	namespace      string
	serviceAccount string
	audience       string
	ttl            time.Duration
	refreshBefore  time.Duration
	now            func() time.Time

	mu    sync.Mutex
	token string
	// refreshAt is derived per grant in mint, so one short grant cannot narrow
	// every later margin.
	refreshAt time.Time
}

// New builds a Source over an existing clientset.
func New(client kubernetes.Interface, opts Options) (*Source, error) {
	if client == nil {
		return nil, errors.New("authtoken: a Kubernetes clientset is required")
	}
	if opts.Audience == "" {
		return nil, fmt.Errorf("authtoken: an audience is required; it is read from the %s/%s ConfigMap or set with %s",
			controller.Namespace(), ConfigMapName, EnvAudience)
	}

	if opts.TTL > 0 && opts.TTL < MinTTL {
		return nil, fmt.Errorf("authtoken: a token lifetime of %s is below the Kubernetes minimum of %s; the apiserver rejects the TokenRequest rather than raising it", opts.TTL, MinTTL)
	}

	source := &Source{
		client:         client,
		namespace:      cmp.Or(opts.Namespace, DefaultNamespace),
		serviceAccount: cmp.Or(opts.ServiceAccount, DefaultServiceAccount),
		audience:       opts.Audience,
		ttl:            opts.TTL,
		refreshBefore:  opts.RefreshBefore,
		now:            opts.Now,
	}
	if source.ttl <= 0 {
		source.ttl = DefaultTTL
	}
	if source.refreshBefore <= 0 {
		source.refreshBefore = DefaultRefreshBefore
	}
	if source.now == nil {
		source.now = time.Now
	}
	return source, nil
}

// Discover builds a Source for the NVKE in restConfig. found is false, with a
// nil error, when NKE_E2E_AUTH_DISABLE is set or there is no ConfigMap — how
// the suite keeps working against an NVKE without authentication.
func Discover(ctx context.Context, restConfig *rest.Config) (*Source, bool, error) {
	off, err := disabled()
	if err != nil {
		return nil, false, err
	}
	if off {
		return nil, false, nil
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, false, fmt.Errorf("authtoken: building Kubernetes clientset: %w", err)
	}
	return discover(ctx, client)
}

func discover(ctx context.Context, client kubernetes.Interface) (*Source, bool, error) {
	audience := os.Getenv(EnvAudience)
	if audience == "" {
		discovered, found, err := AudienceFromCluster(ctx, client)
		if err != nil {
			return nil, false, err
		}
		if !found {
			// No auth ConfigMap: this NVKE authenticates nobody.
			return nil, false, nil
		}
		audience = discovered
	}

	ttl, err := ttlFromEnv()
	if err != nil {
		return nil, false, err
	}

	source, err := New(client, Options{
		Namespace:      os.Getenv(EnvNamespace),
		ServiceAccount: os.Getenv(EnvServiceAccount),
		Audience:       audience,
		TTL:            ttl,
	})
	if err != nil {
		return nil, false, err
	}
	return source, true, nil
}

// RawConfig returns the auth.yaml the NVKE pod mounts, out of the
// nke-auth-config ConfigMap; found is false, with a nil error, when it does not
// exist. It stops at the bytes, and each caller decodes what it asserts on.
func RawConfig(ctx context.Context, client kubernetes.Interface) ([]byte, bool, error) {
	namespace := controller.Namespace()
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("authtoken: reading ConfigMap %s/%s: %w", namespace, ConfigMapName, err)
	}

	raw, ok := cm.Data[ConfigMapKey]
	if !ok {
		return nil, false, fmt.Errorf("authtoken: ConfigMap %s/%s has no %q key", namespace, ConfigMapName, ConfigMapKey)
	}
	return []byte(raw), true, nil
}

// AudienceFromCluster reads one deployment-wide audience out of the
// nke-auth-config ConfigMap. Only the top-level "audiences" list is read, since
// nothing shipped overrides it for the Kubernetes issuer; any entry would do,
// as NVKE matches any-of.
func AudienceFromCluster(ctx context.Context, client kubernetes.Interface) (string, bool, error) {
	raw, found, err := RawConfig(ctx, client)
	if err != nil || !found {
		return "", false, err
	}

	// Only the one field, rather than reproducing internal/auth's schema.
	var parsed struct {
		Audiences []string `json:"audiences"`
	}
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return "", false, fmt.Errorf("authtoken: parsing %s from ConfigMap %s/%s: %w", ConfigMapKey, controller.Namespace(), ConfigMapName, err)
	}
	if len(parsed.Audiences) == 0 || parsed.Audiences[0] == "" {
		return "", false, fmt.Errorf("authtoken: ConfigMap %s/%s sets no top-level audiences", controller.Namespace(), ConfigMapName)
	}
	return parsed.Audiences[0], true, nil
}

// Token returns a valid bearer token, minting one when the cached token is
// missing or within RefreshBefore of expiring.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && s.now().Before(s.refreshAt) {
		return s.token, nil
	}
	return s.mint(ctx)
}

// mint calls TokenRequest. The caller must hold s.mu.
func (s *Source) mint(ctx context.Context) (string, error) {
	seconds := int64(s.ttl.Seconds())
	request := &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{
			Audiences:         []string{s.audience},
			ExpirationSeconds: &seconds,
		},
	}

	response, err := s.client.CoreV1().ServiceAccounts(s.namespace).CreateToken(ctx, s.serviceAccount, request, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("authtoken: minting a token for %s with audience %q: %w (the e2e caller needs create on serviceaccounts/token in %s)",
			s.Identity(), s.audience, err, s.namespace)
	}
	if response.Status.Token == "" {
		return "", fmt.Errorf("authtoken: TokenRequest for %s returned an empty token", s.Identity())
	}

	// From the response, never the requested TTL: a cluster with
	// --service-account-max-token-expiration below it silently grants less.
	expires := response.Status.ExpirationTimestamp.Time

	// An already-expired grant is refused here, so clock skew is reported as a
	// credential problem instead of a 401 from NVKE later.
	if !expires.After(s.now()) {
		return "", fmt.Errorf(
			"authtoken: TokenRequest for %s returned a token that expired at %s, already past at %s; "+
				"check clock skew between this host and the apiserver",
			s.Identity(), expires.Format(time.RFC3339), s.now().Format(time.RFC3339),
		)
	}

	s.token = response.Status.Token
	s.refreshAt = expires.Add(-refreshMargin(expires.Sub(s.now()), s.refreshBefore))

	return s.token, nil
}

// refreshMargin is how far before expiry this grant is re-minted: half the
// lifetime when the configured margin would eat all of it, since a cluster can
// grant less than RefreshBefore. Clamped at zero.
func refreshMargin(lifetime, configured time.Duration) time.Duration {
	return max(0, min(configured, lifetime/2))
}

// Audience is the "aud" the minted tokens carry.
func (s *Source) Audience() string { return s.audience }

// Identity is the namespace/name of the service account being minted for.
func (s *Source) Identity() string { return s.namespace + "/" + s.serviceAccount }

// disabled reports whether EnvDisable is set to a true value. An unparseable
// value is an error rather than false: reading it as false would silently send
// a credential the caller meant to withhold.
func disabled() (bool, error) {
	raw := strings.TrimSpace(os.Getenv(EnvDisable))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("authtoken: parsing %s=%q: %w (it takes a boolean: 1, t, T, TRUE, true, True, or their false counterparts)", EnvDisable, raw, err)
	}
	return value, nil
}

func ttlFromEnv() (time.Duration, error) {
	raw := os.Getenv(EnvTTL)
	if raw == "" {
		return 0, nil
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("authtoken: parsing %s=%q: %w", EnvTTL, raw, err)
	}
	return ttl, nil
}
