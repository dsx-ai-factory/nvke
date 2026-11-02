// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func unschedulablePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "probe", Name: "gpu"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient nvidia.com/gpu."},
				{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}
}

func TestPodStatusDetails(t *testing.T) {
	if got, want := PodStatusDetails(unschedulablePod()), []string{
		"PodScheduled=False Unschedulable (0/3 nodes are available: 3 Insufficient nvidia.com/gpu.)",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("unschedulable pod:\n got %q\nwant %q", got, want)
	}

	pulling := &corev1.Pod{Status: corev1.PodStatus{
		Phase:   corev1.PodFailed,
		Reason:  "Evicted",
		Message: "The node was low on resource: memory.",
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  "setup",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:                 "probe",
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "mirror/probe:1"`}},
			RestartCount:         3,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
		}},
	}}
	if got, want := PodStatusDetails(pulling), []string{
		"pod Evicted (The node was low on resource: memory.)",
		"container setup exited 2 Error",
		`container probe waiting ImagePullBackOff (Back-off pulling image "mirror/probe:1")`,
		"container probe restarted 3 times, last exit 137 OOMKilled",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("failing containers:\n got %q\nwant %q", got, want)
	}

	if got := PodStatusDetails(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Error"}}); len(got) != 0 {
		t.Errorf("a reason with no message adds nothing to the phase line, got %q", got)
	}
}

func TestWarningEvents(t *testing.T) {
	at := func(minute int) metav1.Time {
		return metav1.NewTime(time.Date(2026, 10, 21, 12, minute, 0, 0, time.UTC))
	}
	events := []corev1.Event{
		{Type: corev1.EventTypeWarning, Reason: "Failed", Message: "pull failed", LastTimestamp: at(3), Count: 4},
		{Type: corev1.EventTypeNormal, Reason: "Scheduled", Message: "assigned", LastTimestamp: at(4)},
		{Type: corev1.EventTypeWarning, Reason: "FailedScheduling", Message: " no node ", LastTimestamp: at(1)},
		{Type: corev1.EventTypeWarning, Reason: "BackOff", Message: "backing off", EventTime: metav1.NewMicroTime(at(5).Time)},
	}
	if got, want := warningEvents(events, 2), []string{
		"event Failed: pull failed (x4)",
		"event BackOff: backing off",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("latest two Warning events, oldest first:\n got %q\nwant %q", got, want)
	}
}

// An identity that may not list events still gets the status details.
func TestWaitForPodTimeoutSaysWhy(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pod := unschedulablePod()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).WithStatusSubresource(pod).Build()
	_, err := WaitForPod(context.Background(), c, client.ObjectKeyFromObject(pod), 10*time.Millisecond)
	if err == nil {
		t.Fatal("WaitForPod returned no error for a pod that never ran")
	}
	for _, want := range []string{"timeout after", "phase Pending", "Insufficient nvidia.com/gpu"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
