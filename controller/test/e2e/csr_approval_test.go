// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	kubeletPKIHostPath          = "/var/lib/kubelet/pki"
	kubeletConfigHostPath       = "/var/lib/kubelet/config.yaml"
	kubeletServingCurrentCert   = kubeletPKIHostPath + "/kubelet-server-current.pem"
	kubeletCSRApprovalReason    = "NKEValidatedNodeIdentity"
	kubeletCSRRotationContainer = "rotate-kubelet-serving-certificate"
)

func validateKubeletServingCSRApproval(test *tenantTest) {
	It("approves a new serving CSR created by a real kubelet", func(ctx context.Context) {
		workerPool := expectWorkerPool(ctx, test)
		worker := expectSucceed(test.nvke.WaitForReadyNodeInPool(ctx, workerPool.ID))
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))
		adminClient := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
		previousCSRs := expectSucceed(csrNames(ctx, tenantClient))

		By("Verifying serving certificate bootstrap is enabled and removing the kubelet's current serving-certificate link")
		Expect(triggerKubeletServingCertificateRotation(ctx, adminClient, worker.Name)).To(Succeed())

		By("Waiting for the tenant CSR controller to approve the new kubelet-serving CSR and for Kubernetes to issue its certificate")
		Eventually(func(ctx context.Context) error {
			return assertNewIssuedKubeletServingCSR(ctx, tenantClient, worker.Name, previousCSRs)
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(time.Second).Should(Succeed())

		By("Verifying the restarted kubelet returns to Ready")
		Eventually(func(ctx context.Context) error {
			var node corev1.Node
			if err := tenantClient.Get(ctx, client.ObjectKey{Name: worker.Name}, &node); err != nil {
				return fmt.Errorf("getting tenant Node %s: %w", worker.Name, err)
			}
			if ready, detail := kubernetesNodeReady(&node); !ready {
				return fmt.Errorf("tenant Node %s is not Ready after kubelet certificate rotation: %s", worker.Name, detail)
			}
			return nil
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	}, Label("lifecycle", "post-ready", "csr-approval"), NodeTimeout(10*time.Minute))
}

func csrNames(ctx context.Context, tenantClient client.Client) (map[string]struct{}, error) {
	var csrs certificatesv1.CertificateSigningRequestList
	if err := tenantClient.List(ctx, &csrs); err != nil {
		return nil, fmt.Errorf("listing tenant CSRs before kubelet certificate rotation: %w", err)
	}
	names := make(map[string]struct{}, len(csrs.Items))
	for i := range csrs.Items {
		names[csrs.Items[i].Name] = struct{}{}
	}
	return names, nil
}

func triggerKubeletServingCertificateRotation(ctx context.Context, adminClient client.Client, nodeName string) error {
	privileged := true
	hostPathType := corev1.HostPathDirectory
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-kubelet-serving-csr-",
			Namespace:    corev1.NamespaceDefault,
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
			Containers: []corev1.Container{{
				Name:  kubeletCSRRotationContainer,
				Image: kube.KubectlImage,
				Command: []string{
					"/bin/sh",
					"-ec",
				},
				Args: []string{fmt.Sprintf(`
grep -Eq '^serverTLSBootstrap:[[:space:]]*true[[:space:]]*$' /host%s
test -L /host%s
old_target="$(readlink /host%s)"
rm -f /host%s
chroot /host /usr/bin/systemctl restart kubelet
printf 'removed serving certificate link %%s and restarted kubelet\n' "$old_target" > /dev/termination-log
`, kubeletConfigHostPath, kubeletServingCurrentCert, kubeletServingCurrentCert, kubeletServingCurrentCert)},
				SecurityContext: &corev1.SecurityContext{
					Privileged: &privileged,
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "host-root",
					MountPath: "/host",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "host-root",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
					Path: "/",
					Type: &hostPathType,
				}},
			}},
		},
	}

	_, _, err := kube.RunPod(ctx, adminClient, pod, kubeletCSRRotationContainer, 2*time.Minute)
	if err != nil {
		return fmt.Errorf("triggering kubelet serving certificate rotation on %s: %w", nodeName, err)
	}
	return nil
}

func assertNewIssuedKubeletServingCSR(
	ctx context.Context,
	tenantClient client.Client,
	nodeName string,
	previousCSRs map[string]struct{},
) error {
	var csrs certificatesv1.CertificateSigningRequestList
	if err := tenantClient.List(ctx, &csrs); err != nil {
		return fmt.Errorf("listing tenant CSRs after kubelet certificate rotation: %w", err)
	}

	username := "system:node:" + nodeName
	for i := range csrs.Items {
		csr := &csrs.Items[i]
		if _, existed := previousCSRs[csr.Name]; existed ||
			csr.Spec.SignerName != certificatesv1.KubeletServingSignerName ||
			csr.Spec.Username != username {
			continue
		}

		if condition := csrCondition(csr, certificatesv1.CertificateDenied); condition != nil {
			return fmt.Errorf("new kubelet-serving CSR %s was denied: reason=%q message=%q", csr.Name, condition.Reason, condition.Message)
		}
		approved := csrCondition(csr, certificatesv1.CertificateApproved)
		if approved == nil {
			return fmt.Errorf("new kubelet-serving CSR %s is still pending", csr.Name)
		}
		if approved.Reason != kubeletCSRApprovalReason {
			return fmt.Errorf("new kubelet-serving CSR %s was approved by %q, want %q", csr.Name, approved.Reason, kubeletCSRApprovalReason)
		}
		if len(csr.Status.Certificate) == 0 {
			return fmt.Errorf("new kubelet-serving CSR %s is approved but has no issued certificate", csr.Name)
		}
		return nil
	}
	return fmt.Errorf("kubelet %s has not created a new serving CSR", nodeName)
}

func csrCondition(
	csr *certificatesv1.CertificateSigningRequest,
	conditionType certificatesv1.RequestConditionType,
) *certificatesv1.CertificateSigningRequestCondition {
	index := slices.IndexFunc(csr.Status.Conditions, func(condition certificatesv1.CertificateSigningRequestCondition) bool {
		return condition.Type == conditionType && condition.Status == corev1.ConditionTrue
	})
	if index < 0 {
		return nil
	}
	return &csr.Status.Conditions[index]
}
