// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// tenantKubeconfigNamespace is where TenantAccessController publishes
	// per-cluster kubeconfigs (see internal/controllers/tenantaccess).
	tenantKubeconfigNamespace = "nke-system"
	tenantKubeconfigKey       = "value"

	spireServerNamespace = "spire-server"
	spireServerContainer = "spire-server"
	spireServerConfigMap = "spire-server"
	spireServerConfigKey = "server.conf"

	spireServerInstanceLabel = "spire"
	spireServerNameLabel     = "server"

	cweSPIFFEPath       = "/nke-service/cloud-workflow-engine"
	spireExecPluginName = "k8s-spiffe-workload-jwt-exec-auth"
	spireAudienceEnv    = "SPIFFE_JWT_AUDIENCE"

	jwtRefreshBefore = time.Minute
)

type executorFactory func(string, *url.URL) (remotecommand.Executor, error)

// TenantClientFactory builds controller-runtime clients for tenant clusters.
// It obtains the tenant endpoint, CA, and authentication mode from the
// TenantAccessController-published kubeconfig in nke-system. Plain kubeconfig
// credentials are preserved; the SPIRE exec plugin is replaced by a lazily
// minted JWT-SVID for CWE's identity.
type TenantClientFactory struct {
	coreClient coreclient.CoreV1Interface
	coreREST   rest.Interface

	newExecutor executorFactory
	now         func() time.Time
}

// BuildTenantClientFactory returns a factory backed by the management cluster.
// SPIRE-specific settings are discovered lazily only when a tenant's published
// kubeconfig uses the SPIRE exec plugin. The REST config supplies the Core
// client used for management-cluster reads and is captured for SPDY pod exec.
func BuildTenantClientFactory(
	managementConfig *rest.Config,
) (*TenantClientFactory, error) {
	if managementConfig == nil {
		return nil, errors.New("management REST config is required")
	}
	managementConfig = rest.CopyConfig(managementConfig)
	managementCoreClient, err := coreclient.NewForConfig(managementConfig)
	if err != nil {
		return nil, fmt.Errorf("building management Core client: %w", err)
	}

	return &TenantClientFactory{
		coreClient: managementCoreClient,
		coreREST:   managementCoreClient.RESTClient(),
		newExecutor: func(method string, requestURL *url.URL) (remotecommand.Executor, error) {
			return remotecommand.NewSPDYExecutor(managementConfig, method, requestURL)
		},
		now: time.Now,
	}, nil
}

// GetClient returns a tenant client for a given cluster.
func (f *TenantClientFactory) GetClient(ctx context.Context, clusterName string) (client.Client, error) {
	restConfig, err := f.tenantRESTConfig(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	return newTenantClient(restConfig, clusterName)
}

// GetAdminClient returns a tenant client from the CAPI admin kubeconfig.
// Post-ready probes use it to create short-lived workloads without widening the
// production CWE SPIFFE identity's permissions solely for test setup.
func (f *TenantClientFactory) GetAdminClient(ctx context.Context, clusterName string) (client.Client, error) {
	clusterName = strings.TrimSpace(clusterName)
	if clusterName == "" {
		return nil, errors.New("cluster name is required")
	}

	secretNamespace := "cluster-" + clusterName
	secretName := clusterName + "-kubeconfig"
	secret, err := f.coreClient.Secrets(secretNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting CAPI admin kubeconfig secret %s/%s: %w", secretNamespace, secretName, err)
	}
	kubeconfig, ok := secret.Data[tenantKubeconfigKey]
	if !ok || len(kubeconfig) == 0 {
		return nil, fmt.Errorf("CAPI admin kubeconfig secret %s/%s has no non-empty %q key", secretNamespace, secretName, tenantKubeconfigKey)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing CAPI admin kubeconfig for %q: %w", clusterName, err)
	}
	return newTenantClient(restConfig, clusterName)
}

func newTenantClient(restConfig *rest.Config, clusterName string) (client.Client, error) {
	scheme := runtime.NewScheme()
	for name, addToScheme := range map[string]func(*runtime.Scheme) error{
		"apps/v1":         appsv1.AddToScheme,
		"coordination/v1": coordinationv1.AddToScheme,
		"core/v1":         corev1.AddToScheme,
		"policy/v1":       policyv1.AddToScheme,
	} {
		if err := addToScheme(scheme); err != nil {
			return nil, fmt.Errorf("registering %s tenant scheme: %w", name, err)
		}
	}

	tenantClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building tenant cluster client for %q: %w", clusterName, err)
	}
	return tenantClient, nil
}

func (f *TenantClientFactory) tenantRESTConfig(ctx context.Context, clusterName string) (*rest.Config, error) {
	clusterName = strings.TrimSpace(clusterName)
	if clusterName == "" {
		return nil, errors.New("cluster name is required")
	}

	secretName := clusterName + "-kubeconfig"
	secret, err := f.coreClient.Secrets(tenantKubeconfigNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting tenant kubeconfig secret %s/%s: %w", tenantKubeconfigNamespace, secretName, err)
	}

	kubeconfig, ok := secret.Data[tenantKubeconfigKey]
	if !ok || len(kubeconfig) == 0 {
		return nil, fmt.Errorf("tenant kubeconfig secret %s/%s has no non-empty %q key", tenantKubeconfigNamespace, secretName, tenantKubeconfigKey)
	}

	publishedConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing tenant kubeconfig for %q: %w", clusterName, err)
	}

	if !isSPIREExecConfig(publishedConfig.ExecProvider) {
		return publishedConfig, nil
	}

	audience, err := spireAudience(publishedConfig.ExecProvider)
	if err != nil {
		return nil, fmt.Errorf("reading SPIRE JWT audience from tenant kubeconfig for %q: %w", clusterName, err)
	}
	trustDomain, err := f.managementTrustDomain(ctx)
	if err != nil {
		return nil, err
	}

	// The exec plugin needs an in-cluster Workload API socket that the host-side
	// test does not have. Keep only the endpoint and TLS settings, then replace
	// the exec credentials with direct, lazily minted SPIRE JWT-SVIDs.
	tenantConfig := rest.AnonymousClientConfig(publishedConfig)
	tokenSource := newExpiringJWTSource(
		func(tokenCtx context.Context) (string, error) {
			return f.mintJWT(tokenCtx, trustDomain, audience)
		},
		f.now,
	)
	tenantConfig.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
		return &jwtBearerRoundTripper{delegate: delegate, source: tokenSource}
	})

	return tenantConfig, nil
}

func isSPIREExecConfig(execConfig *clientcmdapi.ExecConfig) bool {
	return execConfig != nil && filepath.Base(execConfig.Command) == spireExecPluginName
}

func spireAudience(execConfig *clientcmdapi.ExecConfig) (string, error) {
	if execConfig == nil {
		return "", fmt.Errorf("%s is not set", spireAudienceEnv)
	}

	var audience string
	for _, env := range execConfig.Env {
		if env.Name != spireAudienceEnv {
			continue
		}
		value := strings.TrimSpace(env.Value)
		if value == "" {
			return "", fmt.Errorf("%s is empty", spireAudienceEnv)
		}
		if audience != "" && audience != value {
			return "", fmt.Errorf("%s has conflicting values %q and %q", spireAudienceEnv, audience, value)
		}
		audience = value
	}
	if audience == "" {
		return "", fmt.Errorf("%s is not set", spireAudienceEnv)
	}
	return audience, nil
}

func (f *TenantClientFactory) mintJWT(ctx context.Context, trustDomain, audience string) (string, error) {
	pod, err := f.readySPIREServerPod(ctx)
	if err != nil {
		return "", err
	}

	spiffeID := "spiffe://" + trustDomain + cweSPIFFEPath
	command := []string{
		"bin/spire-server", "jwt", "mint",
		"-spiffeID", spiffeID,
		"-audience", audience,
	}
	execRequest := f.coreREST.Post().
		Namespace(spireServerNamespace).
		Resource("pods").
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: spireServerContainer,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, clientgoscheme.ParameterCodec)

	executor, err := f.newExecutor(http.MethodPost, execRequest.URL())
	if err != nil {
		return "", fmt.Errorf("building SPIRE server executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return "", fmt.Errorf("minting tenant JWT in %s/%s: %w", spireServerNamespace, pod.Name, err)
		}
		return "", fmt.Errorf("minting tenant JWT in %s/%s: %w: %s", spireServerNamespace, pod.Name, err, message)
	}

	fields := strings.Fields(stdout.String())
	if len(fields) != 1 {
		return "", fmt.Errorf("SPIRE server returned %d whitespace-delimited fields while minting a tenant JWT", len(fields))
	}
	return fields[0], nil
}

func (f *TenantClientFactory) managementTrustDomain(ctx context.Context) (string, error) {
	configMap, err := f.coreClient.ConfigMaps(spireServerNamespace).Get(
		ctx, spireServerConfigMap, metav1.GetOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("getting SPIRE server ConfigMap %s/%s: %w", spireServerNamespace, spireServerConfigMap, err)
	}

	serverConfig, ok := configMap.Data[spireServerConfigKey]
	if !ok || strings.TrimSpace(serverConfig) == "" {
		return "", fmt.Errorf("SPIRE server ConfigMap %s/%s has no non-empty %q key", spireServerNamespace, spireServerConfigMap, spireServerConfigKey)
	}

	trustDomain, err := trustDomainFromSPIREServerConfig([]byte(serverConfig))
	if err != nil {
		return "", fmt.Errorf("reading management trust domain from SPIRE server ConfigMap %s/%s: %w", spireServerNamespace, spireServerConfigMap, err)
	}
	return trustDomain, nil
}

func trustDomainFromSPIREServerConfig(data []byte) (string, error) {
	var config struct {
		Server struct {
			TrustDomain string   `json:"trust_domain"`
			AdminIDs    []string `json:"admin_ids"`
		} `json:"server"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("parsing %s JSON: %w", spireServerConfigKey, err)
	}

	if trustDomain := strings.TrimSpace(config.Server.TrustDomain); trustDomain != "" {
		if err := validateTrustDomain(trustDomain); err != nil {
			return "", err
		}
		return trustDomain, nil
	}

	for _, adminID := range config.Server.AdminIDs {
		parsed, err := url.Parse(strings.TrimSpace(adminID))
		if err == nil && parsed.Scheme == "spiffe" && parsed.Host != "" && parsed.Path == cweSPIFFEPath {
			return parsed.Host, nil
		}
	}
	return "", errors.New("server configuration has neither trust_domain nor a CWE admin_id")
}

func validateTrustDomain(trustDomain string) error {
	parsed, err := url.Parse("spiffe://" + trustDomain)
	if err != nil || parsed.Host != trustDomain || parsed.Path != "" {
		return fmt.Errorf("invalid SPIRE trust domain %q", trustDomain)
	}
	return nil
}

func (f *TenantClientFactory) readySPIREServerPod(ctx context.Context) (*corev1.Pod, error) {
	selectorLabels := spireServerSelector()
	pods, err := f.coreClient.Pods(spireServerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: klabels.SelectorFromSet(selectorLabels).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing SPIRE server pods: %w", err)
	}

	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || !podReady(pod) {
			continue
		}
		if !podHasContainer(pod, spireServerContainer) {
			continue
		}
		return pod.DeepCopy(), nil
	}

	return nil, fmt.Errorf("no Ready SPIRE server pod found in namespace %q with labels %v", spireServerNamespace, selectorLabels)
}

func spireServerSelector() map[string]string {
	return map[string]string{
		"app.kubernetes.io/instance": spireServerInstanceLabel,
		"app.kubernetes.io/name":     spireServerNameLabel,
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podHasContainer(pod *corev1.Pod, name string) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return true
		}
	}
	return false
}

type jwtMintFunc func(context.Context) (string, error)

// expiringJWTSource serializes refreshes and lets a caller waiting behind an
// in-flight mint stop promptly when its request context is canceled.
type expiringJWTSource struct {
	mint          jwtMintFunc
	now           func() time.Time
	refreshBefore time.Duration
	lock          chan struct{}

	token   string
	expires time.Time
}

func newExpiringJWTSource(mint jwtMintFunc, now func() time.Time) *expiringJWTSource {
	return &expiringJWTSource{
		mint:          mint,
		now:           now,
		refreshBefore: jwtRefreshBefore,
		lock:          make(chan struct{}, 1),
	}
}

func (s *expiringJWTSource) Token(ctx context.Context) (string, error) {
	if err := s.acquire(ctx); err != nil {
		return "", err
	}
	defer s.release()

	now := s.now()
	if s.token != "" && now.Add(s.refreshBefore).Before(s.expires) {
		return s.token, nil
	}

	token, err := s.mint(ctx)
	if err != nil {
		return "", err
	}
	expires, err := jwtExpiration(token)
	if err != nil {
		return "", fmt.Errorf("reading minted JWT expiration: %w", err)
	}
	if !expires.After(now) {
		return "", fmt.Errorf("minted JWT expired at %s", expires.Format(time.RFC3339))
	}

	s.token = token
	s.expires = expires
	return token, nil
}

func (s *expiringJWTSource) Invalidate(ctx context.Context, token string) {
	if err := s.acquire(ctx); err != nil {
		return
	}
	defer s.release()

	// A late 401 for an older request must not discard a token another request
	// has already refreshed.
	if s.token == token {
		s.token = ""
		s.expires = time.Time{}
	}
}

func (s *expiringJWTSource) acquire(ctx context.Context) error {
	select {
	case s.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *expiringJWTSource) release() {
	<-s.lock
}

func jwtExpiration(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("JWT has %d segments, want 3", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding JWT claims: %w", err)
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parsing JWT claims: %w", err)
	}
	if claims.ExpiresAt <= 0 {
		return time.Time{}, errors.New("JWT has no valid exp claim")
	}
	return time.Unix(claims.ExpiresAt, 0), nil
}

type jwtBearerRoundTripper struct {
	delegate http.RoundTripper
	source   *expiringJWTSource
}

func (t *jwtBearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	token, err := t.source.Token(request.Context())
	if err != nil {
		return nil, fmt.Errorf("obtaining tenant JWT: %w", err)
	}

	firstRequest := requestWithBearer(request, token)
	response, err := t.delegate.RoundTrip(firstRequest)
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}

	t.source.Invalidate(request.Context(), token)
	if !requestCanBeReplayed(request) {
		return response, nil
	}
	if closeErr := discardAndClose(response.Body); closeErr != nil {
		return nil, fmt.Errorf("closing unauthorized tenant response: %w", closeErr)
	}

	refreshedToken, err := t.source.Token(request.Context())
	if err != nil {
		return nil, fmt.Errorf("refreshing tenant JWT after unauthorized response: %w", err)
	}
	retryRequest, err := replayRequestWithBearer(request, refreshedToken)
	if err != nil {
		return nil, fmt.Errorf("replaying tenant request after JWT refresh: %w", err)
	}

	retryResponse, err := t.delegate.RoundTrip(retryRequest)
	if retryResponse != nil && retryResponse.StatusCode == http.StatusUnauthorized {
		t.source.Invalidate(request.Context(), refreshedToken)
	}
	return retryResponse, err
}

func requestWithBearer(request *http.Request, token string) *http.Request {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token)
	return clone
}

func requestCanBeReplayed(request *http.Request) bool {
	return request.Body == nil || request.Body == http.NoBody || request.GetBody != nil
}

func replayRequestWithBearer(request *http.Request, token string) (*http.Request, error) {
	clone := requestWithBearer(request, token)
	if request.Body == nil || request.Body == http.NoBody {
		return clone, nil
	}

	body, err := request.GetBody()
	if err != nil {
		return nil, err
	}
	clone.Body = body
	return clone, nil
}

func discardAndClose(body io.ReadCloser) error {
	if body == nil {
		return nil
	}
	_, copyErr := io.Copy(io.Discard, body)
	closeErr := body.Close()
	return errors.Join(copyErr, closeErr)
}
