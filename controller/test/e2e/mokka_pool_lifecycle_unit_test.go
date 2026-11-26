// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMokkaPoolProblems(t *testing.T) {
	ready := func(node corev1.Node) corev1.Node {
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		return node
	}
	primary := ready(mokkaGPUNode("gb", "gb300", mokkaProfiles["gb300"].product, 4))
	extra := ready(mokkaGPUNode("h", "h100", mokkaProfiles["h100"].product, 8))
	cp := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "control-plane"}}
	pools := map[string][]string{"gb300": {"gb"}, "h100": {"h"}}
	for _, tc := range []struct {
		name   string
		mutate func([]corev1.Node) []corev1.Node
		want   string
	}{
		{name: "correct pools"},
		{name: "missing node", mutate: func(n []corev1.Node) []corev1.Node { return n[:2] }, want: "h: missing tenant node"},
		{name: "wrong model", mutate: func(n []corev1.Node) []corev1.Node {
			n[2].Labels[gpuProductLabel] = mokkaProfiles["gb300"].product
			return n
		}, want: "want \"NVIDIA-H100-80GB-HBM3\" x8"},
		{name: "wrong count", mutate: func(n []corev1.Node) []corev1.Node {
			n[2] = ready(mokkaGPUNode("h", "h100", mokkaProfiles["h100"].product, 4))
			return n
		}, want: "x4, want"},
		{name: "self-consistent wrong profile", mutate: func(n []corev1.Node) []corev1.Node {
			n[2] = ready(mokkaGPUNode("h", "gb300", mokkaProfiles["gb300"].product, 4))
			return n
		}, want: "profile \"gb300\", want \"h100\""},
		{name: "not ready", mutate: func(n []corev1.Node) []corev1.Node { n[2].Status.Conditions = nil; return n }, want: "h:"},
		{name: "stale node", mutate: func(n []corev1.Node) []corev1.Node {
			return append(n, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "removed"}})
		}, want: "4 tenant nodes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := []corev1.Node{*cp.DeepCopy(), *primary.DeepCopy(), *extra.DeepCopy()}
			if tc.mutate != nil {
				nodes = tc.mutate(nodes)
			}
			problems := mokkaPoolProblems(nodes, pools)
			if tc.want == "" {
				require.Empty(t, problems)
			} else {
				require.Contains(t, joinProblems("pools", problems).Error(), tc.want)
			}
		})
	}
}

func TestMokkaContinuityProblem(t *testing.T) {
	before := corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "original"}, Spec: corev1.PodSpec{NodeName: "gb"}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: gpuContainer}}}}
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{name: "unchanged"},
		{name: "recreated", mutate: func(p *corev1.Pod) { p.UID = "replacement" }},
		{name: "moved", mutate: func(p *corev1.Pod) { p.Spec.NodeName = "h" }},
		{name: "restarted", mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 }},
		{name: "not ready", mutate: func(p *corev1.Pod) { p.Status.Conditions = nil }},
		{name: "deleting", mutate: func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := before.DeepCopy()
			if tc.mutate != nil {
				tc.mutate(after)
				require.Error(t, mokkaContinuityProblem(before, *after))
			} else {
				require.NoError(t, mokkaContinuityProblem(before, *after))
			}
		})
	}
}

func TestMokkaHeartbeats(t *testing.T) {
	latest, gap, err := mokkaHeartbeats("GPU 0: NVIDIA GB300 NVL\n100\n101\n106\n")
	require.NoError(t, err)
	require.EqualValues(t, 106, latest)
	require.EqualValues(t, 5, gap)
	for _, logs := range []string{"", "GPU 0: NVIDIA GB300 NVL", "bad", "102\n101"} {
		_, _, err := mokkaHeartbeats(logs)
		require.Error(t, err)
	}
}
