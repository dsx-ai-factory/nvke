// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package tenantauth

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/kube"
)

const (
	// The upstream SPIFFE exec-credential plugin, fetched from GitHub Releases.
	pluginDownloadBaseURL = "https://github.com/spiffe/k8s-spiffe-workload-jwt-exec-auth/releases/download"
	pluginVersion         = "0.0.7"
	pluginAsset           = "k8s-spiffe-workload-jwt-exec-auth_Linux_x86_64.tar.gz"
	// pluginChecksums is goreleaser's per-release checksum file
	// (<project>_<version>_checksums.txt); there is no plain "checksums.txt".
	pluginChecksums   = "k8s-spiffe-workload-jwt-exec-auth_" + pluginVersion + "_checksums.txt"
	pluginInstallPath = "/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth"

	podGenerateName       = "e2e-verify-"
	downloadContainerName = "download"
	verifyContainerName   = "verify"

	// downloadScript fetches and sha256-verifies the exec plugin into the
	// shared binary volume.
	downloadScript = `set -e
cd /tmp
wget -qO "` + pluginAsset + `" "$URL/` + pluginAsset + `"
wget -qO checksums.txt "$URL/` + pluginChecksums + `"
grep " ` + pluginAsset + `$" checksums.txt | sha256sum -c -
tar xzf "` + pluginAsset + `"
install -m 0755 k8s-spiffe-workload-jwt-exec-auth ` + pluginInstallPath + `
`

	// verifyScript self-fetches the published kubeconfig with the pod's own SA
	// token, then runs auth whoami + can-i (which trigger the exec plugin / a
	// real JWT-SVID) and finally a real `kubectl get nodes` list call. An empty
	// whoami, a "no" result, or a failed list is a reported fact, not a script
	// failure. The parseable lines are written to /dev/termination-log so the
	// host can read them from the Pod object (a controller-runtime client cannot
	// stream logs).
	verifyScript = `set -e
kubectl get secret -n nke-system "${CLUSTER}-kubeconfig" \
  -o go-template='{{ index .data "value" | base64decode }}' > /work/kubeconfig
KC="--kubeconfig /work/kubeconfig"
set +e
exec > /dev/termination-log
echo "WHOAMI $(kubectl $KC auth whoami -o jsonpath='{.status.userInfo.username}' 2>/dev/null)"
for c in "$@"; do
  verb=${c%%:*}; rest=${c#*:}; res=${rest%%:*}; ns=${rest##*:}
  case "$ns" in
    "")  out=$(kubectl $KC auth can-i "$verb" "$res" 2>/dev/null); nsout="-" ;;
    "*") out=$(kubectl $KC auth can-i "$verb" "$res" --all-namespaces 2>/dev/null); nsout="*" ;;
    *)   out=$(kubectl $KC auth can-i "$verb" "$res" -n "$ns" 2>/dev/null); nsout="$ns" ;;
  esac
  echo "CANI $verb $res $nsout $out"
done
if kubectl $KC get nodes >/dev/null 2>&1; then echo "LISTNODES ok"; else echo "LISTNODES fail"; fi
`
)

// buildPod assembles the verifier Pod for identity id against clusterName,
// passing the encoded checks as args to the verify container.
func buildPod(id Identity, clusterName string, checks []Check) *corev1.Pod {
	automount := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: podGenerateName,
			Namespace:    id.Namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ServiceAccountName:           id.ServiceAccount,
			AutomountServiceAccountToken: &automount,
			// We use an init container to download the
			// k8s-spiffe-workload-jwt-exec-auth binary since it's not included
			// in the alpine image. We download to the emptyDir volume that gets
			// mounted in the main container in the same path the kubeconfig
			// expected the cred auth plugin to be
			// (/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth)
			InitContainers: []corev1.Container{
				{
					Name:    downloadContainerName,
					Image:   kube.KubectlImage,
					Command: []string{"sh", "-c", downloadScript},
					Env: []corev1.EnvVar{
						{Name: "URL", Value: fmt.Sprintf("%s/v%s", pluginDownloadBaseURL, pluginVersion)},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "binary", MountPath: "/opt/spiffe/bin"},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:    verifyContainerName,
					Image:   kube.KubectlImage,
					Command: append([]string{"sh", "-c", verifyScript, "verify"}, encodeChecks(checks)...),
					Env: []corev1.EnvVar{
						{Name: "CLUSTER", Value: clusterName},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "binary", MountPath: "/opt/spiffe/bin"},
						{Name: "workload-api", MountPath: "/spiffe-workload-api", ReadOnly: true},
						{Name: "work", MountPath: "/work"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "binary", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "workload-api", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
					Driver:   "csi.spiffe.io",
					ReadOnly: boolPtr(true),
				}}},
				{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
}

// encodeChecks renders each Check as "verb:resource:ns" where ns is empty / "*"
// / a namespace.
func encodeChecks(checks []Check) []string {
	args := make([]string, 0, len(checks))
	for _, c := range checks {
		args = append(args, strings.Join([]string{c.Verb, c.Resource, c.Namespace}, ":"))
	}
	return args
}

func boolPtr(b bool) *bool { return &b }
