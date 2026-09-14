// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestKubernetesSourceOptionsMatches(t *testing.T) {
	managedSelector := labels.SelectorFromSet(labels.Set{"managed": "true"})
	tests := map[string]struct {
		opts   KubernetesSourceOptions
		object *corev1.Secret
		want   []string
	}{
		"nil label selector matches": {
			object: secretForFilterTest("any", nil),
			want:   []string{"first", "second"},
		},
		"namespace matches": {
			opts:   KubernetesSourceOptions{Namespace: "target"},
			object: secretForFilterTest("target", nil),
			want:   []string{"first", "second"},
		},
		"namespace does not match": {
			opts:   KubernetesSourceOptions{Namespace: "target"},
			object: secretForFilterTest("other", nil),
		},
		"labels match": {
			opts:   KubernetesSourceOptions{LabelSelector: managedSelector},
			object: secretForFilterTest("any", map[string]string{"managed": "true"}),
			want:   []string{"first", "second"},
		},
		"labels do not match": {
			opts:   KubernetesSourceOptions{LabelSelector: managedSelector},
			object: secretForFilterTest("any", map[string]string{"managed": "false"}),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newKubernetesEventHandler(
				func(context.Context, *corev1.Secret) []string {
					return []string{"first", "second"}
				},
				tt.opts,
			)
			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
			t.Cleanup(queue.ShutDown)

			h.Create(t.Context(), event.TypedCreateEvent[*corev1.Secret]{Object: tt.object}, queue)

			got := make([]string, 0, queue.Len())
			for range queue.Len() {
				request, shutdown := queue.Get()
				require.False(t, shutdown)
				queue.Done(request)
				queue.Forget(request)
				got = append(got, request)
			}
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func secretForFilterTest(namespace string, objectLabels map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Labels:    objectLabels,
	}}
}
