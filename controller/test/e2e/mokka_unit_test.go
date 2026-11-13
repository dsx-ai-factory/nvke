// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func mokkaTestNode(name string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{mokkaLabelKey: "true"}}}
}

func mokkaTestPod(node string, ready bool, steps ...corev1.ContainerStatus) corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Pod{
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Conditions:            []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			InitContainerStatuses: steps,
		},
	}
}

func deletingPod(pod corev1.Pod) corev1.Pod {
	pod.DeletionTimestamp = &metav1.Time{}
	return pod
}

func validatorStep(name string, terminated *corev1.ContainerStateTerminated) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: terminated}}
}

func TestMokkaPodProblems(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes []corev1.Node
		pods  []corev1.Pod
		want  int
		found []string
	}{
		{
			name:  "every labeled node runs a Ready pod",
			nodes: []corev1.Node{mokkaTestNode("a"), mokkaTestNode("b")},
			pods:  []corev1.Pod{mokkaTestPod("a", true), mokkaTestPod("b", true)},
			want:  2,
		},
		{
			name:  "a pod that is not Ready",
			nodes: []corev1.Node{mokkaTestNode("a")},
			pods:  []corev1.Pod{mokkaTestPod("a", false)},
			want:  1,
			found: []string{"a: no Ready Mokka pod"},
		},
		{
			name:  "a pod on another node does not count",
			nodes: []corev1.Node{mokkaTestNode("a")},
			pods:  []corev1.Pod{mokkaTestPod("b", true)},
			want:  1,
			found: []string{"a: no Ready Mokka pod"},
		},
		{
			name:  "fewer labeled nodes than the pools ask for",
			nodes: []corev1.Node{mokkaTestNode("a")},
			pods:  []corev1.Pod{mokkaTestPod("a", true)},
			want:  2,
			found: []string{"1 nodes are labeled " + mokkaLabelKey + ", want 2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.found, mokkaPodProblems(tc.nodes, tc.pods, tc.want))
		})
	}
}

func TestValidatorProblems(t *testing.T) {
	passed := &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}
	for _, tc := range []struct {
		name  string
		pods  []corev1.Pod
		found []string
	}{
		{
			name: "every step exited 0",
			pods: []corev1.Pod{mokkaTestPod("a", true, validatorStep("driver-validation", passed), validatorStep("cuda-validation", passed))},
		},
		{
			name:  "no validator on the node",
			found: []string{"a: no validator pod"},
		},
		{
			name: "a validator being deleted does not count",
			pods: []corev1.Pod{
				deletingPod(mokkaTestPod("a", true, validatorStep("cuda-validation", passed))),
				mokkaTestPod("a", false, validatorStep("cuda-validation", nil)),
			},
			found: []string{"a: validator step cuda-validation has not finished"},
		},
		{
			name:  "steps not started",
			pods:  []corev1.Pod{mokkaTestPod("a", false)},
			found: []string{"a: the validator has not started its steps"},
		},
		{
			name:  "a step still running",
			pods:  []corev1.Pod{mokkaTestPod("a", false, validatorStep("driver-validation", passed), validatorStep("cuda-validation", nil))},
			found: []string{"a: validator step cuda-validation has not finished"},
		},
		{
			name: "a step that ended other than by exiting 0",
			pods: []corev1.Pod{mokkaTestPod("a", false, validatorStep("driver-validation",
				&corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown"}))},
			found: []string{"a: validator step driver-validation exited 137: ContainerStatusUnknown"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.found, validatorProblems([]corev1.Node{mokkaTestNode("a")}, tc.pods))
		})
	}
}

func TestMokkaGPUOperatorValues(t *testing.T) {
	t.Log("Testing: the embedded values turn off what Mokka replaces")
	var values struct {
		Driver struct {
			Enabled *bool `json:"enabled"`
		} `json:"driver"`
		Toolkit struct {
			Enabled *bool `json:"enabled"`
		} `json:"toolkit"`
	}
	require.NoError(t, yaml.Unmarshal(mokkaGPUOperatorValues, &values))
	require.Equal(t, new(bool), values.Driver.Enabled)
	require.Equal(t, new(bool), values.Toolkit.Enabled)
}
