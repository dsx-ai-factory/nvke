// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
)

func validateEtcdEncryption(test *tenantTest) {
	validateEtcdEncryptionConfiguration(test)
	validateEtcdEncryptionKeyRotation(test)
}

func validateBrownfieldEtcdEncryption(test *tenantTest) {
	It("enables etcd encryption on an existing cluster", func(ctx context.Context) {
		Expect(test.cluster.EtcdEncrypted).To(BeFalse(), "cluster should initially be unencrypted")

		By("Waiting for every desired node to be ready")
		Eventually(test.assertDesiredNodesReady).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(1*time.Second).
			Should(Succeed(), "all desired nodes should be ready before enabling etcd encryption")

		By("Enabling etcd encryption")
		resp := expectSucceed(test.nvke.UpdateEtcdEncryption(ctx, test.cluster.ID, true))
		Expect(resp.StatusCode()).To(Equal(200), string(resp.Body))
		Expect(resp.JSON200).NotTo(BeNil())
		cluster := resp.JSON200
		Expect(cluster.EtcdEncrypted).To(BeFalse(), "encryption should remain false while enablement is in progress")
		Expect(cluster.Conditions.UpdateInProgress).NotTo(BeNil())
		Expect(cluster.Conditions.UpdateInProgress.Type).To(Equal(api.UpdateInProgressTypeEtcdEncryption))

		By("Waiting for etcd encryption enablement to complete")
		test.cluster = expectSucceed(test.nvke.WaitForEtcdEncryptionEnabled(ctx, test.cluster.ID))
		Expect(test.cluster.EtcdEncrypted).To(BeTrue())
	}, Label("lifecycle", "encryption"), NodeTimeout(15*time.Minute))

	validateEtcdEncryptionConfiguration(test)

	It("rejects disabling etcd encryption", func(ctx context.Context) {
		By("Attempting to disable etcd encryption")
		resp := expectSucceed(test.nvke.UpdateEtcdEncryption(ctx, test.cluster.ID, false))
		Expect(resp.StatusCode()).To(Equal(400), string(resp.Body))
		Expect(resp.JSON400).NotTo(BeNil())
		Expect(resp.JSON400.Message).To(Equal("disabling etcd encryption is not supported"))

		cluster := expectSucceed(test.nvke.GetCluster(ctx, test.cluster.ID))
		Expect(cluster.EtcdEncrypted).To(BeTrue())
	}, Label("lifecycle", "encryption"), NodeTimeout(5*time.Minute))
}

func validateEtcdEncryptionConfiguration(test *tenantTest) {
	It("configures Kubernetes API encryption at rest", func(ctx context.Context) {
		By("Validating the kubeadm API encryption arguments")
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))
		Expect(validateKubeadmEncryptionConfig(ctx, tenantClient)).To(Succeed())

		By("Verifying a Secret is encrypted in raw etcd")
		Expect(runEtcdEncryptionVerifier(ctx, test.k8sClient, test.k8sClientset, adminKubeconfigSecret(ctx, test))).To(Succeed())
	}, Label("lifecycle", "encryption"), NodeTimeout(15*time.Minute))
}

func validateEtcdEncryptionKeyRotation(test *tenantTest) {
	It("rotates the etcd encryption key through the operator Temporal workflow", func(ctx context.Context) {
		By("Waiting for every desired node to be ready")
		Eventually(test.assertDesiredNodesReady).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(1*time.Second).
			Should(Succeed(), "all desired nodes should be ready before rotating the etcd encryption key")

		beforeKey := expectSucceed(readEtcdEncryptionKeyState(ctx, test.k8sClient, test.cluster.ID.String()))
		Expect(beforeKey.pending).To(BeFalse(), "a pending key should not exist before rotation")
		beforeRestarts := expectSucceed(readKubeAPIServerRestartCounts(ctx, test.k8sClient, adminKubeconfigSecret(ctx, test)))

		By("Starting the operator-only Temporal workflow")
		workflowID := expectSucceed(runEtcdEncryptionKeyRotation(ctx, test.k8sClient, test.cluster.ID.String()))
		GinkgoLogr.Info("Etcd encryption key rotation workflow completed", "workflowID", workflowID)

		By("Verifying the new key was promoted and temporary state was removed")
		afterKey := expectSucceed(readEtcdEncryptionKeyState(ctx, test.k8sClient, test.cluster.ID.String()))
		Expect(afterKey.fingerprint).NotTo(Equal(beforeKey.fingerprint), "active key fingerprint should change")
		Expect(afterKey.activeSince).To(BeNumerically(">", beforeKey.activeSince), "active key timestamp should advance")
		Expect(afterKey.pending).To(BeFalse(), "pending key should be removed after rotation")

		cluster := expectSucceed(test.nvke.GetCluster(ctx, test.cluster.ID))
		Expect(cluster.EtcdEncrypted).To(BeTrue())
		Expect(cluster.Conditions.UpdateInProgress).To(BeNil())
		Expect(cluster.Conditions.UpdateFailed).To(BeNil())
		test.cluster = cluster

		By("Verifying the configuration update did not restart kube-apiserver")
		afterRestarts := expectSucceed(readKubeAPIServerRestartCounts(ctx, test.k8sClient, adminKubeconfigSecret(ctx, test)))
		Expect(afterRestarts).To(Equal(beforeRestarts))
		Expect(test.assertDesiredNodesReady(ctx)).To(Succeed())

		By("Verifying Secrets remain encrypted in raw etcd after rotation")
		Expect(runEtcdEncryptionVerifier(ctx, test.k8sClient, test.k8sClientset, adminKubeconfigSecret(ctx, test))).To(Succeed())
	}, Label("encryption", "etcd-key-rotation"), NodeTimeout(20*time.Minute))
}

func validateEtcdEncryptionPostDelete(test *tenantTest) {
	It("removes the controller etcd encryption key secret during cleanup", func(ctx context.Context) {
		key := client.ObjectKey{
			Namespace: controller.Namespace(),
			Name:      "etcd-encryption-key-" + test.cluster.ID.String(),
		}
		secret := &corev1.Secret{}
		err := test.k8sClient.Get(ctx, key, secret)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected %s/%s to be deleted, got %v", key.Namespace, key.Name, err)
	}, Label("encryption", "cleanup"), NodeTimeout(1*time.Minute))
}

const (
	kubeadmConfigMapNamespace = "kube-system"
	kubeadmConfigMapName      = "kubeadm-config"
	kubeadmConfigDataKey      = "ClusterConfiguration"

	encryptionProviderConfigFlag = "encryption-provider-config"
	encryptionAutoReloadFlag     = "encryption-provider-config-automatic-reload"
	encryptionProviderConfigPath = "/etc/kubernetes/kube-api-encryption/encryption-config.yaml"

	etcdEncryptionKeySecretPrefix = "etcd-encryption-key-"
	etcdEncryptionActiveKey       = "active"
	etcdEncryptionActiveSinceKey  = "active-since"
	etcdEncryptionPendingKey      = "pending"
)

type etcdEncryptionKeyState struct {
	fingerprint string
	activeSince int64
	pending     bool
}

func readEtcdEncryptionKeyState(
	ctx context.Context,
	c client.Client,
	clusterID string,
) (etcdEncryptionKeyState, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{
		Namespace: controller.Namespace(),
		Name:      etcdEncryptionKeySecretPrefix + clusterID,
	}, &secret); err != nil {
		return etcdEncryptionKeyState{}, fmt.Errorf("getting etcd encryption key secret: %w", err)
	}

	active := secret.Data[etcdEncryptionActiveKey]
	if len(active) == 0 {
		return etcdEncryptionKeyState{}, fmt.Errorf("etcd encryption key secret has no active key")
	}
	activeSince, err := strconv.ParseInt(string(secret.Data[etcdEncryptionActiveSinceKey]), 10, 64)
	if err != nil {
		return etcdEncryptionKeyState{}, fmt.Errorf("parsing etcd encryption active-since timestamp: %w", err)
	}
	_, pending := secret.Data[etcdEncryptionPendingKey]
	fingerprint := sha256.Sum256(active)
	return etcdEncryptionKeyState{
		fingerprint: fmt.Sprintf("%x", fingerprint),
		activeSince: activeSince,
		pending:     pending,
	}, nil
}

func readKubeAPIServerRestartCounts(ctx context.Context, c client.Client, kubeconfigSecret string) (string, error) {
	namespace := mgmtns.Namespace()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-kube-apiserver-restarts-",
			Namespace:    namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "verify",
				Image:   kube.KubectlImage,
				Command: []string{"sh", "-c", kubeAPIServerRestartCountScript},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "kubeconfig",
					MountPath: "/kubeconfig",
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "kubeconfig",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: kubeconfigSecret,
				}},
			}},
		},
	}
	_, message, err := kube.RunPod(ctx, c, pod, "verify", 2*time.Minute)
	if err != nil {
		return "", fmt.Errorf("reading kube-apiserver restart counts: %w", err)
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("kube-apiserver restart count verifier returned no counts")
	}
	return strings.TrimSpace(message), nil
}

func runEtcdEncryptionKeyRotation(ctx context.Context, c client.Client, clusterID string) (string, error) {
	temporalNamespace := envOrDefault("TEMPORAL_K8S_NAMESPACE", "temporal")
	adminToolsDeployment := envOrDefault("TEMPORAL_ADMIN_TOOLS_DEPLOYMENT", "temporal-admintools")

	var deployment appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{
		Namespace: temporalNamespace,
		Name:      adminToolsDeployment,
	}, &deployment); err != nil {
		return "", fmt.Errorf("getting Temporal admin-tools deployment: %w", err)
	}

	workflowID := fmt.Sprintf("e2e-rotate-etcd-key-%s-%d", clusterID, time.Now().UnixNano())
	adminToolsContainer := envOrDefault("TEMPORAL_ADMIN_TOOLS_CONTAINER", "admin-tools")
	pod := &corev1.Pod{
		ObjectMeta: *deployment.Spec.Template.ObjectMeta.DeepCopy(),
		Spec:       *deployment.Spec.Template.Spec.DeepCopy(),
	}
	pod.Name = ""
	pod.GenerateName = "e2e-etcd-key-rotation-"
	pod.Namespace = temporalNamespace
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever

	operatorContainer := -1
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == adminToolsContainer {
			operatorContainer = i
			break
		}
	}
	if operatorContainer == -1 {
		return "", fmt.Errorf(
			"Temporal admin-tools deployment has no %q container",
			adminToolsContainer,
		)
	}

	operator := &pod.Spec.Containers[operatorContainer]
	operator.Command = []string{"sh", "-ec"}
	operator.Args = []string{etcdEncryptionKeyRotationScript}
	operator.Env = append(operator.Env, []corev1.EnvVar{
		{Name: "E2E_CLUSTER_ID", Value: clusterID},
		{Name: "E2E_WORKFLOW_ID", Value: workflowID},
		{Name: "E2E_TEMPORAL_ADDRESS", Value: envOrDefault("E2E_TEMPORAL_ADDRESS", "temporal-frontend."+temporalNamespace+".svc.cluster.local:7233")},
		{Name: "E2E_TEMPORAL_NAMESPACE", Value: envOrDefault("E2E_TEMPORAL_NAMESPACE", "nvke-controller")},
		{Name: "E2E_TEMPORAL_TASK_QUEUE", Value: envOrDefault("E2E_TEMPORAL_TASK_QUEUE", "nvke-controller-tasks")},
	}...)

	if _, _, err := kube.RunPod(ctx, c, pod, adminToolsContainer, 15*time.Minute); err != nil {
		return "", fmt.Errorf("running etcd encryption key rotation workflow: %w", err)
	}
	return workflowID, nil
}

func validateKubeadmEncryptionConfig(ctx context.Context, tenantClient client.Client) error {
	var configMap corev1.ConfigMap
	if err := tenantClient.Get(ctx, client.ObjectKey{
		Namespace: kubeadmConfigMapNamespace,
		Name:      kubeadmConfigMapName,
	}, &configMap); err != nil {
		return fmt.Errorf("getting kubeadm ConfigMap: %w", err)
	}

	clusterConfiguration, ok := configMap.Data[kubeadmConfigDataKey]
	if !ok || strings.TrimSpace(clusterConfiguration) == "" {
		return fmt.Errorf("kubeadm ConfigMap is missing non-empty %q data", kubeadmConfigDataKey)
	}
	extraArgs, err := parseKubeadmExtraArgs(clusterConfiguration)
	if err != nil {
		return err
	}
	if got := extraArgs[encryptionProviderConfigFlag]; got != encryptionProviderConfigPath {
		return fmt.Errorf("kubeadm apiServer.extraArgs[%q] = %q, want %q",
			encryptionProviderConfigFlag, got, encryptionProviderConfigPath)
	}
	if got := extraArgs[encryptionAutoReloadFlag]; !strings.EqualFold(strings.TrimSpace(got), "true") {
		return fmt.Errorf("kubeadm apiServer.extraArgs[%q] = %q, want true", encryptionAutoReloadFlag, got)
	}
	return nil
}

func parseKubeadmExtraArgs(clusterConfiguration string) (map[string]string, error) {
	configJSON, err := yaml.YAMLToJSON([]byte(clusterConfiguration))
	if err != nil {
		return nil, fmt.Errorf("converting kubeadm ClusterConfiguration to JSON: %w", err)
	}

	var config struct {
		APIServer struct {
			ExtraArgs any `json:"extraArgs"`
		} `json:"apiServer"`
	}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return nil, fmt.Errorf("decoding kubeadm ClusterConfiguration: %w", err)
	}

	args := map[string]string{}
	switch extraArgs := config.APIServer.ExtraArgs.(type) {
	case map[string]any:
		for name, value := range extraArgs {
			if rendered, ok := kubeadmArgValue(value); ok {
				args[name] = rendered
			}
		}
	case []any:
		for index, item := range extraArgs {
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("kubeadm apiServer.extraArgs[%d] is not an object", index)
			}
			name, ok := entry["name"].(string)
			if !ok || strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("kubeadm apiServer.extraArgs[%d] has no non-empty name", index)
			}
			if rendered, ok := kubeadmArgValue(entry["value"]); ok {
				args[name] = rendered
			}
		}
	case nil:
		return nil, fmt.Errorf("kubeadm ClusterConfiguration is missing apiServer.extraArgs")
	default:
		return nil, fmt.Errorf("kubeadm apiServer.extraArgs has unsupported type %T", extraArgs)
	}
	return args, nil
}

func kubeadmArgValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	default:
		return "", false
	}
}

// adminKubeconfigSecret writes a short-lived Secret holding a kubeconfig the
// verifier pods can actually use, and returns its name.
//
// The published <cluster>-kubeconfig cannot be mounted directly: on an OIDC
// cluster it authenticates through the SPIRE exec plugin, which is not in the
// kubectl image and needs a Workload API socket the pod does not have. kubectl
// then fails with no message, which is how these two specs failed.
func adminKubeconfigSecret(ctx context.Context, test *tenantTest) string {
	GinkgoHelper()

	kubeconfig := expectSucceed(test.tenantClients.AdminKubeconfig(ctx, test.cluster.Name))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-admin-kubeconfig-",
			Namespace:    mgmtns.Namespace(),
		},
		Data: map[string][]byte{"value": kubeconfig},
	}
	Expect(test.k8sClient.Create(ctx, secret)).To(Succeed(), "should create the verifier kubeconfig secret")
	DeferCleanup(func(ctx context.Context) {
		_ = test.k8sClient.Delete(ctx, secret)
	})
	return secret.Name
}

// runEtcdEncryptionVerifier runs the verifier script as a pod on the management
// cluster. The script retries each call through the tenant's control-plane
// endpoint for up to about a minute before it fails and runs its diagnostics,
// so the pod gets well over two minutes. A failure carries the pod's log tail.
func runEtcdEncryptionVerifier(ctx context.Context, c client.Client, clientset kubernetes.Interface, kubeconfigSecret string) error {
	namespace := mgmtns.Namespace()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-etcd-encryption-",
			Namespace:    namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "verify",
				Image:   kube.KubectlImage,
				Command: []string{"sh", "-c", etcdEncryptionVerifyScript},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "kubeconfig",
					MountPath: "/kubeconfig",
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "kubeconfig",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: kubeconfigSecret,
				}},
			}},
		},
	}
	if _, _, err := kube.RunPodWithLogs(ctx, c, clientset, pod, "verify", 5*time.Minute); err != nil {
		return fmt.Errorf("running etcd encryption verifier pod: %w", err)
	}
	return nil
}

// The test scripts are embedded for the same reason as the fixtures below.
//
//go:embed testdata/kube_apiserver_restart_counts.sh
var kubeAPIServerRestartCountScript string

//go:embed testdata/etcd_encryption_key_rotation.sh
var etcdEncryptionKeyRotationScript string

//go:embed testdata/etcd_encryption_verify.sh
var etcdEncryptionVerifyScript string

//go:embed testdata/fake_etcd_verifier_kubectl.sh
var fakeEtcdVerifierKubectl string

// The CAPD entrypoint copies only the compiled test binary into its inner
// runner, so captured fixtures must not depend on the process working directory.
//
//go:embed testdata/secretbox-transformation-metrics.prom
var secretboxMetricsFixture string

//go:embed testdata/secretbox-transformation-metrics-v1.29.prom
var legacySecretboxMetricsFixture string

func TestValidateKubeadmEncryptionConfig(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core Kubernetes types to scheme: %v", err)
	}

	tests := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{
			name: "v1beta3 map",
			config: `apiServer:
  extraArgs:
    encryption-provider-config: /etc/kubernetes/kube-api-encryption/encryption-config.yaml
    encryption-provider-config-automatic-reload: true
apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
`,
		},
		{
			name: "v1beta4 list with preceding arguments",
			config: `apiServer:
  extraArgs:
  - name: audit-log-maxage
    value: "30"
  - name: bind-address
    value: 0.0.0.0
  - name: encryption-provider-config
    value: /etc/kubernetes/kube-api-encryption/encryption-config.yaml
  - name: encryption-provider-config-automatic-reload
    value: "true"
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
`,
		},
		{
			name: "missing encryption provider argument",
			config: `apiServer:
  extraArgs:
  - name: encryption-provider-config-automatic-reload
    value: "true"
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
`,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: kubeadmConfigMapNamespace,
					Name:      kubeadmConfigMapName,
				},
				Data: map[string]string{kubeadmConfigDataKey: test.config},
			}
			tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
			err := validateKubeadmEncryptionConfig(context.Background(), tenantClient)
			if test.wantErr && err == nil {
				t.Fatal("validateKubeadmEncryptionConfig() error = nil, want error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateKubeadmEncryptionConfig() error = %v", err)
			}
		})
	}
}

func TestEtcdEncryptionVerifyScript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		environment     []string
		wantSuccess     bool
		wantLogContains []string
		wantProviders   string
	}{
		{
			name:            "encrypted Secret succeeds",
			wantSuccess:     true,
			wantLogContains: []string{"raw etcd Secret encryption verified"},
		},
		{
			name:            "verifier Secret create succeeds on a retry",
			environment:     []string{"FAKE_CREATE_FAILURES=1"},
			wantSuccess:     true,
			wantLogContains: []string{"raw etcd Secret encryption verified"},
		},
		{
			name:            "verifier Secret create fails on every attempt",
			environment:     []string{"FAKE_CREATE_FAILURES=99"},
			wantLogContains: []string{"failed to create the verifier Secret", "kubectl error: Unable to connect to the server: fake connection reset by peer"},
		},
		{
			name:            "etcd pod list fails",
			environment:     []string{"FAKE_ETCD_LIST_STATUS=1"},
			wantLogContains: []string{"failed to list etcd pods"},
		},
		{
			name:            "no etcd pod exists",
			environment:     []string{"FAKE_ETCD_POD="},
			wantLogContains: []string{"no etcd pod found"},
		},
		{
			name:            "etcd value read fails",
			environment:     []string{"FAKE_ETCD_READ_STATUS=1"},
			wantLogContains: []string{"failed to read the raw etcd Secret value"},
		},
		{
			name:            "etcd key is missing or empty",
			environment:     []string{"FAKE_ETCD_VALUE=empty"},
			wantLogContains: []string{"raw etcd Secret key is missing or its value is empty"},
		},
		{
			name:            "plaintext marker is present",
			environment:     []string{"FAKE_ETCD_VALUE=plaintext"},
			wantLogContains: []string{"raw etcd Secret contains the plaintext marker"},
		},
		{
			name:            "etcd value uses an unrecognized encoding",
			environment:     []string{"FAKE_ETCD_VALUE=unrecognized"},
			wantLogContains: []string{"raw etcd Secret does not contain the secretbox encryption prefix"},
		},
		{
			name:            "different effective provider remains visible",
			environment:     []string{"FAKE_ETCD_VALUE=unrecognized", "FAKE_PROVIDER_0=other"},
			wantLogContains: []string{"raw etcd Secret does not contain the secretbox encryption prefix"},
			wantProviders:   "kube-apiserver-0=other; kube-apiserver-1=identity",
		},
		{
			name:            "Kubernetes 1.29 metrics remain supported",
			environment:     []string{"FAKE_ETCD_VALUE=unrecognized", "FAKE_PROVIDER_0=secretbox-legacy"},
			wantLogContains: []string{"raw etcd Secret does not contain the secretbox encryption prefix"},
		},
		{
			name:            "missing transformation metric suggests absent config",
			environment:     []string{"FAKE_ETCD_VALUE=unrecognized", "FAKE_PROVIDER_0=none"},
			wantLogContains: []string{"raw etcd Secret does not contain the secretbox encryption prefix"},
			wantProviders:   "kube-apiserver-0=no-observed-transform(config-may-be-absent); kube-apiserver-1=identity",
		},
	}

	// One fake kubectl for all subtests, written before any of them forks. Each
	// subtest used to write its own and exec it at once while its siblings were
	// forking; a fork can hold another subtest's write descriptor open until it
	// execs, and the kernel then refuses to run that file (ETXTBSY). The
	// script's first kubectl call failed exactly that way in CI, twice.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "kubectl"), []byte(fakeEtcdVerifierKubectl), 0o700); err != nil {
		t.Fatalf("writing fake kubectl: %v", err)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			tempDir := t.TempDir()
			terminationLogPath := filepath.Join(tempDir, "termination.log")
			var stderr bytes.Buffer
			command := exec.CommandContext(t.Context(), "sh", "-c", etcdEncryptionVerifyScript)
			command.Stderr = &stderr
			command.Env = append(os.Environ(),
				"PATH="+binDir+":"+os.Getenv("PATH"),
				"E2E_KUBECONFIG_PATH="+filepath.Join(tempDir, "kubeconfig"),
				"E2E_TERMINATION_LOG="+terminationLogPath,
				"E2E_KUBECTL_RETRY_DELAY=0",
				"TMPDIR="+tempDir,
				"FAKE_STATE_DIR="+tempDir,
				"FAKE_SECRETBOX_METRICS="+secretboxMetricsFixture,
				"FAKE_SECRETBOX_LEGACY_METRICS="+legacySecretboxMetricsFixture,
				"FAKE_PROVIDER_0=secretbox",
				"FAKE_PROVIDER_1=identity",
			)
			command.Env = append(command.Env, test.environment...)
			command.Env = command.Environ()

			err := command.Run()
			if test.wantSuccess && err != nil {
				t.Fatalf("etcdEncryptionVerifyScript failed: %v; stderr: %q", err, stderr.String())
			}
			if !test.wantSuccess && err == nil {
				t.Fatal("etcdEncryptionVerifyScript succeeded, want failure")
			}

			terminationLog, readErr := os.ReadFile(terminationLogPath)
			if readErr != nil {
				t.Fatalf("reading termination log: %v", readErr)
			}
			logText := string(terminationLog)
			for _, want := range test.wantLogContains {
				if !strings.Contains(logText, want) {
					t.Errorf("termination log %q does not contain %q; stderr: %q", logText, want, stderr.String())
				}
			}
			if !test.wantSuccess {
				wantProviders := test.wantProviders
				if wantProviders == "" {
					wantProviders = "kube-apiserver-0=secretbox; kube-apiserver-1=identity"
				}
				if !strings.Contains(logText, wantProviders) {
					t.Errorf("termination log %q does not contain exact provider sequence %q", logText, wantProviders)
				}
			}
			for _, secretMaterial := range []string{
				"RAW_SECRET_SENTINEL",
				"nke-e2e-plaintext-e2e-etcd-encryption-",
				"nvke-key-1",
			} {
				if strings.Contains(logText, secretMaterial) {
					t.Errorf("termination log exposes Secret material %q: %q", secretMaterial, logText)
				}
			}
		})
	}
}
