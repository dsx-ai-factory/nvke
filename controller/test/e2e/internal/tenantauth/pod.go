// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantauth

import (
	"fmt"
	"runtime"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
)

const (
	// The upstream SPIFFE exec-credential plugin, fetched from GitHub Releases.
	pluginDownloadBaseURL = "https://github.com/spiffe/k8s-spiffe-workload-jwt-exec-auth/releases/download"
	pluginVersion         = "0.3.0"
	// pluginChecksums is goreleaser's per-release checksum file
	// (<project>_<version>_checksums.txt); there is no plain "checksums.txt".
	pluginChecksums   = "k8s-spiffe-workload-jwt-exec-auth_" + pluginVersion + "_checksums.txt"
	pluginInstallPath = "/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth"

	podGenerateName       = "e2e-verify-"
	downloadContainerName = "download"
	verifyContainerName   = "verify"
	expectedUsernameEnv   = "EXPECTED_USERNAME"
	binaryVolumeName      = "binary"
	exchangeCAVolumeName  = "exchange-ca"
	exchangeCAPath        = "/etc/nke/dsx-identity/ca.crt"

	// downloadScript fetches and sha256-verifies the exec plugin into the
	// shared binary volume. ASSET and CHECKSUMS come in as env vars, set per
	// pod from the runner's own GOARCH -- the plugin ships separate x86_64
	// and arm64 builds under one checksums file.
	downloadScript = `set -e
cd /tmp
# Both fetches reach github.com, and busybox wget has no backoff of its own,
# so a 504 there fails the suite on the first try.
fetch() {
  for delay in 5 15 30 60 0; do
    wget -qO "$1" "$2" && return 0
    [ "${delay}" -eq 0 ] && return 1
    sleep "${delay}"
  done
}
fetch "$ASSET" "$URL/$ASSET"
fetch checksums.txt "$URL/$CHECKSUMS"
grep " $ASSET$" checksums.txt | sha256sum -c -
tar xzf "$ASSET"
install -m 0755 k8s-spiffe-workload-jwt-exec-auth ` + pluginInstallPath + `
`
)

// verifyScript returns the script that self-fetches the published kubeconfig with the pod's own SA
// token, then waits for the pod-specific SPIFFE registration before running
// can-i and a real `kubectl get nodes` list call. An empty whoami, a "no"
// result, or a failed list is a reported fact, not a script failure. The
// parseable lines are written to /dev/termination-log so the host can read
// them from the Pod object (a controller-runtime client cannot stream logs).
//
// The Secret lives in the site's management namespace, which is per-site
// configurable, so the lookup is built from the resolved value.
func verifyScript() string {
	return `set -e
kubectl get secret -n ` + mgmtns.Namespace() + ` "${CLUSTER}-kubeconfig" \
  -o go-template='{{ index .data "value" | base64decode }}' > /work/kubeconfig
KC="--kubeconfig /work/kubeconfig"
set +e
exec > /dev/termination-log
attempt=0
username=
while [ "$attempt" -lt 12 ]; do
  attempt=$((attempt + 1))
  username=$(kubectl $KC auth whoami -o jsonpath='{.status.userInfo.username}' 2>/dev/null)
  [ "$username" = "$EXPECTED_USERNAME" ] && break
  [ "$attempt" -lt 12 ] && sleep 5
done
echo "WHOAMI $username"
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
}

// buildPod assembles the verifier Pod for identity id against clusterName,
// passing the expected username and encoded checks to the verify container.
func buildPod(id Identity, clusterName, expectedUsername string, checks []Check) *corev1.Pod {
	return buildCredentialPod(
		id,
		clusterName,
		verifyScript(),
		[]corev1.EnvVar{{Name: expectedUsernameEnv, Value: expectedUsername}},
		encodeChecks(checks),
	)
}

func buildCredentialPod(id Identity, clusterName, script string, extraEnv []corev1.EnvVar, arguments []string) *corev1.Pod {
	automount := true
	pod := &corev1.Pod{
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
						{Name: "ASSET", Value: pluginAssetName()},
						{Name: "CHECKSUMS", Value: pluginChecksums},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: binaryVolumeName, MountPath: "/opt/spiffe/bin"},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:    verifyContainerName,
					Image:   kube.KubectlImage,
					Command: append([]string{"sh", "-c", script, "verify"}, arguments...),
					Env:     append([]corev1.EnvVar{{Name: "CLUSTER", Value: clusterName}}, extraEnv...),
					VolumeMounts: []corev1.VolumeMount{
						{Name: binaryVolumeName, MountPath: "/opt/spiffe/bin"},
						{Name: "workload-api", MountPath: "/spiffe-workload-api", ReadOnly: true},
						{Name: "work", MountPath: "/work"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: binaryVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "workload-api", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
					Driver:   "csi.spiffe.io",
					ReadOnly: boolPtr(true),
				}}},
				{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
	if id.CABundleConfigMap != "" {
		verify := &pod.Spec.Containers[0]
		verify.Env = append(verify.Env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: exchangeCAPath})
		verify.VolumeMounts = append(verify.VolumeMounts, corev1.VolumeMount{
			Name: exchangeCAVolumeName, MountPath: exchangeCAPath, SubPath: "ca.crt", ReadOnly: true,
		})
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: exchangeCAVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: id.CABundleConfigMap},
			}},
		})
	}
	return pod
}

// pluginAssetName maps the runner's own GOARCH to the plugin's release asset.
// The project's Linux builds are named x86_64/arm64, not Go's amd64/arm64,
// and one checksums.txt file covers every platform in the release.
func pluginAssetName() string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	}
	return fmt.Sprintf("k8s-spiffe-workload-jwt-exec-auth_Linux_%s.tar.gz", arch)
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
