// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
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
		Expect(runEtcdEncryptionVerifier(ctx, test.k8sClient, adminKubeconfigSecret(ctx, test))).To(Succeed())
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
		beforeRestarts := expectSucceed(readKubeAPIServerRestartCounts(ctx, test.k8sClient, adminKubeconfigSecret(ctx, test)))

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
		afterRestarts := expectSucceed(readKubeAPIServerRestartCounts(ctx, test.k8sClient, adminKubeconfigSecret(ctx, test)))
		Expect(afterRestarts).To(Equal(beforeRestarts))
		Expect(test.nke.AssertNodeConditions(
			ctx,
			test.cluster.ID,
			expectedNodeCounts(test.spec),
			nke.IsNodeReady,
		)).To(Succeed())

		By("Verifying Secrets remain encrypted in raw etcd after rotation")
		Expect(runEtcdEncryptionVerifier(ctx, test.k8sClient, adminKubeconfigSecret(ctx, test))).To(Succeed())
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

func readKubeAPIServerRestartCounts(ctx context.Context, c client.Client, kubeconfigSecret string) (string, error) {
	namespace := "nke-system"
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
			Namespace:    "nke-system",
		},
		Data: map[string][]byte{"value": kubeconfig},
	}
	Expect(test.k8sClient.Create(ctx, secret)).To(Succeed(), "should create the verifier kubeconfig secret")
	DeferCleanup(func(ctx context.Context) {
		_ = test.k8sClient.Delete(ctx, secret)
	})
	return secret.Name
}

func runEtcdEncryptionVerifier(ctx context.Context, c client.Client, kubeconfigSecret string) error {
	namespace := "nke-system"
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
	if _, _, err := kube.RunPod(ctx, c, pod, "verify", 2*time.Minute); err != nil {
		return fmt.Errorf("running etcd encryption verifier pod: %w", err)
	}
	return nil
}

const etcdEncryptionVerifyScript = `set -eu
kubeconfig_path="${E2E_KUBECONFIG_PATH:-/kubeconfig/value}"
termination_log="${E2E_TERMINATION_LOG:-/dev/termination-log}"
secret_name="e2e-etcd-encryption-$(date +%s)"
plaintext_marker="nke-e2e-plaintext-$secret_name"
secret_key="/registry/secrets/default/$secret_name"
raw_value_path="${TMPDIR:-/tmp}/raw-etcd-secret-$secret_name"
api_servers_path="${TMPDIR:-/tmp}/kube-apiserver-endpoints-$secret_name"
metrics_raw_path="${TMPDIR:-/tmp}/kube-apiserver-metrics-$secret_name"
metrics_before_path="${TMPDIR:-/tmp}/kube-apiserver-metrics-before-$secret_name"
metrics_after_path="${TMPDIR:-/tmp}/kube-apiserver-metrics-after-$secret_name"
diagnostic_secrets_path="${TMPDIR:-/tmp}/diagnostic-secrets-$secret_name"
etcd_pod="unknown"
cleanup() {
	if [ -f "$diagnostic_secrets_path" ]; then
		while IFS= read -r diagnostic_secret; do
			[ -n "$diagnostic_secret" ] || continue
			kubectl --kubeconfig "$kubeconfig_path" delete secret "$diagnostic_secret" \
				-n default --ignore-not-found >/dev/null 2>&1 || true
		done < "$diagnostic_secrets_path"
	fi
	rm -f "$raw_value_path" "$api_servers_path" "$metrics_raw_path" \
		"$metrics_before_path" "$metrics_after_path" "$diagnostic_secrets_path"
	kubectl --kubeconfig "$kubeconfig_path" delete secret "$secret_name" -n default --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

secret_write_counts() {
	api_server="$1"
	output_path="$2"
	if ! kubectl --kubeconfig "$kubeconfig_path" --server="$api_server" \
		--request-timeout=10s get --raw /metrics > "$metrics_raw_path" 2>/dev/null; then
		rm -f "$metrics_raw_path"
		return 1
	fi

	# Kubernetes records the transformer used for each write. Reduce the
	# provider-specific prefix to a provider name so key names never reach logs.
	if ! awk '
		/^apiserver_storage_transformation_operations_total\{/ &&
		/transformation_type="to_storage"/ &&
		/status="OK"/ {
			if ($0 ~ /resource="/ && $0 !~ /resource="secrets"/) {
				next
			}
			marker = "transformer_prefix=\""
			start = index($0, marker)
			if (start == 0) {
				next
			}
			rest = substr($0, start + length(marker))
			finish = index(rest, "\"")
			if (finish == 0) {
				next
			}
			prefix = substr(rest, 1, finish - 1)
			if (prefix == "") {
				identity += $2
			} else if (index(prefix, "k8s:enc:secretbox:v1:") == 1) {
				secretbox += $2
			} else if (index(prefix, "k8s:enc:") == 1) {
				other += $2
			}
		}
		END {
			printf "identity %.17g\n", identity
			printf "secretbox %.17g\n", secretbox
			printf "other %.17g\n", other
		}
	' "$metrics_raw_path" > "$output_path"; then
		rm -f "$metrics_raw_path" "$output_path"
		return 1
	fi
	rm -f "$metrics_raw_path"
}

provider_delta() {
	awk '
		NR == FNR {
			before[$1] = $2
			next
		}
		$2 > before[$1] {
			if (providers != "") {
				providers = providers "+"
			}
			providers = providers $1
		}
		END {
			if (providers == "") {
				providers = "no-observed-transform(config-may-be-absent)"
			}
			printf "%s", providers
		}
	' "$metrics_before_path" "$metrics_after_path"
}

effective_write_providers() {
	api_servers=""
	if ! api_servers="$(kubectl --kubeconfig "$kubeconfig_path" get pods -n kube-system \
		-l component=kube-apiserver \
		-o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.status.podIP}{"\n"}{end}' 2>/dev/null)"; then
		printf 'unknown'
		return
	fi
	if [ -z "$api_servers" ]; then
		printf 'unknown'
		return
	fi

	printf '%s\n' "$api_servers" | sort > "$api_servers_path"
	providers=""
	probe_index=0
	while IFS='|' read -r api_pod api_ip; do
		[ -n "$api_pod" ] || continue
		probe_index=$((probe_index + 1))
		provider="unknown"
		if [ -z "$api_ip" ]; then
			provider="missing-pod-ip"
		else
			case "$api_ip" in
				*:*) api_server="https://[$api_ip]:6443" ;;
				*) api_server="https://$api_ip:6443" ;;
			esac
			diagnostic_secret="${secret_name}-api-${probe_index}"
			if ! secret_write_counts "$api_server" "$metrics_before_path"; then
				provider="metrics-before-error"
			elif ! kubectl --kubeconfig "$kubeconfig_path" --server="$api_server" \
				--request-timeout=10s create secret generic "$diagnostic_secret" \
				-n default --from-literal=probe="provider-diagnostic" >/dev/null 2>&1; then
				provider="write-error"
			else
				printf '%s\n' "$diagnostic_secret" >> "$diagnostic_secrets_path"
				if ! secret_write_counts "$api_server" "$metrics_after_path"; then
					provider="metrics-after-error"
				else
					provider="$(provider_delta)"
				fi
			fi
		fi
		if [ -n "$providers" ]; then
			providers="$providers; "
		fi
		providers="${providers}${api_pod}=${provider}"
	done < "$api_servers_path"
	printf '%s' "${providers:-unknown}"
}

fail_verification() {
	reason="$1"
	providers="$(effective_write_providers)"
	printf '%s (etcd pod: %s; effective write providers: %s)\n' \
		"$reason" "$etcd_pod" "$providers" > "$termination_log"
	exit 1
}

if ! kubectl --kubeconfig "$kubeconfig_path" create secret generic "$secret_name" \
	-n default --from-literal=probe="$plaintext_marker" >/dev/null; then
	fail_verification "failed to create the verifier Secret"
fi

# items[*], not items[0]: indexing an empty list is an error, so a cluster with
# no etcd pod took the branch below and reported a failed list. Both causes
# reached one message and the "no etcd pod" branch was unreachable.
if ! etcd_pods="$(kubectl --kubeconfig "$kubeconfig_path" get pods -n kube-system \
	-l component=etcd -o jsonpath='{.items[*].metadata.name}')"; then
	etcd_pod="unknown"
	fail_verification "failed to list etcd pods"
fi
etcd_pod="${etcd_pods%% *}"
if [ -z "$etcd_pod" ]; then
	etcd_pod="unknown"
	fail_verification "no etcd pod found"
fi

etcdctl_for_secret() {
	kubectl --kubeconfig "$kubeconfig_path" exec -n kube-system "$etcd_pod" -c etcd -- \
		etcdctl \
			--endpoints=https://127.0.0.1:2379 \
			--cacert=/etc/kubernetes/pki/etcd/ca.crt \
			--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
			--key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
			get "$secret_key" "$@"
}

# Keep the binary protobuf value only in a temporary file so the etcdctl exit
# status can be checked independently from the content checks. The EXIT trap
# removes the file, and neither the value nor etcdctl stderr is ever logged.
if ! etcdctl_for_secret --print-value-only > "$raw_value_path" 2>/dev/null; then
	fail_verification "failed to read the raw etcd Secret value"
fi
if [ ! -s "$raw_value_path" ]; then
	fail_verification "raw etcd Secret key is missing or its value is empty"
fi
if grep -a -F -q "$plaintext_marker" "$raw_value_path"; then
	fail_verification "raw etcd Secret contains the plaintext marker"
fi
if ! grep -a -q 'k8s:enc:secretbox:v1:' "$raw_value_path"; then
	fail_verification "raw etcd Secret does not contain the secretbox encryption prefix"
fi

echo "raw etcd Secret encryption verified" > "$termination_log"
`

const fakeEtcdVerifierKubectl = `#!/bin/sh
set -eu
args="$*"
secret_name="unknown"
api_server=""
for arg in "$@"; do
	case "$arg" in
		/registry/secrets/default/*)
			secret_name="${arg##*/}"
			;;
		--server=*)
			api_server="${arg#--server=}"
			;;
	esac
done

case "$args" in
	*" delete secret "*)
		exit 0
		;;
	*" create secret generic "*)
		if [ -n "$api_server" ]; then
			case "$api_server" in
				*10.0.0.10*) server_index=0 ;;
				*10.0.0.11*) server_index=1 ;;
				*) exit 98 ;;
			esac
			: > "${FAKE_STATE_DIR}/server-${server_index}"
		fi
		exit "${FAKE_CREATE_STATUS:-0}"
		;;
	*" get --raw /metrics"*)
		case "$api_server" in
			*10.0.0.10*)
				server_index=0
				provider="${FAKE_PROVIDER_0:-secretbox}"
				;;
			*10.0.0.11*)
				server_index=1
				provider="${FAKE_PROVIDER_1:-identity}"
				;;
			*)
				exit 97
				;;
		esac
		if [ ! -f "${FAKE_STATE_DIR}/server-${server_index}" ]; then
			exit 0
		fi
		case "$provider" in
			identity)
				printf '%s\n' \
					'apiserver_storage_transformation_operations_total{resource="secrets",status="OK",transformation_type="to_storage",transformer_prefix=""} 1'
				;;
			secretbox)
				printf '%s' "$FAKE_SECRETBOX_METRICS"
				;;
			secretbox-legacy)
				printf '%s' "$FAKE_SECRETBOX_LEGACY_METRICS"
				;;
			other)
				printf '%s\n' \
					'apiserver_storage_transformation_operations_total{resource="secrets",status="OK",transformation_type="to_storage",transformer_prefix="k8s:enc:aescbc:v1:"} 1' \
					'apiserver_storage_transformation_operations_total{resource="secrets",status="OK",transformation_type="to_storage",transformer_prefix="nke-key-1:"} 1'
				;;
		esac
		;;
	*" get pods -n kube-system -l component=etcd "*)
		if [ "${FAKE_ETCD_LIST_STATUS:-0}" -ne 0 ]; then
			exit "$FAKE_ETCD_LIST_STATUS"
		fi
		# No colon, so an empty FAKE_ETCD_POD models a cluster with no etcd pod
		# rather than falling back to the default.
		etcd_pod_out="${FAKE_ETCD_POD-etcd-0}"
		# Real kubectl treats an out-of-range index as an error, so items[0]
		# against an empty list exits non-zero instead of printing nothing.
		# Without this the empty case is indistinguishable from a healthy one.
		if [ -z "$etcd_pod_out" ]; then
			case "$args" in
				*'items[0]'*)
					printf '%s\n' 'error: array index out of bounds' >&2
					exit 1
					;;
			esac
		fi
		printf '%s' "$etcd_pod_out"
		;;
	*" get pods -n kube-system -l component=kube-apiserver "*)
		printf '%s\n' 'kube-apiserver-1|10.0.0.11' 'kube-apiserver-0|10.0.0.10'
		;;
	*" --count-only"*)
		printf '%s\n' 'Error: --count-only is only for --write-out=fields' >&2
		exit 128
		;;
	*" --print-value-only"*)
		if [ "${FAKE_ETCD_READ_STATUS:-0}" -ne 0 ]; then
			exit "$FAKE_ETCD_READ_STATUS"
		fi
		case "${FAKE_ETCD_VALUE:-encrypted}" in
			empty)
				;;
			encrypted)
				printf 'binary-prefix-k8s:enc:secretbox:v1:encrypted-payload'
				;;
			plaintext)
				printf 'RAW_SECRET_SENTINEL-nke-e2e-plaintext-%s' "$secret_name"
				;;
			unrecognized)
				printf 'RAW_SECRET_SENTINEL-unrecognized-encoding'
				;;
		esac
		;;
	*)
		printf 'unexpected fake kubectl invocation: %s\n' "$args" >&2
		exit 99
		;;
esac
`

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

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			tempDir := t.TempDir()
			kubectlPath := filepath.Join(tempDir, "kubectl")
			if err := os.WriteFile(kubectlPath, []byte(fakeEtcdVerifierKubectl), 0o700); err != nil {
				t.Fatalf("writing fake kubectl: %v", err)
			}
			terminationLogPath := filepath.Join(tempDir, "termination.log")
			command := exec.CommandContext(t.Context(), "sh", "-c", etcdEncryptionVerifyScript)
			command.Env = append(os.Environ(),
				"PATH="+tempDir+":"+os.Getenv("PATH"),
				"E2E_KUBECONFIG_PATH="+filepath.Join(tempDir, "kubeconfig"),
				"E2E_TERMINATION_LOG="+terminationLogPath,
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
				t.Fatalf("etcdEncryptionVerifyScript failed: %v", err)
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
					t.Errorf("termination log %q does not contain %q", logText, want)
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
				"nke-key-1",
			} {
				if strings.Contains(logText, secretMaterial) {
					t.Errorf("termination log exposes Secret material %q: %q", secretMaterial, logText)
				}
			}
		})
	}
}
