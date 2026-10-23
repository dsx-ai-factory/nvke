// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/authtoken"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

// The negative cases, and the only thing in the suite that would notice if NVKE
// stopped rejecting: every other spec presents the admitted credential.
var _ = Describe("NVKE API authentication", Label("authn"), func() {
	var (
		restConfig *rest.Config
		clientset  kubernetes.Interface
		audience   string
		mode       string
	)

	BeforeEach(func(ctx SpecContext) {
		restConfig, clientset = authnCluster()

		// Two signals: a cluster can carry the ConfigMap while NVKE does not
		// mount it, and then every case below returns 200 legitimately.
		var found bool
		audience, found, mode = authnDeployment(ctx, clientset)
		if !found {
			authnAbsentf(
				"authentication is OFF: ConfigMap %s/%s does not exist, so this NVKE is configured to authenticate nobody and there is no audience to mint against",
				controller.Namespace(), authtoken.ConfigMapName,
			)
		}
		if mode == authnModeDisabled {
			authnAbsentf(
				"authentication is OFF: %s is not set on Deployment %s/%s, so the authentication middleware is not in the request chain even though ConfigMap %s carries an audience",
				authnConfigPathEnv, controller.Namespace(), controller.Deployment(), authtoken.ConfigMapName,
			)
		}
	})

	// requireEnforcing skips the rejection cases: in permissive mode the
	// middleware counts would_reject and serves anyway, so 200 is correct.
	requireEnforcing := func() {
		GinkgoHelper()
		if mode == authnModePermissive {
			Skip(fmt.Sprintf(
				"authentication is PERMISSIVE: permissiveMode is true in %s/%s, so a rejected request is counted and then served, and 200 is the correct answer to this case",
				controller.Namespace(), nvke.ControllerConfigMap,
			))
		}
	}

	It("rejects a request that carries no token with 401", func(ctx SpecContext) {
		requireEnforcing()

		// A nil source sends no Authorization header, not an empty one.
		status, body, header := authnListClusters(ctx, authnClient(restConfig, nil))

		Expect(status).To(Equal(http.StatusUnauthorized), "response body: %s", body)
		Expect(header.Get("WWW-Authenticate")).To(HavePrefix("Bearer"),
			"RFC 7235 requires a challenge on a 401")
		expectGenericErrorBody(body)
	})

	It("admits nke-clients/nke-client with the configured audience with 200", func(ctx SpecContext) {
		// Not gated on enforcing: this proves a 401 elsewhere is about the
		// credential and not about NVKE being unreachable.
		source, err := authtoken.New(clientset, authtoken.Options{Audience: audience})
		Expect(err).NotTo(HaveOccurred(), "build the admitted token source")

		status, body, _ := authnListClusters(ctx, authnClient(restConfig, source))

		Expect(status).To(Equal(http.StatusOK),
			"%s with audience %q should be admitted; response body: %s", source.Identity(), audience, body)
	})

	It("rejects a token minted for the wrong audience with 401", func(ctx SpecContext) {
		requireEnforcing()

		// Only the audience is wrong, so a 200 would mean NVKE accepts any token
		// this cluster ever issued, including every pod's automounted one.
		source, err := authtoken.New(clientset, authtoken.Options{Audience: authnWrongAudience})
		Expect(err).NotTo(HaveOccurred(), "build the wrong-audience token source")

		token := authnMint(ctx, source)
		status, body, header := authnListClusters(ctx, authnClient(restConfig, source))

		Expect(status).To(Equal(http.StatusUnauthorized),
			"a token for audience %q must not be accepted by an NVKE expecting %q; response body: %s",
			authnWrongAudience, audience, body)
		Expect(header.Get("WWW-Authenticate")).To(HavePrefix("Bearer"))
		expectGenericErrorBody(body)
		expectNoTokenEcho(body, token)
	})

	It("rejects a token from an unadmitted service account with 403", func(ctx SpecContext) {
		requireEnforcing()

		// Authenticates, fails admission: 403 means no better token would help.
		source, err := authtoken.New(clientset, authtoken.Options{
			Namespace:      authnOutsiderNamespace,
			ServiceAccount: authnOutsiderServiceAccount,
			Audience:       audience,
		})
		Expect(err).NotTo(HaveOccurred(), "build the unadmitted token source")

		token := authnMint(ctx, source)
		status, body, _ := authnListClusters(ctx, authnClient(restConfig, source))

		Expect(status).To(Equal(http.StatusForbidden),
			"%s authenticates but matches no admission rule, so it is a 403 and not a 401; response body: %s",
			source.Identity(), body)
		expectGenericErrorBody(body)
		expectNoTokenEcho(body, token)
	})

	// The anonymous surface, asserted against an NVKE authenticating everything
	// else, hence the shared BeforeEach skip.
	DescribeTable("leaves the anonymous surface reachable with no token",
		func(ctx SpecContext, path string) {
			tctx, cancel := context.WithTimeout(ctx, authnTimeout)
			defer cancel()

			status, body, err := authnForwarder(restConfig).Get(tctx, path)
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(Equal(http.StatusOK),
				"%s is on the anonymous surface in auth/authn/routes.go and must stay reachable without a credential; response body: %s",
				path, body)
		},
		// Exempt deliberately: Tilt probes it and the UI reads version metadata
		// from it with no token. The only "/api/"-prefixed
		// exemption, so the only one a narrowing of the exemption table would
		// break. /healthz and /readyz are covered credential-free in health_test.go.
		Entry("version", "/api/version"),
	)

	// The escapes. A path is exempt only when it is exempt under both the
	// escaped form and the form http.ServeMux routes on, and these are the
	// shapes where the two disagree. routes_escaping_test.go pins the predicate;
	// here the whole chain is real.
	DescribeTable("rejects an API path dressed up as something anonymous",
		func(ctx SpecContext, path string) {
			requireEnforcing()

			tctx, cancel := context.WithTimeout(ctx, authnTimeout)
			defer cancel()

			// The forwarder, so the path reaches NVKE exactly as written.
			status, body, err := authnForwarder(restConfig).Get(tctx, path)
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(Equal(http.StatusUnauthorized),
				"%s routes to the API and must be authenticated; 401 and not merely \"not 200\", because a 400 or a 404 here means the request reached a handler with no credential on it. Response body: %s",
				path, body)
			expectGenericErrorBody(body)
		},
		// Which letter is encoded and which asset extension it carries are inert:
		// routes_escaping_test.go enumerates those. These two are the distinct
		// encoding classes, run here over the real chain.
		Entry("an encoded a in /api/, with a .js suffix", "/%61pi/clusters/"+authnProbeUUID+".js"),
		// One segment, so the escaped form has no "/api/" prefix to refuse.
		Entry("an encoded slash after /api", "/api%2Fclusters.js"),
	)
})

// authnProbeUUID is the id the escape paths carry. Their asset suffix makes it
// unparseable as a UUID, so a request past authentication stops at the
// parameter binder with a 400 — hence 401 exactly, not "not 200".
const authnProbeUUID = "00000000-0000-0000-0000-000000000000"

// authnConfigPathEnv puts the middleware in the chain; permissiveMode in
// nke-controller-config decides whether it enforces.
const authnConfigPathEnv = "AUTH_CONFIG_PATH"

const (
	authnModeDisabled   = "disabled"
	authnModePermissive = "permissive"
	authnModeEnforcing  = "enforcing"
)

// From deploy/k8s/nvke-auth-dev.yaml; .invalid is reserved and cannot collide.
const (
	authnOutsiderNamespace      = "nke-outsiders"
	authnOutsiderServiceAccount = "nke-outsider"
	authnWrongAudience          = "https://nke.wrong.invalid"
)

// authnTimeout bounds one cluster read or NVKE call. These specs create nothing,
// so anything slower is a broken transport.
const authnTimeout = 60 * time.Second

// detectAuthnEnforcement reports whether the NVKE Deployment's authentication
// middleware is absent, permissive or enforcing.
func detectAuthnEnforcement(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	deployment, err := clientset.AppsV1().Deployments(controller.Namespace()).Get(ctx, controller.Deployment(), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting Deployment %s/%s: %w", controller.Namespace(), controller.Deployment(), err)
	}

	container := controllerContainer(deployment)
	if container == nil {
		return "", fmt.Errorf("deployment %s/%s has no container named %s", controller.Namespace(), controller.Deployment(), controller.Deployment())
	}

	if _, found := containerEnvValue(container, authnConfigPathEnv); !found {
		return authnModeDisabled, nil
	}

	settings, err := clientset.CoreV1().ConfigMaps(controller.Namespace()).Get(ctx, nvke.ControllerConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting ConfigMap %s/%s: %w", controller.Namespace(), nvke.ControllerConfigMap, err)
	}
	permissive, err := nvke.ControllerSetting(settings.Data, "permissiveMode")
	if err != nil {
		return "", err
	}
	if permissive == true {
		return authnModePermissive, nil
	}
	return authnModeEnforcing, nil
}

// controllerContainer picks the NVKE container by name: a scan over all of them would
// read a sidecar's AUTH_CONFIG_PATH as NVKE's.
func controllerContainer(deployment *appsv1.Deployment) *corev1.Container {
	for i := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[i].Name == controller.Deployment() {
			return &deployment.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

// containerEnvValue finds env var name. found is true whenever the variable is
// declared, even if its value comes from a reference.
func containerEnvValue(container *corev1.Container, name string) (string, bool) {
	for _, env := range container.Env {
		if env.Name == name {
			return env.Value, true
		}
	}
	return "", false
}

func authnCluster() (*rest.Config, kubernetes.Interface) {
	GinkgoHelper()
	cfg, err := config.Load(cfgPath)
	Expect(err).NotTo(HaveOccurred(), "load e2e configuration")

	restConfig, err := kube.RESTConfigWithInClusterFallback(cfg.Kube.Kubeconfig, cfg.Kube.Context)
	Expect(err).NotTo(HaveOccurred(), "the authn specs require KUBECONFIG or in-cluster configuration")

	clientset, err := kubernetes.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred(), "build a Kubernetes clientset")

	return restConfig, clientset
}

// authnDeployment reads the audience the deployment names and its enforcement
// mode; found is false when there is no auth ConfigMap. Callers skip on
// different conditions, so it reports rather than decides.
func authnDeployment(ctx context.Context, clientset kubernetes.Interface) (string, bool, string) {
	GinkgoHelper()
	tctx, cancel := context.WithTimeout(ctx, authnTimeout)
	defer cancel()

	audience, found, err := authtoken.AudienceFromCluster(tctx, clientset)
	Expect(err).NotTo(HaveOccurred(), "read the deployment's auth configuration")

	mode, err := detectAuthnEnforcement(tctx, clientset)
	Expect(err).NotTo(HaveOccurred(), "read the NVKE deployment's enforcement mode")

	return audience, found, mode
}

// authnAbsentf ends a spec that found no authentication deployed. It skips by
// default; NKE_E2E_REQUIRE_AUTHN makes it fail instead, because every spec here
// gates on artifacts that vanish with the feature, so deleting the feature and
// its configuration would turn the pass green and silent. Absence only.
func authnAbsentf(format string, args ...any) {
	GinkgoHelper()
	message := fmt.Sprintf(format, args...)
	if authnRequired() {
		Fail(fmt.Sprintf("%s asserts this cluster authenticates, and it does not. %s", authnRequireEnv, message))
	}
	Skip(message)
}

// authnRequireEnv turns every "not deployed" skip into a failure; unset skips.
const authnRequireEnv = "NKE_E2E_REQUIRE_AUTHN"

func authnRequired() bool {
	GinkgoHelper()
	required, err := parseAuthnRequired(os.Getenv(authnRequireEnv))
	Expect(err).NotTo(HaveOccurred())
	return required
}

// parseAuthnRequired reads the variable; unset or empty is false. Anything
// ParseBool refuses is an error, because a variable whose job is to fail a run
// must not decline to.
func parseAuthnRequired(raw string) (bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false, nil
	}
	required, err := strconv.ParseBool(trimmed)
	if err != nil {
		return false, fmt.Errorf("parsing %s=%q: %w (it takes a boolean: 1, t, T, TRUE, true, True, or their false counterparts)", authnRequireEnv, raw, err)
	}
	return required, nil
}

// authnForwarder is the shared tunnel to NVKE, used where a spec needs the path
// to arrive exactly as written rather than as the generated client builds it.
func authnForwarder(restConfig *rest.Config) *kube.ServiceForwarder {
	GinkgoHelper()
	forwarder, err := kube.SharedServiceForwarder(restConfig, controller.Namespace(), controller.Service(), controller.Port())
	Expect(err).NotTo(HaveOccurred(), "build port-forward client")
	return forwarder
}

// authnClient builds an NVKE API client carrying exactly the credential given.
func authnClient(restConfig *rest.Config, tokens nvke.TokenSource) *nvke.Client {
	GinkgoHelper()
	client, err := nvke.NewClientWithTokenSource(restConfig, nvke.Options{}, tokens)
	Expect(err).NotTo(HaveOccurred(), "build an NVKE API client")
	return client
}

// authnMint pulls the token out before the request, so the spec can assert the
// error body does not echo it. The source caches.
func authnMint(ctx context.Context, source *authtoken.Source) string {
	GinkgoHelper()
	tctx, cancel := context.WithTimeout(ctx, authnTimeout)
	defer cancel()

	token, err := source.Token(tctx)
	Expect(err).NotTo(HaveOccurred(), "mint a token for %s", source.Identity())
	Expect(token).NotTo(BeEmpty())
	return token
}

// authnListClusters probes GET /api/clusters. It fails only on a transport
// error, so every status is data.
func authnListClusters(ctx context.Context, client *nvke.Client) (int, []byte, http.Header) {
	GinkgoHelper()
	tctx, cancel := context.WithTimeout(ctx, authnTimeout)
	defer cancel()

	resp, err := client.API().ListClustersWithResponse(tctx, nil)
	Expect(err).NotTo(HaveOccurred(), "GET /api/clusters")
	Expect(resp.HTTPResponse).NotTo(BeNil())
	return resp.StatusCode(), resp.Body, resp.HTTPResponse.Header
}

// expectGenericErrorBody asserts the rejection body is the JSON shape every
// other error uses. The wording is free to change; an HTML page is not.
func expectGenericErrorBody(body []byte) {
	GinkgoHelper()
	var decoded struct {
		Message string `json:"message"`
	}
	Expect(json.Unmarshal(body, &decoded)).To(Succeed(), "rejection body is not JSON: %s", body)
	Expect(decoded.Message).NotTo(BeEmpty(), "rejection body carries no message: %s", body)
}

// expectNoTokenEcho asserts the rejection did not hand the credential back into
// every proxy log in between. Each segment is checked, because echoing the
// signature alone is as bad.
func expectNoTokenEcho(body []byte, token string) {
	GinkgoHelper()
	rendered := string(body)
	Expect(rendered).NotTo(ContainSubstring(token), "the rejection body echoed the presented token")
	for _, segment := range strings.Split(token, ".") {
		Expect(rendered).NotTo(ContainSubstring(segment), "the rejection body echoed part of the presented token")
	}
}

// TestDetectAuthnEnforcement covers the classification the skips depend on: a
// skipped negative spec looks like one that never existed, and no cluster has
// more than one of the three states.
func TestDetectAuthnEnforcement(t *testing.T) {
	deployment := func(env ...corev1.EnvVar) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: controller.Namespace(), Name: controller.Deployment()},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: controller.Deployment(), Env: env}},
					},
				},
			},
		}
	}
	settings := func(permissive bool) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: controller.Namespace(), Name: nvke.ControllerConfigMap},
			Data:       map[string]string{"config.yaml": fmt.Sprintf("metricsListenAddress: 127.0.0.1:9090\npermissiveMode: %t\n", permissive)},
		}
	}
	authConfigPath := corev1.EnvVar{Name: "AUTH_CONFIG_PATH", Value: "/etc/nke/auth/auth.yaml"}

	tests := []struct {
		name       string
		env        []corev1.EnvVar
		permissive bool
		want       string
	}{
		{
			name:       "no AUTH_CONFIG_PATH is authentication off, whatever permissiveMode says",
			permissive: true,
			want:       authnModeDisabled,
		},
		{
			name:       "AUTH_CONFIG_PATH with permissiveMode true is permissive",
			env:        []corev1.EnvVar{authConfigPath},
			permissive: true,
			want:       authnModePermissive,
		},
		{
			name: "AUTH_CONFIG_PATH with permissiveMode false is enforcing",
			env:  []corev1.EnvVar{authConfigPath},
			want: authnModeEnforcing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := detectAuthnEnforcement(context.Background(), fake.NewClientset(deployment(tt.env...), settings(tt.permissive)))
			if err != nil {
				t.Fatalf("detectAuthnEnforcement: %v", err)
			}
			if got != tt.want {
				t.Fatalf("detectAuthnEnforcement = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("a missing nke-controller-config is an error, not a mode", func(t *testing.T) {
		if _, err := detectAuthnEnforcement(context.Background(), fake.NewClientset(deployment(authConfigPath))); err == nil {
			t.Fatal("expected an error when nke-controller-config cannot be read")
		}
	})

	t.Run("a missing deployment is an error, not a mode", func(t *testing.T) {
		// Skipping here would turn a broken kubeconfig into a green run.
		if _, err := detectAuthnEnforcement(context.Background(), fake.NewClientset()); err == nil {
			t.Fatal("expected an error when the NVKE deployment cannot be read")
		}
	})

	t.Run("a sidecar's environment is not NVKE's", func(t *testing.T) {
		// A sidecar's AUTH_CONFIG_PATH is not NVKE's.
		sidecarOnly := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: controller.Namespace(), Name: controller.Deployment()},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "sidecar",
						Env:  []corev1.EnvVar{{Name: "AUTH_CONFIG_PATH", Value: "/etc/nke/auth/auth.yaml"}},
					}}},
				},
			},
		}
		if _, err := detectAuthnEnforcement(context.Background(), fake.NewClientset(sidecarOnly)); err == nil {
			t.Fatal("expected an error when the Deployment carries no NVKE container")
		}
	})
}

// A parse reading CI's "true" as false would take the assertion away silently.
func TestParseAuthnRequired(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		// The accept-set belongs to strconv.ParseBool. What is ours: unset reads as
		// false, and the value is trimmed first.
		{raw: "", want: false},
		{raw: "true", want: true},
		{raw: " true ", want: true},
		{raw: "false", want: false},
		{raw: "please", wantErr: true},
	} {
		t.Run(strconv.Quote(tc.raw), func(t *testing.T) {
			got, err := parseAuthnRequired(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseAuthnRequired(%q) = %v, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAuthnRequired(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("parseAuthnRequired(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
