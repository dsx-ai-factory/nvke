// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package auth mints controller API bearer tokens from the management-cluster kubeconfig.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport"
)

// authConfigNamespaces are the namespaces the controller runs in, in the order
// they are searched: a greenfield site's name first, then the names earlier
// sites keep — nke-controller for one installed from the greenfield stacks
// before the nvke rename, cloud-workflow-engine for anything older. The first
// that holds the ConfigMap is used.
var authConfigNamespaces = []string{"nvke-controller", "nke-controller", "cloud-workflow-engine"}

// authConfigMaps are the names the chart gives the authentication ConfigMap,
// in the order they are searched: a greenfield site's name first, then the
// name every earlier site keeps by pinning apiAuth.configMap.name. Every
// (namespace, name) pair is tried, because a site that pins one of the two
// need not have pinned the other.
var authConfigMaps = []string{"nvke-auth-config", "nke-auth-config"}

const (
	authConfigKey       = "auth.yaml"
	tokenNamespace      = "nke-clients"
	tokenServiceAccount = "nke-client"
	tokenLifetime       = 10 * time.Minute
	tokenRenewBefore    = time.Minute
)

// TokenSource supplies bearer tokens for nvkectl's controller API calls. Mint returns the
// oidc-login plugin's bearer token, a TokenRequest for nke-clients/nke-client,
// or no credential when the management cluster has no controller auth ConfigMap in
// either controller namespace.
type TokenSource struct {
	client     kubernetes.Interface
	restConfig *rest.Config
	endpoint   string
	cache      *tokenCache

	mu sync.Mutex
}

// NewTokenSource builds a token source from a management cluster kubeconfig.
func NewTokenSource(kubeconfig, endpoint string) (*TokenSource, error) {
	if kubeconfig == "" {
		return nil, fmt.Errorf("kubeconfig is empty: run 'nvkectl config set-site --kubeconfig <path>'")
	}

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %q: %w", kubeconfig, err)
	}

	if useExecToken(restConfig) {
		return &TokenSource{
			restConfig: restConfig,
			endpoint:   endpoint,
		}, nil
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building Kubernetes client from kubeconfig %q: %w", kubeconfig, err)
	}
	return &TokenSource{
		client:   client,
		endpoint: endpoint,
		cache:    loadTokenCache(),
	}, nil
}

// useExecToken is true only when client-go would actually run oidc-login.
// A token, basic auth, or client cert on the same user makes it skip exec.
func useExecToken(cfg *rest.Config) bool {
	if !usesOIDCLogin(cfg.ExecProvider) {
		return false
	}
	if cfg.BearerToken != "" || cfg.BearerTokenFile != "" || cfg.Username != "" {
		return false
	}
	hasCert := len(cfg.CertData) != 0 || cfg.CertFile != ""
	hasKey := len(cfg.KeyData) != 0 || cfg.KeyFile != ""
	return !hasCert || !hasKey
}

func usesOIDCLogin(exec *clientcmdapi.ExecConfig) bool {
	if exec == nil {
		return false
	}
	if filepath.Base(exec.Command) == "kubectl-oidc_login" {
		return true
	}
	return len(exec.Args) > 0 && exec.Args[0] == "oidc-login"
}

// Mint returns a bearer token for the next controller API request.
func (s *TokenSource) Mint(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.restConfig != nil {
		return s.mintExec()
	}

	now := time.Now()
	if s.cache != nil {
		if token, ok := s.cache.tokens[s.endpoint]; ok && token.usableAt(now) {
			return token.Value, nil
		}
	}

	audience, enabled, err := s.audience(ctx)
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", nil
	}

	expirationSeconds := int64(tokenLifetime.Seconds())
	request := &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expirationSeconds,
		},
	}
	response, err := s.client.CoreV1().
		ServiceAccounts(tokenNamespace).
		CreateToken(ctx, tokenServiceAccount, request, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("minting token for %s/%s: %w", tokenNamespace, tokenServiceAccount, err)
	}
	if response.Status.Token == "" {
		return "", fmt.Errorf("minting token for %s/%s: apiserver returned an empty token", tokenNamespace, tokenServiceAccount)
	}
	token := cachedToken{
		Value:     response.Status.Token,
		ExpiresAt: response.Status.ExpirationTimestamp.Time,
	}
	if !token.ExpiresAt.IsZero() {
		if s.cache != nil {
			s.cache.store(s.endpoint, token)
		}
	}
	return response.Status.Token, nil
}

// mintExec runs client-go's exec wrappers against a stub RoundTripper that
// steals the Authorization header and never hits the network.
func (s *TokenSource) mintExec() (string, error) {
	tc, err := s.restConfig.TransportConfig()
	if err != nil {
		return "", fmt.Errorf("building kubeconfig exec transport: %w", err)
	}
	catcher := &tokenCatcher{}
	rt, err := transport.HTTPWrappersForConfig(tc, catcher)
	if err != nil {
		return "", fmt.Errorf("wrapping kubeconfig exec transport: %w", err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://nvkectl.exec.invalid/", nil)
	if err != nil {
		return "", fmt.Errorf("building exec token request: %w", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", fmt.Errorf("running kubeconfig exec plugin: %w", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if catcher.token == "" {
		return "", fmt.Errorf("kubeconfig exec plugin returned no token")
	}
	return catcher.token, nil
}

type tokenCatcher struct {
	token string
}

func (t *tokenCatcher) RoundTrip(req *http.Request) (*http.Response, error) {
	if token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok {
		t.token = token
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

func (s *TokenSource) audience(ctx context.Context) (string, bool, error) {
	var configMap *corev1.ConfigMap
	var namespace, name string
	// A ConfigMap this kubeconfig may not read is information, not a failure:
	// RBAC scoped to resourceNames denies every name it does not grant, which is
	// exactly how a site pinned to the legacy name looks once there are two names
	// to try. Keep the first denial and carry on. It is returned only if no
	// candidate turns out to be readable, so a denied read still never degrades
	// into "authentication is off" and a bare, rejected request.
	var denied error
	for _, candidateNS := range authConfigNamespaces {
		for _, candidateName := range authConfigMaps {
			found, err := s.client.CoreV1().
				ConfigMaps(candidateNS).
				Get(ctx, candidateName, metav1.GetOptions{})
			switch {
			case apierrors.IsNotFound(err):
				continue
			case apierrors.IsForbidden(err):
				if denied == nil {
					denied = fmt.Errorf("reading controller authentication ConfigMap %s/%s: %w",
						candidateNS, candidateName, err)
				}
				continue
			case err != nil:
				return "", false, fmt.Errorf("reading controller authentication ConfigMap %s/%s: %w",
					candidateNS, candidateName, err)
			}
			configMap, namespace, name = found, candidateNS, candidateName
			break
		}
		if configMap != nil {
			break
		}
	}
	if configMap == nil {
		return "", false, denied
	}

	raw, ok := configMap.Data[authConfigKey]
	if !ok {
		return "", false, fmt.Errorf("controller authentication ConfigMap %s/%s has no %q key",
			namespace, name, authConfigKey)
	}

	var config struct {
		Audiences []string `yaml:"audiences"`
	}
	if err := yaml.Unmarshal([]byte(raw), &config); err != nil {
		return "", false, fmt.Errorf("parsing %q from controller authentication ConfigMap %s/%s: %w",
			authConfigKey, namespace, name, err)
	}
	if len(config.Audiences) == 0 || config.Audiences[0] == "" {
		return "", false, fmt.Errorf("controller authentication ConfigMap %s/%s sets no audiences",
			namespace, name)
	}
	return config.Audiences[0], true, nil
}
