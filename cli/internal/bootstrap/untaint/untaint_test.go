// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package untaint

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var (
	otherTaint       = corev1.Taint{Key: "nke.nvidia.com/maintenance", Value: "kube-vip", Effect: corev1.TaintEffectNoSchedule}
	sameKeyNoExecute = corev1.Taint{Key: ControlPlaneTaintKey, Effect: corev1.TaintEffectNoExecute}
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func node(name string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Taints: taints},
	}
}

func taintsOf(t *testing.T, client *fake.Clientset, name string) []corev1.Taint {
	t.Helper()
	got, err := client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node %s: %v", name, err)
	}
	return got.Spec.Taints
}

func TestControlPlane(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nodes       []runtime.Object
		wantChanged []string
		wantTaints  map[string][]corev1.Taint
	}{
		{
			name: "removes the taint from every control-plane node",
			nodes: []runtime.Object{
				node("cp-01", controlPlaneTaint),
				node("cp-02", controlPlaneTaint),
				node("cp-03", controlPlaneTaint),
			},
			wantChanged: []string{"cp-01", "cp-02", "cp-03"},
			wantTaints:  map[string][]corev1.Taint{"cp-01": {}, "cp-02": {}, "cp-03": {}},
		},
		{
			name: "keeps taints that are not the control-plane one",
			nodes: []runtime.Object{
				node("cp-01", otherTaint, controlPlaneTaint),
			},
			wantChanged: []string{"cp-01"},
			wantTaints:  map[string][]corev1.Taint{"cp-01": {otherTaint}},
		},
		{
			name: "keeps the same key with another effect",
			nodes: []runtime.Object{
				node("cp-01", sameKeyNoExecute, controlPlaneTaint),
			},
			wantChanged: []string{"cp-01"},
			wantTaints:  map[string][]corev1.Taint{"cp-01": {sameKeyNoExecute}},
		},
		{
			name: "leaves an already schedulable node alone",
			nodes: []runtime.Object{
				node("cp-01"),
				node("cp-02", controlPlaneTaint),
			},
			wantChanged: []string{"cp-02"},
			wantTaints:  map[string][]corev1.Taint{"cp-01": nil, "cp-02": {}},
		},
		{
			name:        "does nothing on an empty cluster",
			wantChanged: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset(tc.nodes...)

			changed, err := ControlPlane(context.Background(), client, quietLogger())
			if err != nil {
				t.Fatalf("ControlPlane: %v", err)
			}

			if !reflect.DeepEqual(changed, tc.wantChanged) {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			for name, want := range tc.wantTaints {
				if got := taintsOf(t, client, name); !reflect.DeepEqual(got, want) {
					t.Errorf("%s taints = %v, want %v", name, got, want)
				}
			}
		})
	}
}

func TestControlPlaneIsIdempotent(t *testing.T) {
	client := fake.NewClientset(node("cp-01", controlPlaneTaint))

	first, err := ControlPlane(context.Background(), client, quietLogger())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !reflect.DeepEqual(first, []string{"cp-01"}) {
		t.Fatalf("first run changed %v, want [cp-01]", first)
	}

	second, err := ControlPlane(context.Background(), client, quietLogger())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second run changed %v, want nothing", second)
	}
}

func TestControlPlaneReportsAnUpdateFailure(t *testing.T) {
	client := fake.NewClientset(node("cp-01", controlPlaneTaint), node("cp-02", controlPlaneTaint))
	client.PrependReactor("update", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.UpdateAction).GetObject().(*corev1.Node).Name == "cp-02" {
			return true, nil, errors.New("admission denied")
		}
		return false, nil, nil
	})

	changed, err := ControlPlane(context.Background(), client, quietLogger())
	if err == nil {
		t.Fatal("ControlPlane returned nil, want the update failure")
	}
	for _, want := range []string{"cp-02", "admission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if !reflect.DeepEqual(changed, []string{"cp-01"}) {
		t.Errorf("changed = %v, want the nodes untainted before the failure [cp-01]", changed)
	}
}

func TestNewClientRejectsAnUnreadableKubeconfig(t *testing.T) {
	_, err := NewClient("/nonexistent/kubeconfig")
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/kubeconfig") {
		t.Errorf("err = %v, want one naming the kubeconfig path", err)
	}
}
