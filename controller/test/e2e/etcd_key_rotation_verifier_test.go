// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
)

func verifyPreexistingSecretRotation(ctx context.Context, c client.Client, kubeconfigSecret, secretName, previousHeader string) (string, error) {
	mode := "before"
	if previousHeader != "" {
		mode = "after"
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-etcd-rewrite-proof-", Namespace: mgmtns.Namespace()},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "verify", Image: kube.KubectlImage, Command: []string{"sh", "-c", etcdKeyRotationVerifyScript},
				Env: []corev1.EnvVar{
					{Name: "E2E_MODE", Value: mode},
					{Name: "E2E_SECRET_NAME", Value: secretName},
					{Name: "E2E_OLD_HEADER", Value: previousHeader},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "kubeconfig", MountPath: "/kubeconfig", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{Name: "kubeconfig", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: kubeconfigSecret},
			}}},
		},
	}
	_, message, err := kube.RunPod(ctx, c, pod, "verify", 3*time.Minute)
	if err != nil {
		return "", fmt.Errorf("verify preexisting Secret rotation: %w", err)
	}
	fingerprint := strings.TrimPrefix(strings.TrimSpace(message), "header-sha256=")
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("rotation verifier did not return a valid header fingerprint")
	}
	return fingerprint, nil
}

// The fixed payload is a public test marker. Neither raw ciphertext nor key
// names leave the pod: only a SHA256 of the public storage header is returned.
const etcdKeyRotationVerifyScript = `set -eu
umask 077
kubeconfig_path="${E2E_KUBECONFIG_PATH:-/kubeconfig/value}"
termination_log="${E2E_TERMINATION_LOG:-/dev/termination-log}"
mode="${E2E_MODE:?verification mode is required}"
secret_name="${E2E_SECRET_NAME:?preexisting Secret name is required}"
old_header="${E2E_OLD_HEADER:-}"
fixture_value="nvke-rotation-public-fixture"
fixture_base64="$(printf '%s' "$fixture_value" | base64 | tr -d '\n')"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/rotation-proof.XXXXXX")"
diagnostics="$workdir/diagnostics"
: > "$diagnostics"
cleanup() {
	while IFS= read -r name; do
		[ -n "$name" ] || continue
		kubectl --kubeconfig "$kubeconfig_path" delete secret "$name" -n default --ignore-not-found >/dev/null 2>&1 || true
	done < "$diagnostics"
	rm -rf "$workdir"
}
trap cleanup EXIT
fail() { printf '%s\n' "$1" > "$termination_log"; exit 1; }
case "$mode" in before|after) ;; *) fail 'unsupported verification mode' ;; esac
etcd_pods="$(kubectl --kubeconfig "$kubeconfig_path" get pods -n kube-system -l component=etcd -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)" || fail 'cannot list etcd members'
etcd_pod="${etcd_pods%% *}"
[ -n "$etcd_pod" ] || fail 'no etcd member available'

storage_header_hash() {
	name="$1"
	kubectl --kubeconfig "$kubeconfig_path" exec -n kube-system "$etcd_pod" -c etcd -- \
		etcdctl --endpoints=https://127.0.0.1:2379 \
		--cacert=/etc/kubernetes/pki/etcd/ca.crt \
		--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
		--key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
		get "/registry/secrets/default/$name" --print-value-only > "$workdir/raw" 2>/dev/null || return 1
	[ -s "$workdir/raw" ] || return 1
	if grep -a -F -q "$fixture_value" "$workdir/raw"; then return 1; fi
	# The first five colon-separated fields identify provider/version/key name.
	# Cut at the first NUL or newline so ciphertext never reaches cut; BusyBox
	# tr lacks character classes. Nothing raw is printed. Key names are public
	# metadata but we return their digest only.
	header="$(LC_ALL=C head -c 128 "$workdir/raw" | tr '\000' '\n' | head -n 1 | cut -d : -f 1-5)"
	printf '%s' "$header" | grep -Eq '^k8s:enc:secretbox:v1:[A-Za-z0-9_.-]+$' || return 1
	printf '%s' "$header" | sha256sum | cut -d ' ' -f 1
}
header_hash="$(storage_header_hash "$secret_name")" || fail 'preexisting Secret has missing, plaintext, or unsupported storage'

if [ "$mode" = after ]; then
	[ -n "$old_header" ] || fail 'missing before-rotation header fingerprint'
	[ "$header_hash" != "$old_header" ] || fail 'preexisting Secret was not rewritten under a different key'
fi
kubectl --kubeconfig "$kubeconfig_path" get pods -n kube-system -l component=kube-apiserver \
	-o jsonpath='{range .items[*]}{.status.podIP}{"\n"}{end}' > "$workdir/apis" 2>/dev/null || fail 'cannot list API servers'
[ -s "$workdir/apis" ] || fail 'no API server available'
index=0
while IFS= read -r ip; do
	[ -n "$ip" ] || fail 'API server has no address'
	case "$ip" in *:*) server="https://[$ip]:6443" ;; *) server="https://$ip:6443" ;; esac
	value="$(kubectl --kubeconfig "$kubeconfig_path" --server="$server" --request-timeout=10s \
		get secret "$secret_name" -n default -o jsonpath='{.data.probe}' 2>/dev/null)" || fail 'an API server cannot read the preexisting Secret'
	[ "$value" = "$fixture_base64" ] || fail 'preexisting Secret payload changed'
	if [ "$mode" = after ]; then
		index=$((index + 1))
		probe="$secret_name-api-$index"
		printf '%s\n' "$probe" >> "$diagnostics"
		kubectl --kubeconfig "$kubeconfig_path" --server="$server" --request-timeout=10s \
			create secret generic "$probe" -n default --from-literal=probe="$fixture_value" >/dev/null 2>&1 || fail 'an API server cannot write an encrypted probe'
		probe_header="$(storage_header_hash "$probe")" || fail 'an API server wrote missing, plaintext, or unsupported storage'
		[ "$probe_header" = "$header_hash" ] || fail 'API servers have not converged on the rewritten Secret key'
	fi
done < "$workdir/apis"
printf 'header-sha256=%s' "$header_hash" > "$termination_log"
`

const fakeRotationVerifierKubectl = `#!/bin/sh
set -eu
args="$*"
name=""
server=""
for arg in "$@"; do
	case "$arg" in /registry/secrets/default/*) name="${arg##*/}" ;; --server=*) server="${arg#--server=}" ;; esac
done
case "$args" in
	*' get pods -n kube-system -l component=etcd '*) printf 'etcd-0' ;;
	*' get pods -n kube-system -l component=kube-apiserver '*) printf '10.0.0.10\n10.0.0.11\n' ;;
	*' get secret '*)
		[ "${FAKE_READ_FAIL:-0}" = 0 ] || exit 1
		printf '%s' "${FAKE_PAYLOAD:-bnZrZS1yb3RhdGlvbi1wdWJsaWMtZml4dHVyZQ==}"
		;;
	*' create secret generic '*)
		[ "${FAKE_WRITE_FAIL:-0}" = 0 ] || exit 1
		printf '%s\n' "$server" >> "$FAKE_WRITES"
		;;
	*' delete secret '*) exit 0 ;;
	*' --print-value-only'*)
		case "$name" in *-api-2) key="${FAKE_SECOND_KEY:-new-key}" ;; *-api-*) key='new-key' ;; *) key="${FAKE_PRIOR_KEY:-new-key}" ;; esac
		if [ "${FAKE_PLAINTEXT:-0}" = 1 ]; then printf 'nvke-rotation-public-fixture'; else printf 'k8s:enc:secretbox:v1:%s:\000CIPHERTEXT_SENTINEL' "$key"; fi
		;;
	*) exit 99 ;;
esac
`

func TestEtcdKeyRotationVerifierRequiresExistingSecretRewriteAndConvergence(t *testing.T) {
	old := sha256.Sum256([]byte("k8s:enc:secretbox:v1:old-key"))
	for _, test := range []struct {
		name    string
		env     []string
		before  bool
		success bool
		want    string
	}{
		{name: "preexisting Secret checked before rotation", before: true, success: true},
		{name: "rewritten and all API servers converged", success: true},
		{name: "old Secret never rewritten", env: []string{"FAKE_PRIOR_KEY=old-key"}, want: "not rewritten"},
		{name: "one API server retained old primary", env: []string{"FAKE_SECOND_KEY=old-key"}, want: "not converged"},
		{name: "Secret unreadable", env: []string{"FAKE_READ_FAIL=1"}, want: "cannot read"},
		{name: "Secret payload changed", env: []string{"FAKE_PAYLOAD=changed"}, want: "payload changed"},
		{name: "new writes fail", env: []string{"FAKE_WRITE_FAIL=1"}, want: "cannot write"},
		{name: "plaintext leaked into storage", env: []string{"FAKE_PLAINTEXT=1"}, want: "plaintext"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(fakeRotationVerifierKubectl), 0o700); err != nil {
				t.Fatal(err)
			}
			termination := filepath.Join(dir, "termination")
			mode := "after"
			if test.before {
				mode = "before"
			}
			cmd := exec.CommandContext(t.Context(), "sh", "-c", etcdKeyRotationVerifyScript)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMPDIR="+dir,
				"E2E_TERMINATION_LOG="+termination, "E2E_MODE="+mode, "E2E_SECRET_NAME=rotation-proof",
				fmt.Sprintf("E2E_OLD_HEADER=%x", old), "FAKE_WRITES="+filepath.Join(dir, "writes"))
			cmd.Env = append(cmd.Env, test.env...)
			output, err := cmd.CombinedOutput()
			if (err == nil) != test.success {
				t.Fatalf("success=%v, err=%v, output=%s", test.success, err, output)
			}
			log, err := os.ReadFile(termination)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(log), test.want) {
				t.Errorf("log %q does not contain %q", log, test.want)
			}
			for _, sensitive := range []string{"CIPHERTEXT_SENTINEL", "nvke-rotation-public-fixture", "old-key", "new-key"} {
				if strings.Contains(string(log)+string(output), sensitive) {
					t.Errorf("verifier leaked %s", sensitive)
				}
			}
			if test.before {
				if _, err := os.Stat(filepath.Join(dir, "writes")); !os.IsNotExist(err) {
					t.Fatal("before-rotation check wrote a new Secret")
				}
			}
		})
	}
}

// rotationSecretTraffic records API reads of a public preexisting fixture while
// the workflow rotates its key. The stop function waits for any in-flight read,
// so failed qualification cannot leave background requests behind.
type rotationSecretTraffic struct {
	Succeeded int
	Failed    int
}

func monitorRotationSecret(ctx context.Context, c client.Client, fixture *corev1.Secret) func() rotationSecretTraffic {
	trafficCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	result := rotationSecretTraffic{}
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			requestCtx, requestCancel := context.WithTimeout(trafficCtx, 5*time.Second)
			var secret corev1.Secret
			err := c.Get(requestCtx, client.ObjectKeyFromObject(fixture), &secret)
			requestCancel()
			if err != nil && trafficCtx.Err() != nil {
				return
			}
			if err != nil || !bytes.Equal(secret.Data["probe"], fixture.Data["probe"]) {
				result.Failed++
			} else {
				result.Succeeded++
			}
			select {
			case <-trafficCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() rotationSecretTraffic { cancel(); <-done; return result }
}

type rotationTrafficClient struct {
	client.Client
	observed chan struct{}
	reads    int
	fail     bool
}

func (c *rotationTrafficClient) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	c.reads++
	if c.reads == 2 {
		close(c.observed)
	}
	if c.fail {
		return errors.New("API read unavailable")
	}
	obj.(*corev1.Secret).Data = map[string][]byte{"probe": []byte("nvke-rotation-public-fixture")}
	return nil
}

func TestRotationSecretTrafficRecordsErrorsAndStops(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failed-reads-", fail), func(t *testing.T) {
			c := &rotationTrafficClient{observed: make(chan struct{}), fail: fail}
			fixture := &corev1.Secret{Data: map[string][]byte{"probe": []byte("nvke-rotation-public-fixture")}}
			stop := monitorRotationSecret(t.Context(), c, fixture)
			t.Cleanup(func() { stop() })
			select {
			case <-c.observed:
			case <-time.After(5 * time.Second):
				t.Fatal("traffic never made two API reads")
			}
			result := stop()
			if fail && result.Failed == 0 {
				t.Fatal("API failures were ignored")
			}
			if !fail && (result.Succeeded == 0 || result.Failed != 0) {
				t.Fatalf("unexpected traffic outcome: %+v", result)
			}
			if again := stop(); again != result {
				t.Fatal("stop did not return a stable result")
			}
		})
	}
}
