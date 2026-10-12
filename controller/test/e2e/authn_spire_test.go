// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/authtoken"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

// The SPIRE half of NVKE API authentication. The Workload API issues a JWT-SVID
// only to an attested process, so the caller is a pod this spec creates and its
// termination message crosses back. The wrong-audience case is what makes the
// 200 mean anything; path-prefix admission is admit_test.go's job.
var _ = Describe("NVKE API authentication with a SPIRE JWT-SVID", Label("authn", "authn-spire"), func() {
	var (
		runtimeClient   client.Client
		admission       spireAdmission
		controllerImage string
	)

	BeforeEach(func(ctx SpecContext) {
		restConfig, clientset := authnCluster()

		var err error
		runtimeClient, err = kube.RuntimeClient(restConfig)
		Expect(err).NotTo(HaveOccurred(), "build a controller-runtime client")

		tctx, cancel := context.WithTimeout(ctx, authnTimeout)
		defer cancel()

		// Three skips, because any one of these missing makes the spec a lie.
		mode, err := detectAuthnEnforcement(tctx, clientset)
		Expect(err).NotTo(HaveOccurred(), "read the NVKE deployment's enforcement mode")
		if mode == authnModeDisabled {
			authnAbsentf(
				"authentication is OFF on Deployment %s/%s: %s is not set, so the middleware is not in the request chain and no SVID could be rejected",
				controller.Namespace(), controller.Deployment(), authnConfigPathEnv,
			)
		}
		if mode != authnModeEnforcing {
			// Permissive answers 200 to everything, so both cases below would
			// pass against an NVKE that never heard of SPIRE.
			Skip(fmt.Sprintf(
				"authentication is %s on Deployment %s/%s: a SPIRE acceptance that is not allowed to reject proves nothing",
				mode, controller.Namespace(), controller.Deployment(),
			))
		}

		found := false
		admission, found, err = spireAdmissionFromCluster(tctx, clientset)
		Expect(err).NotTo(HaveOccurred(), "read the deployment's auth configuration")
		if !found {
			Skip(fmt.Sprintf(
				"ConfigMap %s/%s configures no issuer named %q, so this NVKE trusts no SPIRE and there is nothing to accept",
				controller.Namespace(), authtoken.ConfigMapName, spireIssuerName,
			))
		}

		_, err = clientset.CoreV1().ServiceAccounts(spireProbeNamespace).
			Get(tctx, spireProbeServiceAccount, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			Skip(fmt.Sprintf(
				"ServiceAccount %s/%s does not exist: apply deploy/k8s/nke-spire-client.yaml, or run tilt up, so SPIRE has an identity to attest",
				spireProbeNamespace, spireProbeServiceAccount,
			))
		}
		Expect(err).NotTo(HaveOccurred(), "read the probe's service account")

		// NVKE's own image, the one on the node carrying bash, curl and the
		// Workload API JWT fetcher.
		controllerImage, err = controllerContainerImage(tctx, clientset)
		Expect(err).NotTo(HaveOccurred(), "read the NVKE deployment's image")
	})

	It("admits an attested workload's JWT-SVID and rejects one minted for the wrong audience", func(ctx SpecContext) {
		pod := spireProbePod(controllerImage, admission)

		_, message, err := kube.RunPod(ctx, runtimeClient, pod, spireProbeContainer, spireProbeTimeout)
		Expect(err).NotTo(HaveOccurred(),
			"the SPIRE probe pod did not complete; its report was %q", message)

		report := parseProbeReport(message)
		Expect(report).NotTo(HaveKey("error"),
			"the probe never reached NVKE; its report was %s", message)

		// The identity, before the status codes, and derived from the
		// configuration so ClusterSPIFFEID drift fails here.
		spiffeID := report["spiffe_id"]
		Expect(spiffeID).To(HavePrefix("spiffe://"+admission.TrustDomain+"/"),
			"the probe presented an SVID from a trust domain NVKE does not admit; report: %s", message)

		// Any one of the admitted prefixes, because admission takes any one.
		Expect(spiffeID).To(Or(admittedPathPrefixMatchers(admission)...),
			"the probe's SPIFFE ID is outside every admitted path prefix %v, so a 200 would mean the prefix is not being checked; report: %s",
			admission.PathPrefixes, message)

		Expect(report["admitted"]).To(Equal("200"),
			"a JWT-SVID for audience %q from %s must be admitted; report: %s", admission.Audience, spiffeID, message)
		Expect(report["wrong_audience"]).To(Equal("401"),
			"an SVID identical but for its audience must be rejected, or NVKE is not checking \"aud\" on SPIRE tokens; report: %s", message)
	})
})

// The probe scaffolding, mirroring deploy/k8s/nke-spire-client.yaml.
const (
	spireProbeNamespace      = "nke-spire-clients"
	spireProbeServiceAccount = "nke-spire-client"
	spireProbeConfigMap      = "nke-spire-probe"
	spireProbeScriptKey      = "probe.sh"
	spireProbePodName        = "nke-spire-probe"
	spireProbeContainer      = "probe"
)

// spireIssuerName is the `issuers[].name` this spec looks for, not the issuer
// URL, which differs per site.
const spireIssuerName = "spire"

// spireProbeTimeout bounds the whole probe. probe.sh waits 90s per SVID fetch.
const spireProbeTimeout = 5 * time.Minute

const (
	spireProbeScriptDir    = "/opt/nke-spire-probe"
	spireWorkloadAPIVolume = "spiffe-workload-api"
	spireWorkloadAPIMount  = "/spiffe-workload-api"
	spireCSIDriver         = "csi.spiffe.io"
)

// spireAdmission is the identity space NVKE admits and the audience it expects.
type spireAdmission struct {
	Audience     string
	TrustDomain  string
	PathPrefixes []string
}

// spireAdmissionFromCluster reads the SPIRE issuer out of the nke-auth-config
// ConfigMap; found is false, with a nil error, when there is none.
func spireAdmissionFromCluster(ctx context.Context, clientset kubernetes.Interface) (spireAdmission, bool, error) {
	raw, found, err := authtoken.RawConfig(ctx, clientset)
	if err != nil || !found {
		return spireAdmission{}, false, err
	}
	return parseSpireAdmission(raw)
}

func admittedPathPrefixMatchers(admission spireAdmission) []types.GomegaMatcher {
	matchers := make([]types.GomegaMatcher, 0, len(admission.PathPrefixes))
	for _, prefix := range admission.PathPrefixes {
		matchers = append(matchers, HavePrefix("spiffe://"+admission.TrustDomain+prefix))
	}
	return matchers
}

// parseSpireAdmission is the decoding half, split out to be testable.
func parseSpireAdmission(raw []byte) (spireAdmission, bool, error) {
	var parsed struct {
		Audiences []string `json:"audiences"`
		Issuers   []struct {
			Name      string   `json:"name"`
			Audiences []string `json:"audiences"`
			Admit     struct {
				SPIFFE *struct {
					TrustDomain  string   `json:"trustDomain"`
					PathPrefixes []string `json:"pathPrefixes"`
				} `json:"spiffe"`
			} `json:"admit"`
		} `json:"issuers"`
	}
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return spireAdmission{}, false, fmt.Errorf("parsing %s: %w", authtoken.ConfigMapKey, err)
	}

	for _, issuer := range parsed.Issuers {
		if issuer.Name != spireIssuerName {
			continue
		}
		if issuer.Admit.SPIFFE == nil {
			// An error rather than found=false, or this would report "no SPIRE
			// configured" about a deployment that plainly configures one.
			return spireAdmission{}, false, fmt.Errorf(
				"issuer %q sets no admit.spiffe rule, so no SPIFFE ID can be admitted through it", spireIssuerName,
			)
		}
		if issuer.Admit.SPIFFE.TrustDomain == "" || len(issuer.Admit.SPIFFE.PathPrefixes) == 0 {
			return spireAdmission{}, false, fmt.Errorf(
				"issuer %q has an incomplete admit.spiffe rule (trustDomain=%q, pathPrefixes=%v)",
				spireIssuerName, issuer.Admit.SPIFFE.TrustDomain, issuer.Admit.SPIFFE.PathPrefixes,
			)
		}

		// The per-issuer list supersedes the deployment-wide one rather than
		// adding to it. NVKE matches any-of, so the first entry will do.
		audiences := parsed.Audiences
		if len(issuer.Audiences) > 0 {
			audiences = issuer.Audiences
		}
		if len(audiences) == 0 || audiences[0] == "" {
			return spireAdmission{}, false, fmt.Errorf(
				"issuer %q resolves to no audience, and NVKE requires at least one", spireIssuerName,
			)
		}
		audience := audiences[0]

		return spireAdmission{
			Audience:     audience,
			TrustDomain:  issuer.Admit.SPIFFE.TrustDomain,
			PathPrefixes: issuer.Admit.SPIFFE.PathPrefixes,
		}, true, nil
	}
	return spireAdmission{}, false, nil
}

func controllerContainerImage(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	deployment, err := clientset.AppsV1().Deployments(controller.Namespace()).
		Get(ctx, controller.Deployment(), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting Deployment %s/%s: %w", controller.Namespace(), controller.Deployment(), err)
	}
	container := controllerContainer(deployment)
	if container == nil {
		return "", fmt.Errorf("deployment %s/%s has no container named %s", controller.Namespace(), controller.Deployment(), controller.Deployment())
	}
	return container.Image, nil
}

// spireProbePod is the caller: a one-shot pod with the SPIFFE CSI driver
// mounted, running the script from the ConfigMap. One-shot because a long-lived
// workload would keep passing on an SVID issued before its ClusterSPIFFEID went
// away.
func spireProbePod(image string, admission spireAdmission) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// GenerateName, because RunPod keeps failed pods for inspection.
			GenerateName: spireProbePodName + "-",
			Namespace:    spireProbeNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "cloud-workflow-engine",
				"nke.nvidia.com/purpose":    "nke-auth-dev",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: spireProbeServiceAccount,
			Containers: []corev1.Container{{
				Name:            spireProbeContainer,
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/bash", spireProbeScriptDir + "/" + spireProbeScriptKey},
				// Every knob probe.sh reads, set from what was discovered
				// rather than left to the script's defaults.
				Env: []corev1.EnvVar{
					{Name: "NKE_AUDIENCE", Value: admission.Audience},
					{Name: "NKE_WRONG_AUDIENCE", Value: authnWrongAudience},
					{Name: "NKE_URL", Value: fmt.Sprintf("http://%s.%s.svc:%s", controller.Service(), controller.Namespace(), controller.Port())},
				},
				// The report is the termination message; the fallback covers a
				// container that dies before writing one.
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				VolumeMounts: []corev1.VolumeMount{
					{Name: spireWorkloadAPIVolume, MountPath: spireWorkloadAPIMount, ReadOnly: true},
					{Name: "probe", MountPath: spireProbeScriptDir, ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: spireWorkloadAPIVolume,
					VolumeSource: corev1.VolumeSource{
						CSI: &corev1.CSIVolumeSource{Driver: spireCSIDriver, ReadOnly: ptr.To(true)},
					},
				},
				{
					Name: "probe",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: spireProbeConfigMap},
							DefaultMode:          ptr.To(int32(0o555)),
						},
					},
				},
			},
		},
	}
}

// parseProbeReport turns the probe's line of `key=value` pairs into a map,
// split on the first "=". Unparseable fields are dropped, so a mangled report
// fails on a missing key with the message quoted.
func parseProbeReport(message string) map[string]string {
	report := map[string]string{}
	for _, field := range strings.Fields(message) {
		key, value, found := strings.Cut(field, "=")
		if !found || key == "" {
			continue
		}
		report[key] = value
	}
	return report
}

// This decides whether the live spec runs at all, so a "found=false" bug would
// present as a skip. The fixture keeps the shape of the real ConfigMap.
func TestParseSpireAdmission(t *testing.T) {
	const full = `
apiVersion: auth.nke.nvidia.com/v1alpha1
kind: AuthenticationConfiguration
audiences: ["https://nke.tilt.local"]
issuers:
  - name: local-kubernetes
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks: {uri: "https://kubernetes.default.svc/openid/v1/jwks"}
    algorithms: [RS256]
    admit:
      serviceAccounts:
        - namespace: nke-clients
  - name: spire
    issuer: "https://nke.site-controller.internal"
    jwks: {uri: "https://nke.site-controller.internal:30443/keys"}
    algorithms: [RS256]
    admit:
      spiffe:
        trustDomain: cwe-capi.nke.local
        pathPrefixes: ["/nke-e2e/"]
`

	t.Run("reads the SPIRE entry past the Kubernetes one", func(t *testing.T) {
		got, found, err := parseSpireAdmission([]byte(full))
		if err != nil || !found {
			t.Fatalf("parseSpireAdmission: found=%v err=%v", found, err)
		}
		if got.Audience != "https://nke.tilt.local" {
			t.Errorf("audience = %q", got.Audience)
		}
		if got.TrustDomain != "cwe-capi.nke.local" {
			t.Errorf("trustDomain = %q", got.TrustDomain)
		}
		if len(got.PathPrefixes) != 1 || got.PathPrefixes[0] != "/nke-e2e/" {
			t.Errorf("pathPrefixes = %v", got.PathPrefixes)
		}
	})

	t.Run("a per-issuer audience override wins", func(t *testing.T) {
		yamlText := strings.Replace(full,
			`    issuer: "https://nke.site-controller.internal"`,
			"    issuer: \"https://nke.site-controller.internal\"\n    audiences: [\"https://cwe.spire.local\"]", 1)
		got, found, err := parseSpireAdmission([]byte(yamlText))
		if err != nil || !found {
			t.Fatalf("parseSpireAdmission: found=%v err=%v", found, err)
		}
		if got.Audience != "https://cwe.spire.local" {
			t.Errorf("audience = %q, want the per-issuer override", got.Audience)
		}
	})

	t.Run("no SPIRE issuer is a skip, not an error", func(t *testing.T) {
		kubernetesOnly, _, cut := strings.Cut(full, "  - name: spire")
		if !cut {
			t.Fatal("the fixture no longer contains a SPIRE issuer to remove")
		}
		_, found, err := parseSpireAdmission([]byte(kubernetesOnly))
		if err != nil {
			t.Fatalf("parseSpireAdmission: %v", err)
		}
		if found {
			t.Fatal("found a SPIRE issuer in a Kubernetes-only configuration")
		}
	})

	t.Run("a SPIRE issuer with no spiffe rule is an error, not a skip", func(t *testing.T) {
		broken := strings.Replace(full,
			"      spiffe:\n        trustDomain: cwe-capi.nke.local\n        pathPrefixes: [\"/nke-e2e/\"]",
			"      serviceAccounts:\n        - namespace: nke-clients", 1)
		if _, _, err := parseSpireAdmission([]byte(broken)); err == nil {
			t.Fatal("expected an error for a SPIRE issuer with no admit.spiffe rule")
		}
	})
}

// The wire format between deploy/k8s/nke-spire-client.yaml and the assertions.
func TestParseProbeReport(t *testing.T) {
	report := parseProbeReport("spiffe_id=spiffe://cwe-capi.nke.local/nke-e2e/nke-spire-client admitted=200 wrong_audience=401\n")
	for key, want := range map[string]string{
		"spiffe_id":      "spiffe://cwe-capi.nke.local/nke-e2e/nke-spire-client",
		"admitted":       "200",
		"wrong_audience": "401",
	} {
		if got := report[key]; got != want {
			t.Errorf("report[%q] = %q, want %q", key, got, want)
		}
	}

	// The failure shape has to survive the same parse.
	failed := parseProbeReport("error=no JWT-SVID for audience https://nke.tilt.local from unix:///x.sock within 90s")
	if _, ok := failed["error"]; !ok {
		t.Fatalf("an error report parsed to %v", failed)
	}
}

// TestControllerContainerImage pins the lookup the probe pod's image comes from.
func TestControllerContainerImage(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: controller.Namespace(), Name: controller.Deployment()},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						// A sidecar can come first; the lookup is by name.
						{Name: "sidecar", Image: "docker.local/other:tilt"},
						{Name: controller.Deployment(), Image: "docker.local/cwe:tilt"},
					},
				},
			},
		},
	}
	got, err := controllerContainerImage(context.Background(), fake.NewClientset(deployment))
	if err != nil {
		t.Fatalf("controllerContainerImage: %v", err)
	}
	if got != "docker.local/cwe:tilt" {
		t.Fatalf("controllerContainerImage = %q", got)
	}

	if _, err := controllerContainerImage(context.Background(), fake.NewClientset()); err == nil {
		t.Fatal("expected an error when the NVKE deployment cannot be read")
	}
}
