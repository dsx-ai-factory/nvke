// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/kube"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

func validateEtcdEncryption(test *tenantTest) {
	validateEtcdEncryptionConfiguration(test)
	validateEtcdEncryptionKeyRotation(test)
}

func validateBrownfieldEtcdEncryption(test *tenantTest) {
	It("enables etcd encryption on an existing cluster", func(ctx context.Context) {
		Expect(test.cluster.EtcdEncrypted).To(BeFalse(), "cluster should initially be unencrypted")

		By("Waiting for every desired node to be ready")
		Eventually(func(ctx context.Context) error {
			return test.nke.AssertNodeConditions(
				ctx,
				test.cluster.ID,
				expectedNodeCounts(test.spec),
				nke.IsNodeReady,
			)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(1*time.Second).
			Should(Succeed(), "all desired nodes should be ready before enabling etcd encryption")

		By("Enabling etcd encryption")
		resp := expectSucceed(test.nke.UpdateEtcdEncryption(ctx, test.cluster.ID, true))
		Expect(resp.StatusCode()).To(Equal(200), string(resp.Body))
		Expect(resp.JSON200).NotTo(BeNil())
		cluster := resp.JSON200
		Expect(cluster.EtcdEncrypted).To(BeFalse(), "encryption should remain false while enablement is in progress")
		Expect(cluster.Conditions.UpdateInProgress).NotTo(BeNil())
		Expect(cluster.Conditions.UpdateInProgress.Type).To(Equal(api.UpdateInProgressTypeEtcdEncryption))

		By("Waiting for etcd encryption enablement to complete")
		test.cluster = expectSucceed(test.nke.WaitForEtcdEncryptionEnabled(ctx, test.cluster.ID))
		Expect(test.cluster.EtcdEncrypted).To(BeTrue())
	}, Label("lifecycle", "encryption"), NodeTimeout(15*time.Minute))

	validateEtcdEncryptionConfiguration(test)

	It("rejects disabling etcd encryption", func(ctx context.Context) {
		By("Attempting to disable etcd encryption")
		resp := expectSucceed(test.nke.UpdateEtcdEncryption(ctx, test.cluster.ID, false))
		Expect(resp.StatusCode()).To(Equal(400), string(resp.Body))
		Expect(resp.JSON400).NotTo(BeNil())
		Expect(resp.JSON400.Message).To(Equal("disabling etcd encryption is not supported"))

		cluster := expectSucceed(test.nke.GetCluster(ctx, test.cluster.ID))
		Expect(cluster.EtcdEncrypted).To(BeTrue())
	}, Label("lifecycle", "encryption"), NodeTimeout(5*time.Minute))
}

func validateEtcdEncryptionConfiguration(test *tenantTest) {
	It("configures Kubernetes API encryption at rest", func(ctx context.Context) {
		By("Validating the kubeadm API encryption arguments")
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))
		Expect(validateKubeadmEncryptionConfig(ctx, tenantClient)).To(Succeed())

		By("Verifying a Secret is encrypted in raw etcd")
		Expect(runEtcdEncryptionVerifier(ctx, test.k8sClient, test.cluster.Name)).To(Succeed())
	}, Label("lifecycle", "encryption"), NodeTimeout(15*time.Minute))
}

func validateEtcdEncryptionKeyRotation(test *tenantTest) {
	It("rotates the etcd encryption key through the operator Temporal workflow", func(ctx context.Context) {
		By("Waiting for every desired node to be ready")
		Eventually(func(ctx context.Context) error {
			return test.nke.AssertNodeConditions(
				ctx,
				test.cluster.ID,
				expectedNodeCounts(test.spec),
				nke.IsNodeReady,
			)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(1*time.Second).
			Should(Succeed(), "all desired nodes should be ready before rotating the etcd encryption key")

		beforeKey := expectSucceed(readEtcdEncryptionKeyState(ctx, test.k8sClient, test.cluster.ID.String()))
		Expect(beforeKey.pending).To(BeFalse(), "a pending key should not exist before rotation")
		beforeRestarts := expectSucceed(readKubeAPIServerRestartCounts(ctx, test.k8sClient, test.cluster.Name))

		By("Starting the operator-only Temporal workflow")
		workflowID := expectSucceed(runEtcdEncryptionKeyRotation(ctx, test.k8sClient, test.cluster.ID.String()))
		GinkgoLogr.Info("Etcd encryption key rotation workflow completed", "workflowID", workflowID)

		By("Verifying the new key was promoted and temporary state was removed")
		afterKey := expectSucceed(readEtcdEncryptionKeyState(ctx, test.k8sClient, test.cluster.ID.String()))
		Expect(afterKey.fingerprint).NotTo(Equal(beforeKey.fingerprint), "active key fingerprint should change")
		Expect(afterKey.activeSince).To(BeNumerically(">", beforeKey.activeSince), "active key timestamp should advance")
		Expect(afterKey.pending).To(BeFalse(), "pending key should be removed after rotation")

		cluster := expectSucceed(test.nke.GetCluster(ctx, test.cluster.ID))
		Expect(cluster.EtcdEncrypted).To(BeTrue())
		Expect(cluster.Conditions.UpdateInProgress).To(BeNil())
		Expect(cluster.Conditions.UpdateFailed).To(BeNil())
		test.cluster = cluster

		By("Verifying the configuration update did not restart kube-apiserver")
		afterRestarts := expectSucceed(readKubeAPIServerRestartCounts(ctx, test.k8sClient, test.cluster.Name))
		Expect(afterRestarts).To(Equal(beforeRestarts))
		Expect(test.nke.AssertNodeConditions(
			ctx,
			test.cluster.ID,
			expectedNodeCounts(test.spec),
			nke.IsNodeReady,
		)).To(Succeed())

		By("Verifying Secrets remain encrypted in raw etcd after rotation")
		Expect(runEtcdEncryptionVerifier(ctx, test.k8sClient, test.cluster.Name)).To(Succeed())
	}, Label("encryption", "etcd-key-rotation"), NodeTimeout(20*time.Minute))
}

func validateEtcdEncryptionPostDelete(test *tenantTest) {
	It("removes the CWE etcd encryption key secret during cleanup", func(ctx context.Context) {
		key := client.ObjectKey{
			Namespace: envOrDefault("CWE_NAMESPACE", "cloud-workflow-engine"),
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
		Namespace: envOrDefault("CWE_NAMESPACE", "cloud-workflow-engine"),
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

func readKubeAPIServerRestartCounts(ctx context.Context, c client.Client, clusterName string) (string, error) {
	namespace := "cluster-" + clusterName
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
					SecretName: clusterName + "-kubeconfig",
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

const kubeAPIServerRestartCountScript = `set -eu
counts="$(kubectl --kubeconfig /kubeconfig/value get pods -n kube-system \
  -l component=kube-apiserver \
  -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.containerStatuses[?(@.name=="kube-apiserver")].restartCount}{"\n"}{end}' \
  | sort)"
if [ -z "$counts" ]; then
  echo "no kube-apiserver restart counts found" > /dev/termination-log
  exit 1
fi
printf '%s' "$counts" > /dev/termination-log
`

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
		{Name: "E2E_TEMPORAL_NAMESPACE", Value: envOrDefault("E2E_TEMPORAL_NAMESPACE", "cloud-workflow-engine")},
		{Name: "E2E_TEMPORAL_TASK_QUEUE", Value: envOrDefault("E2E_TEMPORAL_TASK_QUEUE", "cloud-workflow-engine-tasks")},
	}...)

	if _, _, err := kube.RunPod(ctx, c, pod, adminToolsContainer, 15*time.Minute); err != nil {
		return "", fmt.Errorf("running etcd encryption key rotation workflow: %w", err)
	}
	return workflowID, nil
}

const etcdEncryptionKeyRotationScript = `set -eu
if ! temporal workflow execute \
  --address "$E2E_TEMPORAL_ADDRESS" \
  --namespace "$E2E_TEMPORAL_NAMESPACE" \
  --task-queue "$E2E_TEMPORAL_TASK_QUEUE" \
  --type RotateEtcdEncryptionKey \
  --workflow-id "$E2E_WORKFLOW_ID" \
  --input "{\"clusterID\":\"$E2E_CLUSTER_ID\"}" \
  --execution-timeout 15m \
  --run-timeout 15m \
  >/tmp/temporal-output 2>&1; then
  cat /tmp/temporal-output > /dev/termination-log
  exit 1
fi
printf 'workflow %s completed' "$E2E_WORKFLOW_ID" > /dev/termination-log
`

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

func runEtcdEncryptionVerifier(ctx context.Context, c client.Client, clusterName string) error {
	namespace := "cluster-" + clusterName
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
					SecretName: clusterName + "-kubeconfig",
				}},
			}},
		},
	}
	if _, _, err := kube.RunPod(ctx, c, pod, "verify", 2*time.Minute); err != nil {
		return fmt.Errorf("running etcd encryption verifier pod: %w", err)
	}
	return nil
}

const etcdEncryptionVerifyScript = `set -eu
secret_name="e2e-etcd-encryption-$(date +%s)"
plaintext_marker="nke-e2e-plaintext-$secret_name"
cleanup() {
  kubectl --kubeconfig /kubeconfig/value delete secret "$secret_name" -n default --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

kubectl --kubeconfig /kubeconfig/value create secret generic "$secret_name" \
  -n default --from-literal=probe="$plaintext_marker" >/dev/null

etcd_pod="$(kubectl --kubeconfig /kubeconfig/value get pods -n kube-system \
  -l component=etcd -o jsonpath='{.items[0].metadata.name}')"
if [ -z "$etcd_pod" ]; then
  echo "no etcd pod found" > /dev/termination-log
  exit 1
fi

read_secret_from_etcd() {
  kubectl --kubeconfig /kubeconfig/value exec -n kube-system "$etcd_pod" -c etcd -- \
    etcdctl \
      --endpoints=https://127.0.0.1:2379 \
      --cacert=/etc/kubernetes/pki/etcd/ca.crt \
      --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
      --key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
      get "/registry/secrets/default/$secret_name" --print-value-only
}

# Read the raw etcd value as a byte stream. Do not store or print it: protobuf
# values can contain NUL bytes and may include Secret contents on failure.
if ! read_secret_from_etcd | grep -a -q 'k8s:enc:secretbox:v1:'; then
  echo "raw etcd Secret is missing the secretbox encryption prefix" > /dev/termination-log
  exit 1
fi
if read_secret_from_etcd | grep -a -F -q "$plaintext_marker"; then
  echo "raw etcd Secret contains the plaintext marker" > /dev/termination-log
  exit 1
fi

echo "raw etcd Secret encryption verified" > /dev/termination-log
`

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
