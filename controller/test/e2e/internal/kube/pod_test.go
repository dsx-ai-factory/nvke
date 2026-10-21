// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// terminalVerifierPod is a one-shot verifier pod whose container has already
// terminated. The fake client keeps the status a pod is created with, so
// RunPodWithLogs sees it finished on its first poll.
func terminalVerifierPod(phase corev1.PodPhase, terminated *corev1.ContainerStateTerminated) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-etcd-encryption-knvqc", Namespace: "nvke-system"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "verify", Image: KubectlImage}},
		},
		Status: corev1.PodStatus{
			Phase: phase,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "verify",
				State: corev1.ContainerState{Terminated: terminated},
			}},
		},
	}
}

func TestRunPodWithLogs(t *testing.T) {
	t.Parallel()

	const containerLog = "kubectl create failed on attempt 5 of 5:\nUnable to connect to the server: read tcp: connection reset by peer\n"

	tests := []struct {
		name       string
		phase      corev1.PodPhase
		terminated *corev1.ContainerStateTerminated
		// wantErr lists substrings the error must contain in this order; nil
		// wants success.
		wantErr      []string
		wantLogReads int
	}{
		{
			name:       "a failed pod reports its termination message and then the log tail",
			phase:      corev1.PodFailed,
			terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: "failed to create the verifier Secret"},
			wantErr: []string{
				"verifier pod nvke-system/e2e-etcd-encryption-knvqc failed: exitCode=1 reason=Error message=failed to create the verifier Secret",
				"; log tail:\n" + strings.TrimSpace(containerLog),
			},
			wantLogReads: 1,
		},
		{
			name:       "a succeeded pod reads no log",
			phase:      corev1.PodSucceeded,
			terminated: &corev1.ContainerStateTerminated{Reason: "Completed", Message: "raw etcd Secret encryption verified"},
		},
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding core/v1 to the scheme: %v", err)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logReads := 0
			clientset := kubernetesfake.NewClientset()
			clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "log" {
					return false, nil, nil
				}
				logReads++
				generic, ok := action.(k8stesting.GenericAction)
				if !ok {
					t.Fatalf("log action is a %T, want a GenericAction carrying the options", action)
				}
				options, ok := generic.GetValue().(*corev1.PodLogOptions)
				if !ok || options.Container != "verify" || options.TailLines == nil || *options.TailLines != PodLogTailLines {
					t.Errorf("GetLogs options = %+v, want the verify container and the last %d lines", options, PodLogTailLines)
				}
				return true, &runtime.Unknown{Raw: []byte(containerLog)}, nil
			})

			runtimeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			pod := terminalVerifierPod(tc.phase, tc.terminated)
			_, message, err := RunPodWithLogs(t.Context(), runtimeClient, clientset, pod, "verify", time.Minute)
			if message != tc.terminated.Message {
				t.Errorf("termination message = %q, want %q", message, tc.terminated.Message)
			}
			if logReads != tc.wantLogReads {
				t.Errorf("log reads = %d, want %d", logReads, tc.wantLogReads)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("RunPodWithLogs() error = %v, want success", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RunPodWithLogs() error = nil, want the failure")
			}
			rest := err.Error()
			for _, want := range tc.wantErr {
				index := strings.Index(rest, want)
				if index < 0 {
					t.Fatalf("error %q does not contain %q after the preceding parts", err, want)
				}
				rest = rest[index+len(want):]
			}
		})
	}
}
