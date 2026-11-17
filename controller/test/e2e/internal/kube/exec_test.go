// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDefaultContainer(t *testing.T) {
	containers := []corev1.Container{{Name: "main"}, {Name: "sidecar"}}
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        string
	}{
		{name: "the annotated container", annotations: map[string]string{defaultContainerAnnotation: "node-agent"}, want: "node-agent"},
		{name: "the first container without the annotation", want: "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}, Spec: corev1.PodSpec{Containers: containers}}
			if got := DefaultContainer(pod); got != tc.want {
				t.Errorf("DefaultContainer() = %q, want %q", got, tc.want)
			}
		})
	}
}
