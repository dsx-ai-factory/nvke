// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
)

// Published per-cluster kubeconfigs (see internal/controllers/tenantaccess)
// live in the site's management namespace, which mgmtns.Namespace resolves --
// it is per-site configurable, so it is never assumed here.
const (
	tenantKubeconfigKey = "value"

	spireServerNamespace = "spire-server"
	spireServerContainer = "spire-server"
	spireServerConfigKey = "server.conf"

	spireServerNameLabel = "server"

	// spireReleaseEnv overrides the helm release name SPIRE was installed
	// under. Both the server ConfigMap and the instance label derive from it,
	// so one variable covers both.
	spireReleaseEnv = "NKE_E2E_SPIRE_RELEASE"
	// defaultSpireRelease is what controller/Tiltfile installs SPIRE as, which
	// is why the derived names were previously hardcoded.
	defaultSpireRelease = "spire"

	// adminSPIFFEPath is bound to cluster-admin in every tenant by the
	// nvke:capi-cluster-admin ClusterRoleBinding the nke-rbac ConfigMap carries.
	// GetAdminClient mints this rather than widening nvke:cwe for test setup.
	adminSPIFFEPath     = "/nvke-service/capi-manager"
	spireExecPluginName = "k8s-spiffe-workload-jwt-exec-auth"
	spireAudienceEnv    = "SPIFFE_JWT_AUDIENCE"
	spireExchangeEnv    = "SPIFFE_JWT_EXCHANGE_ENDPOINT"

	tokenExchangeGrantType  = "urn:ietf:params:oauth:grant-type:token-exchange" // #nosec G101 -- public RFC 8693 identifier, not a credential.
	tokenTypeJWT            = "urn:ietf:params:oauth:token-type:jwt"            // #nosec G101 -- public RFC 8693 identifier, not a credential.
	maxExchangeResponseSize = 1 << 20

	jwtRefreshBefore    = time.Minute
	tenantAdminAuthInfo = "admin"
	tenantContextName   = "tenant"
)

type executorFactory func(string, *url.URL) (remotecommand.Executor, error)

// TenantClientFactoryOption configures a TenantClientFactory.
type TenantClientFactoryOption func(*TenantClientFactory) error

// EndpointRelay maps a tenant API server's published host:port to one the
// caller can reach, and returns the substitute. Called once per REST config,
// so an implementation that opens a tunnel should reuse an existing one for
// an address it has already been asked about.
type EndpointRelay func(ctx context.Context, hostPort string) (string, error)

// WithEndpointRelay routes every tenant API server through relay.
//
// A tenant's published kubeconfig names its own VIP, which is right for
// anything inside the site and wrong for a test process outside it. On Forge
// that address is unroutable from CI: run 36051890151 brought up a tenant
// whose nodes the controller reported Ready, then spent its whole budget failing to
// dial 10.91.194.69:6443 to confirm it from the cluster side.
//
// Left unset every tenant client talks to the published address directly,
// which is what the lanes that share a network with their clusters need.
func WithEndpointRelay(relay EndpointRelay) TenantClientFactoryOption {
	return func(factory *TenantClientFactory) error {
		if relay == nil {
			return errors.New("endpoint relay is nil")
		}
		factory.relay = relay
		return nil
	}
}

// WithJWTExchangeCABundle adds PEM-encoded roots used to verify an HTTPS JWT
// exchange endpoint declared by a published tenant kubeconfig.
func WithJWTExchangeCABundle(bundle []byte) TenantClientFactoryOption {
	return func(factory *TenantClientFactory) error {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(bundle) {
			return errors.New("JWT exchange CA bundle contains no certificates")
		}

		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return errors.New("default HTTP transport is not an *http.Transport")
		}
		transport := defaultTransport.Clone()
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}
		factory.exchangeClient = &http.Client{Transport: transport}
		return nil
	}
}

// TenantClientFactory builds controller-runtime clients for tenant clusters.
// It obtains the tenant endpoint, CA, and authentication mode from the
// TenantAccessController-published kubeconfig in the site's management
// namespace (see internal/mgmtns). Plain kubeconfig
// credentials are preserved; the SPIRE exec plugin is replaced by a lazily
// minted JWT-SVID for the controller's identity.
type TenantClientFactory struct {
	coreClient coreclient.CoreV1Interface
	coreREST   rest.Interface

	newExecutor    executorFactory
	exchangeClient *http.Client
	now            func() time.Time

	// relay, when set, substitutes a reachable address for each tenant API
	// server. See WithEndpointRelay.
	relay EndpointRelay
}

// BuildTenantClientFactory returns a factory backed by the management cluster.
// SPIRE-specific settings are discovered lazily only when a tenant's published
// kubeconfig uses the SPIRE exec plugin. The REST config supplies the Core
// client used for management-cluster reads and is captured for SPDY pod exec.
func BuildTenantClientFactory(
	managementConfig *rest.Config,
	options ...TenantClientFactoryOption,
) (*TenantClientFactory, error) {
	if managementConfig == nil {
		return nil, errors.New("management REST config is required")
	}
	managementConfig = rest.CopyConfig(managementConfig)
	managementCoreClient, err := coreclient.NewForConfig(managementConfig)
	if err != nil {
		return nil, fmt.Errorf("building management Core client: %w", err)
	}

	factory := &TenantClientFactory{
		coreClient: managementCoreClient,
		coreREST:   managementCoreClient.RESTClient(),
		newExecutor: func(method string, requestURL *url.URL) (remotecommand.Executor, error) {
			return remotecommand.NewSPDYExecutor(managementConfig, method, requestURL)
		},
		exchangeClient: http.DefaultClient,
		now:            time.Now,
	}
	for _, option := range options {
		if err := option(factory); err != nil {
			return nil, fmt.Errorf("configuring tenant client factory: %w", err)
		}
	}
	return factory, nil
}

// GetClient returns a tenant client for a given cluster.
func (f *TenantClientFactory) GetClient(ctx context.Context, clusterName string) (client.Client, error) {
	restConfig, err := f.tenantRESTConfig(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	return newTenantClient(restConfig, clusterName)
}

// GetAdminClient returns a tenant client from the admin kubeconfig TenantAccess
// publishes. Post-ready probes use it to create short-lived workloads without
// widening the production controller SPIFFE identity's permissions for test setup.
//
// With OIDC off TenantAccess publishes the admin kubeconfig itself, and that
// Secret is used as it stands. With OIDC on it publishes a SPIRE exec-auth
// kubeconfig for the controller identity, which is deliberately not an admin
// credential, so authenticate as adminSPIFFEPath instead. nvke:capi-cluster-admin
// binds that identity to cluster-admin in every tenant.
func (f *TenantClientFactory) GetAdminClient(ctx context.Context, clusterName string) (client.Client, error) {
	restConfig, err := f.AdminRESTConfig(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	return newTenantClient(restConfig, clusterName)
}

// AdminRESTConfig is the config GetAdminClient connects with, for a caller
// that builds clients of its own from it.
func (f *TenantClientFactory) AdminRESTConfig(ctx context.Context, clusterName string) (*rest.Config, error) {
	return f.tenantRESTConfigAs(ctx, clusterName, adminSPIFFEPath)
}

func newTenantClient(restConfig *rest.Config, clusterName string) (client.Client, error) {
	scheme := runtime.NewScheme()
	for name, addToScheme := range map[string]func(*runtime.Scheme) error{
		"apps/v1":                      appsv1.AddToScheme,
		"certificates.k8s.io/v1":       certificatesv1.AddToScheme,
		"coordination/v1":              coordinationv1.AddToScheme,
		"core/v1":                      corev1.AddToScheme,
		"networking.k8s.io/v1":         networkingv1.AddToScheme,
		"policy/v1":                    policyv1.AddToScheme,
		"rbac.authorization.k8s.io/v1": rbacv1.AddToScheme,
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

// relayEndpoint points cfg at the relay's substitute address and pins the TLS
// server name to the host the kubeconfig published.
//
// The server name matters as much as the address. A tenant API server's
// certificate covers its VIP and not the loopback a tunnel listens on, so
// without it every request fails verification instead of connecting -- the
// same reason the site's kubeconfig rewrite sets tls-server-name, and the
// reason RelEng's manual procedure calls for it too.
func (f *TenantClientFactory) relayEndpoint(ctx context.Context, cfg *rest.Config) error {
	if f.relay == nil {
		return nil
	}
	published, err := url.Parse(cfg.Host)
	if err != nil {
		return fmt.Errorf("parsing the published API server %q: %w", cfg.Host, err)
	}
	hostPort := published.Host
	if published.Port() == "" {
		hostPort = net.JoinHostPort(published.Hostname(), "443")
	}
	local, err := f.relay(ctx, hostPort)
	if err != nil {
		return err
	}
	if cfg.ServerName == "" {
		cfg.ServerName = published.Hostname()
	}
	published.Host = local
	cfg.Host = published.String()
	return nil
}

func (f *TenantClientFactory) tenantRESTConfig(ctx context.Context, clusterName string) (*rest.Config, error) {
	return f.tenantRESTConfigAs(ctx, clusterName, controllerSPIFFEPath())
}

// tenantRESTConfigAs builds a REST config for a tenant, authenticating as the
// given SPIFFE path. Endpoint and CA always come from the published kubeconfig;
// only the identity varies.
func (f *TenantClientFactory) tenantRESTConfigAs(ctx context.Context, clusterName, spiffePath string) (*rest.Config, error) {
	clusterName = strings.TrimSpace(clusterName)
	if clusterName == "" {
		return nil, errors.New("cluster name is required")
	}

	secretName := clusterName + "-kubeconfig"
	secret, err := f.coreClient.Secrets(mgmtns.Namespace()).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting tenant kubeconfig secret %s/%s: %w", mgmtns.Namespace(), secretName, err)
	}

	kubeconfig, ok := secret.Data[tenantKubeconfigKey]
	if !ok || len(kubeconfig) == 0 {
		return nil, fmt.Errorf("tenant kubeconfig secret %s/%s has no non-empty %q key", mgmtns.Namespace(), secretName, tenantKubeconfigKey)
	}

	publishedConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing tenant kubeconfig for %q: %w", clusterName, err)
	}

	// Before the branch below, so both the published config and the
	// AnonymousClientConfig copy the SPIRE path derives from it carry the
	// substituted address.
	if err = f.relayEndpoint(ctx, publishedConfig); err != nil {
		return nil, fmt.Errorf("relaying the tenant API server for %q: %w", clusterName, err)
	}

	if !isSPIREExecConfig(publishedConfig.ExecProvider) {
		return publishedConfig, nil
	}

	audience, err := spireAudience(publishedConfig.ExecProvider)
	if err != nil {
		return nil, fmt.Errorf("reading SPIRE JWT audience from tenant kubeconfig for %q: %w", clusterName, err)
	}
	trustDomain, err := f.ManagementTrustDomain(ctx)
	if err != nil {
		return nil, err
	}
	exchangeEndpoint, err := spireExchangeEndpoint(publishedConfig.ExecProvider)
	if err != nil {
		return nil, fmt.Errorf("reading SPIRE JWT exchange endpoint from tenant kubeconfig for %q: %w", clusterName, err)
	}

	// The exec plugin needs an in-cluster Workload API socket that the host-side
	// test does not have. Keep only the endpoint and TLS settings, then replace
	// the exec credentials with direct, lazily minted SPIRE JWT-SVIDs.
	tenantConfig := rest.AnonymousClientConfig(publishedConfig)
	tokenSource := newExpiringJWTSource(
		func(tokenCtx context.Context) (string, error) {
			return f.mintTenantJWTAs(tokenCtx, trustDomain, audience, spiffePath, exchangeEndpoint)
		},
		f.now,
	)
	tenantConfig.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
		return &jwtBearerRoundTripper{delegate: delegate, source: tokenSource}
	})

	return tenantConfig, nil
}

// AdminKubeconfig returns a self-contained admin kubeconfig for a tenant,
// suitable for mounting into a pod that shells out to kubectl.
//
// The published kubeconfig cannot be used directly on an OIDC cluster: it
// authenticates through the SPIRE exec plugin, which is not in the kubectl image
// and needs a Workload API socket the pod does not have. kubectl then fails with
// no useful message. This keeps the endpoint and CA and swaps the credential for
// a minted cluster-admin JWT.
//
// The token is fixed at call time, so use it promptly and do not cache it.
func (f *TenantClientFactory) AdminKubeconfig(ctx context.Context, clusterName string) ([]byte, error) {
	clusterName = strings.TrimSpace(clusterName)
	if clusterName == "" {
		return nil, errors.New("cluster name is required")
	}

	secretName := clusterName + "-kubeconfig"
	secret, err := f.coreClient.Secrets(mgmtns.Namespace()).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting tenant kubeconfig secret %s/%s: %w", mgmtns.Namespace(), secretName, err)
	}
	published, ok := secret.Data[tenantKubeconfigKey]
	if !ok || len(published) == 0 {
		return nil, fmt.Errorf("tenant kubeconfig secret %s/%s has no non-empty %q key", mgmtns.Namespace(), secretName, tenantKubeconfigKey)
	}

	parsed, err := clientcmd.Load(published)
	if err != nil {
		return nil, fmt.Errorf("parsing tenant kubeconfig for %q: %w", clusterName, err)
	}

	var exec *clientcmdapi.ExecConfig
	for _, authInfo := range parsed.AuthInfos {
		if authInfo != nil && isSPIREExecConfig(authInfo.Exec) {
			exec = authInfo.Exec
			break
		}
	}
	// Not a SPIRE kubeconfig, so it already carries a usable credential.
	if exec == nil {
		return published, nil
	}

	audience, err := spireAudience(exec)
	if err != nil {
		return nil, fmt.Errorf("reading SPIRE JWT audience from tenant kubeconfig for %q: %w", clusterName, err)
	}
	trustDomain, err := f.ManagementTrustDomain(ctx)
	if err != nil {
		return nil, err
	}
	exchangeEndpoint, err := spireExchangeEndpoint(exec)
	if err != nil {
		return nil, fmt.Errorf("reading SPIRE JWT exchange endpoint from tenant kubeconfig for %q: %w", clusterName, err)
	}
	token, err := f.mintTenantJWTAs(ctx, trustDomain, audience, adminSPIFFEPath, exchangeEndpoint)
	if err != nil {
		return nil, err
	}

	var cluster *clientcmdapi.Cluster
	for _, candidate := range parsed.Clusters {
		if candidate != nil {
			cluster = candidate
			break
		}
	}
	if cluster == nil {
		return nil, fmt.Errorf("tenant kubeconfig for %q names no cluster", clusterName)
	}

	config := clientcmdapi.NewConfig()
	config.Clusters[tenantContextName] = &clientcmdapi.Cluster{
		Server:                   cluster.Server,
		CertificateAuthorityData: cluster.CertificateAuthorityData,
		InsecureSkipTLSVerify:    cluster.InsecureSkipTLSVerify,
	}
	config.AuthInfos[tenantAdminAuthInfo] = &clientcmdapi.AuthInfo{Token: token}
	config.Contexts[tenantContextName] = &clientcmdapi.Context{Cluster: tenantContextName, AuthInfo: tenantAdminAuthInfo}
	config.CurrentContext = tenantContextName

	rendered, err := clientcmd.Write(*config)
	if err != nil {
		return nil, fmt.Errorf("writing admin kubeconfig for %q: %w", clusterName, err)
	}
	return rendered, nil
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

func spireExchangeEndpoint(execConfig *clientcmdapi.ExecConfig) (string, error) {
	if execConfig == nil {
		return "", nil
	}

	var endpoint string
	for _, env := range execConfig.Env {
		if env.Name != spireExchangeEnv {
			continue
		}
		value := strings.TrimSpace(env.Value)
		if value == "" {
			return "", fmt.Errorf("%s is empty", spireExchangeEnv)
		}
		if endpoint != "" && endpoint != value {
			return "", fmt.Errorf("%s has conflicting values %q and %q", spireExchangeEnv, endpoint, value)
		}
		endpoint = value
	}
	if endpoint == "" {
		return "", nil
	}

	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("%s must be an absolute HTTPS URL, got %q", spireExchangeEnv, endpoint)
	}
	return endpoint, nil
}

func (f *TenantClientFactory) mintTenantJWTAs(
	ctx context.Context,
	trustDomain, audience, spiffePath, exchangeEndpoint string,
) (string, error) {
	sourcePath := spiffePath
	if exchangeEndpoint != "" {
		var err error
		sourcePath, err = exchangeSourceSPIFFEPath(spiffePath)
		if err != nil {
			return "", err
		}
	}

	sourceToken, err := f.mintJWTAs(ctx, trustDomain, audience, sourcePath)
	if err != nil {
		return "", err
	}
	if exchangeEndpoint == "" {
		return sourceToken, nil
	}
	return f.exchangeJWT(ctx, exchangeEndpoint, sourceToken)
}

func exchangeSourceSPIFFEPath(spiffePath string) (string, error) {
	service, ok := strings.CutPrefix(spiffePath, "/nvke-service/")
	if !ok || service == "" || strings.Contains(service, "/") {
		return "", fmt.Errorf("cannot map SPIFFE path %q to a DSX exchange source identity", spiffePath)
	}
	return "/tenant/nke/service/" + service + "/bm/cwe", nil
}

func (f *TenantClientFactory) exchangeJWT(ctx context.Context, endpoint, sourceToken string) (string, error) {
	form := url.Values{
		"grant_type":           {tokenExchangeGrantType},
		"subject_token":        {sourceToken},
		"subject_token_type":   {tokenTypeJWT},
		"requested_token_type": {tokenTypeJWT},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building tenant JWT exchange request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := f.exchangeClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("exchanging tenant JWT: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxExchangeResponseSize))
	if err != nil {
		return "", fmt.Errorf("reading tenant JWT exchange response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("tenant JWT exchange returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}

	var exchanged struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &exchanged); err != nil {
		return "", fmt.Errorf("decoding tenant JWT exchange response: %w", err)
	}
	if strings.TrimSpace(exchanged.AccessToken) == "" {
		return "", errors.New("tenant JWT exchange response has no access_token")
	}
	return exchanged.AccessToken, nil
}

func (f *TenantClientFactory) mintJWT(ctx context.Context, trustDomain, audience string) (string, error) {
	return f.mintJWTAs(ctx, trustDomain, audience, controllerSPIFFEPath())
}

func (f *TenantClientFactory) mintJWTAs(ctx context.Context, trustDomain, audience, spiffePath string) (string, error) {
	spiffeID := "spiffe://" + trustDomain + spiffePath
	stdout, err := f.execSPIREServer(ctx, "minting tenant JWT", []string{
		"bin/spire-server", "jwt", "mint",
		"-spiffeID", spiffeID,
		"-audience", audience,
	})
	if err != nil {
		return "", err
	}

	fields := strings.Fields(stdout)
	if len(fields) != 1 {
		return "", fmt.Errorf("SPIRE server returned %d whitespace-delimited fields while minting a tenant JWT", len(fields))
	}
	return fields[0], nil
}

// ShowSPIREAgent returns what the management cluster's SPIRE server reports for
// the agent with spiffeID, and an error when it has no such agent.
//
// An agent is listed only once it has attested, so a nil error says a node
// proved the identity to the server, not merely that an entry names it.
func (f *TenantClientFactory) ShowSPIREAgent(ctx context.Context, spiffeID string) (string, error) {
	return f.execSPIREServer(ctx, "showing SPIRE agent "+spiffeID, []string{
		"bin/spire-server", "agent", "show", "-spiffeID", spiffeID,
	})
}

// execSPIREServer runs command in the ready SPIRE server pod and returns its
// stdout. what names the operation in an error, which carries the server's
// stderr when it wrote any.
func (f *TenantClientFactory) execSPIREServer(ctx context.Context, what string, command []string) (string, error) {
	pod, err := f.readySPIREServerPod(ctx)
	if err != nil {
		return "", err
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
			return "", fmt.Errorf("%s in %s/%s: %w", what, spireServerNamespace, pod.Name, err)
		}
		return "", fmt.Errorf("%s in %s/%s: %w: %s", what, spireServerNamespace, pod.Name, err, message)
	}
	return stdout.String(), nil
}

// ManagementTrustDomain reads the SPIRE server's trust domain from its own
// configuration, so it holds whatever the kubeconfig context is called.
func (f *TenantClientFactory) ManagementTrustDomain(ctx context.Context) (string, error) {
	configMap, err := f.coreClient.ConfigMaps(spireServerNamespace).Get(
		ctx, spireServerConfigMapName(), metav1.GetOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("getting SPIRE server ConfigMap %s/%s: %w", spireServerNamespace, spireServerConfigMapName(), err)
	}

	serverConfig, ok := configMap.Data[spireServerConfigKey]
	if !ok || strings.TrimSpace(serverConfig) == "" {
		return "", fmt.Errorf("SPIRE server ConfigMap %s/%s has no non-empty %q key", spireServerNamespace, spireServerConfigMapName(), spireServerConfigKey)
	}

	trustDomain, err := trustDomainFromSPIREServerConfig([]byte(serverConfig))
	if err != nil {
		return "", fmt.Errorf("reading management trust domain from SPIRE server ConfigMap %s/%s: %w", spireServerNamespace, spireServerConfigMapName(), err)
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
		if err == nil && parsed.Scheme == "spiffe" && parsed.Host != "" && parsed.Path == controllerSPIFFEPath() {
			return parsed.Host, nil
		}
	}
	return "", errors.New("server configuration has neither trust_domain nor a controller admin_id")
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

// spireRelease is the helm release SPIRE was installed under. Under the
// nke-stack umbrella every subchart inherits the umbrella's release, so the
// server is nke-stack-server with instance=nke-stack rather than spire-server
// with instance=spire, and the hardcoded names find nothing.
func spireRelease() string {
	if release := os.Getenv(spireReleaseEnv); release != "" {
		return release
	}
	return defaultSpireRelease
}

// spireServerConfigMapName is the chart's <release>-server.
func spireServerConfigMapName() string {
	return spireRelease() + "-server"
}

func spireServerSelector() map[string]string {
	return map[string]string{
		"app.kubernetes.io/instance": spireRelease(),
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
		ExpiresAt json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if decodeErr := decoder.Decode(&claims); decodeErr != nil {
		return time.Time{}, fmt.Errorf("parsing JWT claims: %w", decodeErr)
	}
	if claims.ExpiresAt == "" {
		return time.Time{}, errors.New("JWT has no valid exp claim")
	}
	expiresAt, err := strconv.ParseFloat(claims.ExpiresAt.String(), 64)
	if err != nil || expiresAt <= 0 || math.IsNaN(expiresAt) || math.IsInf(expiresAt, 0) || expiresAt >= float64(math.MaxInt64) {
		return time.Time{}, errors.New("JWT has no valid exp claim")
	}
	seconds, fraction := math.Modf(expiresAt)
	return time.Unix(int64(seconds), int64(fraction*float64(time.Second))), nil
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

// controllerSPIFFEPath is the SPIFFE ID path the controller presents and the
// tenant nvke:cwe bindings name: its ServiceAccount under /nvke-service/.
func controllerSPIFFEPath() string { return "/nvke-service/" + controller.ServiceAccount() }
