// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	temporalactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestDrainKubeNodeCordonsEvictsAndLabelsUsingTenantClient(t *testing.T) {
	fixture := newDrainKubeNodeFixture(t, true,
		drainTestPod("workload", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "workload")),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       "kube-system",
				Name:            "daemon",
				OwnerReferences: controlledBy("apps/v1", "DaemonSet", "daemon"),
			},
			Spec: corev1.PodSpec{NodeName: "worker-0"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   "kube-system",
				Name:        "mirror",
				Annotations: map[string]string{mirrorPodAnnotation: "hash"},
			},
			Spec: corev1.PodSpec{NodeName: "worker-0"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "other-node"},
			Spec:       corev1.PodSpec{NodeName: "worker-1"},
		},
	)

	require.NoError(t, executeDrainKubeNodeActivity(t, fixture.activities, fixture.input))

	var node corev1.Node
	require.NoError(t, fixture.tenantClient.Get(context.Background(), client.ObjectKey{Name: fixture.node.Name}, &node))
	require.True(t, node.Spec.Unschedulable)
	require.Equal(t, "true", node.Labels[drainedNodeLabel])

	assertPodNotFound(t, fixture.tenantClient, "default", "workload")
	assertPodExists(t, fixture.tenantClient, "kube-system", "daemon")
	assertPodExists(t, fixture.tenantClient, "kube-system", "mirror")
	assertPodExists(t, fixture.tenantClient, "default", "other-node")
	require.Equal(t, 1, fixture.provider.calls)
	require.Equal(t, fixture.cluster, fixture.provider.cluster)
	fixture.database.AssertExpectations(t)
}

func TestDrainKubeNodeEvictsEveryPodThatCanBeEvicted(t *testing.T) {
	mirror := drainTestPod("mirror", corev1.PodRunning, nil)
	mirror.Annotations = map[string]string{mirrorPodAnnotation: "hash"}

	for _, test := range []struct {
		name        string
		pod         *corev1.Pod
		wantEvicted bool
	}{
		{
			name:        "pod with a controller is evicted",
			pod:         drainTestPod("workload", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "workload")),
			wantEvicted: true,
		},
		{
			name:        "pod with no controller is evicted",
			pod:         drainTestPod("bare", corev1.PodRunning, nil),
			wantEvicted: true,
		},
		{
			name:        "pending pod is evicted",
			pod:         drainTestPod("starting", corev1.PodPending, controlledBy("apps/v1", "ReplicaSet", "starting")),
			wantEvicted: true,
		},
		{
			name:        "pod of a DaemonSet kind from another API group is evicted",
			pod:         drainTestPod("lookalike", corev1.PodRunning, controlledBy("apps.kruise.io/v1alpha1", "DaemonSet", "agent")),
			wantEvicted: true,
		},
		{
			name: "pod of a DaemonSet is left",
			pod:  drainTestPod("agent", corev1.PodRunning, controlledBy("apps/v1", "DaemonSet", "agent")),
		},
		{
			name: "mirror pod is left",
			pod:  mirror,
		},
		{
			name: "succeeded pod of a Job is left",
			pod:  drainTestPod("job", corev1.PodSucceeded, controlledBy("batch/v1", "Job", "job")),
		},
		{
			name: "failed pod with no controller is left",
			pod:  drainTestPod("failed", corev1.PodFailed, nil),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDrainKubeNodeFixture(t, true, test.pod.DeepCopy())

			require.NoError(t, executeDrainKubeNodeActivity(t, fixture.activities, fixture.input))

			if test.wantEvicted {
				assertPodNotFound(t, fixture.tenantClient, test.pod.Namespace, test.pod.Name)
			} else {
				assertPodExists(t, fixture.tenantClient, test.pod.Namespace, test.pod.Name)
			}
		})
	}
}

func TestDrainKubeNodeForceDeletesAllRemainingPodsAfterGracefulFailure(t *testing.T) {
	fixture := newDrainKubeNodeFixture(t, true,
		drainTestPod("protected", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "protected")),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       "kube-system",
				Name:            "daemon",
				OwnerReferences: controlledBy("apps/v1", "DaemonSet", "daemon"),
			},
			Spec: corev1.PodSpec{NodeName: "worker-0"},
		},
	)
	refusePodEvictions(&fixture)
	fixture.input.ForceDrainOnFailure = true

	require.NoError(t, executeDrainKubeNodeActivity(t, fixture.activities, fixture.input))

	assertPodNotFound(t, fixture.tenantClient, "default", "protected")
	assertPodNotFound(t, fixture.tenantClient, "kube-system", "daemon")
	var node corev1.Node
	require.NoError(t, fixture.tenantClient.Get(context.Background(), client.ObjectKey{Name: fixture.node.Name}, &node))
	require.True(t, node.Spec.Unschedulable)
	require.Equal(t, "true", node.Labels[drainedNodeLabel])
}

func TestDrainKubeNodeDoesNotForceDeleteControlPlanePods(t *testing.T) {
	fixture := newDrainKubeNodeFixture(t, true,
		drainTestPod("protected", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "protected")),
	)
	refusePodEvictions(&fixture)

	err := executeDrainKubeNodeActivity(t, fixture.activities, fixture.input)
	require.ErrorContains(t, err, "failed to drain Kubernetes node worker-0: timed out after 1s waiting for pods to leave: "+
		"default/protected (eviction refused: The disruption budget protected needs 2 healthy pods and has 1 currently)")

	assertPodExists(t, fixture.tenantClient, "default", "protected")
	var node corev1.Node
	require.NoError(t, fixture.tenantClient.Get(context.Background(), client.ObjectKey{Name: fixture.node.Name}, &node))
	require.True(t, node.Spec.Unschedulable)
	require.NotContains(t, node.Labels, drainedNodeLabel)
}

func TestDrainKubeNodeClassifiesEvictionErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		refusal error
		wantErr string
	}{
		{
			name:    "PodDisruptionBudget refuses with 429",
			refusal: pdbRefusal("workload"),
		},
		{
			name:    "PodDisruptionBudget refuses with 403",
			refusal: forbiddenRefusal("workload", policyv1.DisruptionBudgetCause),
		},
		{
			name:    "namespace is in deletion",
			refusal: forbiddenRefusal("workload", corev1.NamespaceTerminatingCause),
		},
		{
			name:    "pod is not found",
			refusal: apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "workload"),
		},
		{
			name:    "403 for a different reason fails the drain",
			refusal: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "workload", errors.New("RBAC: access denied")),
			wantErr: "failed to evict pods: pod default/workload: ",
		},
		{
			name:    "500 fails the drain",
			refusal: apierrors.NewInternalError(errors.New("this pod has more than one PodDisruptionBudget")),
			wantErr: "failed to evict pods: pod default/workload: ",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDrainKubeNodeFixture(t, true,
				drainTestPod("workload", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "workload")),
			)

			var evictions []*policyv1.Eviction
			fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, interceptor.Funcs{
				SubResourceCreate: func(
					ctx context.Context,
					underlying client.Client,
					subResourceName string,
					obj client.Object,
					subResource client.Object,
					opts ...client.SubResourceCreateOption,
				) error {
					if eviction, ok := subResource.(*policyv1.Eviction); ok {
						evictions = append(evictions, eviction.DeepCopy())
					}
					if len(evictions) == 1 {
						return test.refusal
					}
					return underlying.SubResource(subResourceName).Create(ctx, obj, subResource, opts...)
				},
			})
			fixture.provider.tenantClient = fixture.tenantClient

			err := executeDrainKubeNodeActivity(t, fixture.activities, fixture.input)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				require.Len(t, evictions, 1)
				assertPodExists(t, fixture.tenantClient, "default", "workload")
				return
			}

			require.NoError(t, err)
			require.Len(t, evictions, 2)
			for _, eviction := range evictions {
				require.Nil(t, eviction.DeleteOptions, "an eviction must use the grace period of the pod")
			}
			assertPodNotFound(t, fixture.tenantClient, "default", "workload")
		})
	}
}

func TestDrainKubeNodeEvictsTheOtherPodsWhenAnEvictionFails(t *testing.T) {
	for _, test := range []struct {
		name                string
		forceDrainOnFailure bool
	}{
		{name: "control-plane drain fails and gives the failed eviction"},
		{name: "worker drain force-deletes after the other evictions", forceDrainOnFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDrainKubeNodeFixture(t, true,
				drainTestPod("first", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "first")),
				drainTestPod("second", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "second")),
			)
			fixture.input.ForceDrainOnFailure = test.forceDrainOnFailure

			var evicted, requests []string
			failedPod := ""
			fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, interceptor.Funcs{
				List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					requests = append(requests, "list")
					return underlying.List(ctx, list, opts...)
				},
				SubResourceCreate: func(
					ctx context.Context,
					underlying client.Client,
					subResourceName string,
					obj client.Object,
					subResource client.Object,
					opts ...client.SubResourceCreateOption,
				) error {
					evicted = append(evicted, obj.GetName())
					requests = append(requests, "eviction")
					if failedPod == "" || failedPod == obj.GetName() {
						failedPod = obj.GetName()
						return apierrors.NewInternalError(errors.New("this pod has more than one PodDisruptionBudget"))
					}
					return underlying.SubResource(subResourceName).Create(ctx, obj, subResource, opts...)
				},
			})
			fixture.provider.tenantClient = fixture.tenantClient

			err := executeDrainKubeNodeActivity(t, fixture.activities, fixture.input)

			require.Len(t, evicted, 2, "each pod gets one eviction, also after the first one fails")
			require.Equal(t, []string{"list", "eviction", "eviction"}, requests[:3], "both evictions are in the first round")
			otherPod := "first"
			if failedPod == "first" {
				otherPod = "second"
			}
			assertPodNotFound(t, fixture.tenantClient, "default", otherPod)
			if test.forceDrainOnFailure {
				require.NoError(t, err)
				assertPodNotFound(t, fixture.tenantClient, "default", failedPod)
				return
			}
			require.ErrorContains(t, err, "failed to evict pods: pod default/"+failedPod+": Internal error occurred")
			assertPodExists(t, fixture.tenantClient, "default", failedPod)
		})
	}
}

func TestDrainKubeNodeWaitsForTheOtherPodsBeforeItFails(t *testing.T) {
	stopping := drainTestPod("stopping", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "stopping"))
	stopping.Finalizers = []string{"example.com/cleanup"}
	fixture := newDrainKubeNodeFixture(t, true,
		drainTestPod("blocked", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "blocked")),
		stopping,
	)
	blockedEvictions := 0
	fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, interceptor.Funcs{
		SubResourceCreate: func(
			ctx context.Context,
			underlying client.Client,
			subResourceName string,
			obj client.Object,
			subResource client.Object,
			opts ...client.SubResourceCreateOption,
		) error {
			if obj.GetName() == "blocked" {
				blockedEvictions++
				return apierrors.NewInternalError(errors.New("this pod has more than one PodDisruptionBudget"))
			}
			return underlying.SubResource(subResourceName).Create(ctx, obj, subResource, opts...)
		},
	})
	fixture.provider.tenantClient = fixture.tenantClient

	err := executeDrainKubeNodeActivity(t, fixture.activities, fixture.input)

	require.ErrorContains(t, err, "timed out after 1s waiting for pods to leave: ")
	require.ErrorContains(t, err, "default/blocked (eviction failed: Internal error occurred")
	require.ErrorContains(t, err, "default/stopping (deletion timestamp ")
	require.Equal(t, 1, blockedEvictions, "a failed eviction must not be tried again in the attempt")
}

func TestDrainKubeNodeWaitsForAPodThatIsDeletedAfterItsEvictionFails(t *testing.T) {
	deleted := drainTestPod("deleted", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "deleted"))
	deleted.Finalizers = []string{"example.com/cleanup"}
	fixture := newDrainKubeNodeFixture(t, true, deleted)
	fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, interceptor.Funcs{
		SubResourceCreate: func(ctx context.Context, underlying client.Client, _ string, obj, _ client.Object, _ ...client.SubResourceCreateOption) error {
			// The API server deletes the pod, but the response is lost.
			var pod corev1.Pod
			if err := underlying.Get(ctx, client.ObjectKeyFromObject(obj), &pod); err != nil {
				return err
			}
			if err := underlying.Delete(ctx, &pod); err != nil {
				return err
			}
			return errors.New("unexpected EOF")
		},
	})
	fixture.provider.tenantClient = fixture.tenantClient

	err := executeDrainKubeNodeActivity(t, fixture.activities, fixture.input)

	require.ErrorContains(t, err, "timed out after 1s waiting for pods to leave: default/deleted (deletion timestamp ")
}

func TestFailedEvictions(t *testing.T) {
	failure := apierrors.NewInternalError(errors.New("this pod has more than one PodDisruptionBudget"))
	pods := make([]corev1.Pod, 0, drainErrorMaxPods+2)
	allFailed := make(map[client.ObjectKey]error, drainErrorMaxPods+2)
	for i := range drainErrorMaxPods + 2 {
		pod := drainTestPod(fmt.Sprintf("pod-%02d", i), corev1.PodRunning, nil)
		pods = append(pods, *pod)
		allFailed[client.ObjectKeyFromObject(pod)] = failure
	}
	terminating := pods[0]
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	firstPod := client.ObjectKeyFromObject(&pods[0])

	for _, test := range []struct {
		name           string
		pods           []corev1.Pod
		evictionErrors map[client.ObjectKey]error
		wantErr        []string
		notWantErr     string
	}{
		{
			name:           "every pod has a failed eviction",
			pods:           pods,
			evictionErrors: allFailed,
			wantErr:        []string{"pod default/pod-09: Internal error occurred", ", and 2 more]"},
			notWantErr:     "pod-10",
		},
		{
			name:           "a pod has a refused eviction",
			pods:           pods[:1],
			evictionErrors: map[client.ObjectKey]error{firstPod: pdbRefusal("pod-00")},
		},
		{
			name: "a pod has a failed eviction and another pod has a refused eviction",
			pods: pods[:2],
			evictionErrors: map[client.ObjectKey]error{
				firstPod:                             failure,
				client.ObjectKeyFromObject(&pods[1]): pdbRefusal("pod-01"),
			},
		},
		{
			name:           "a pod has no eviction error",
			pods:           pods[:1],
			evictionErrors: map[client.ObjectKey]error{},
		},
		{
			name:           "a pod with a failed eviction has a deletion timestamp",
			pods:           []corev1.Pod{terminating},
			evictionErrors: map[client.ObjectKey]error{firstPod: failure},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := failedEvictions(test.pods, test.evictionErrors)
			if len(test.wantErr) == 0 {
				require.NoError(t, err)
				return
			}
			for _, want := range test.wantErr {
				require.ErrorContains(t, err, want)
			}
			if test.notWantErr != "" {
				require.NotContains(t, err.Error(), test.notWantErr)
			}
		})
	}
}

func TestDrainKubeNodeDoesNotWaitForPodsRecreatedOnTheNode(t *testing.T) {
	original := drainTestPod("workload", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "workload"))
	original.UID = "original"
	fixture := newDrainKubeNodeFixture(t, true, original)
	fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, interceptor.Funcs{
		SubResourceCreate: func(
			ctx context.Context,
			underlying client.Client,
			subResourceName string,
			obj client.Object,
			subResource client.Object,
			opts ...client.SubResourceCreateOption,
		) error {
			if err := underlying.SubResource(subResourceName).Create(ctx, obj, subResource, opts...); err != nil {
				return err
			}
			recreated := drainTestPod(obj.GetName(), corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "workload"))
			recreated.UID = "recreated"
			return underlying.Create(ctx, recreated)
		},
	})
	fixture.provider.tenantClient = fixture.tenantClient

	require.NoError(t, executeDrainKubeNodeActivity(t, fixture.activities, fixture.input))

	var pod corev1.Pod
	require.NoError(t, fixture.tenantClient.Get(context.Background(), client.ObjectKeyFromObject(original), &pod))
	require.Equal(t, types.UID("recreated"), pod.UID)
}

func TestDrainKubeNodeWaitsForTerminatingPods(t *testing.T) {
	for _, test := range []struct {
		name                  string
		pastDeletionTimestamp time.Duration
		wantErr               string
	}{
		{
			name:                  "pod in its grace period holds the drain",
			pastDeletionTimestamp: -time.Minute,
			wantErr:               "default/stuck (deletion timestamp ",
		},
		{
			name:                  "pod 4m50s past its deletion timestamp holds the drain",
			pastDeletionTimestamp: 4*time.Minute + 50*time.Second,
			wantErr:               "default/stuck (deletion timestamp ",
		},
		{
			name:                  "pod 5m10s past its deletion timestamp does not hold the drain",
			pastDeletionTimestamp: 5*time.Minute + 10*time.Second,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stuck := drainTestPod("stuck", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "stuck"))
			stuck.Finalizers = []string{"example.com/cleanup"}
			stuck.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(-test.pastDeletionTimestamp).Truncate(time.Second)}
			fixture := newDrainKubeNodeFixture(t, true, stuck)

			err := executeDrainKubeNodeActivity(t, fixture.activities, fixture.input)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
			assertPodExists(t, fixture.tenantClient, "default", "stuck")
		})
	}
}

func TestDrainKubeNodeReportsWhyTheDrainStopped(t *testing.T) {
	for _, test := range []struct {
		name    string
		funcs   func(t *testing.T, cancelParent func()) interceptor.Funcs
		wantErr string
	}{
		{
			name: "list that ends after the deadline",
			funcs: func(*testing.T, func()) interceptor.Funcs {
				lists := 0
				return interceptor.Funcs{
					SubResourceCreate: refuseEviction,
					List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						lists++
						if lists == 2 {
							if err := waitLongerThanTheDrain(ctx); err != nil {
								return err
							}
						}
						return underlying.List(ctx, list, opts...)
					},
				}
			},
			wantErr: "failed to drain Kubernetes node worker-0: timed out after 1s waiting for pods to leave: " +
				"default/protected (eviction refused: The disruption budget protected needs 2 healthy pods and has 1 currently)",
		},
		{
			name: "eviction that ends after the deadline",
			funcs: func(*testing.T, func()) interceptor.Funcs {
				evictions := 0
				return interceptor.Funcs{
					SubResourceCreate: func(
						ctx context.Context,
						underlying client.Client,
						subResourceName string,
						obj client.Object,
						subResource client.Object,
						opts ...client.SubResourceCreateOption,
					) error {
						evictions++
						if evictions == 2 {
							if err := waitLongerThanTheDrain(ctx); err != nil {
								return err
							}
						}
						return refuseEviction(ctx, underlying, subResourceName, obj, subResource, opts...)
					},
				}
			},
			wantErr: "failed to drain Kubernetes node worker-0: timed out after 1s waiting for pods to leave: " +
				"default/protected (eviction refused: The disruption budget protected needs 2 healthy pods and has 1 currently)",
		},
		{
			name: "rate limiter that refuses a request near its deadline",
			funcs: func(*testing.T, func()) interceptor.Funcs {
				return interceptor.Funcs{
					SubResourceCreate: func(
						ctx context.Context,
						underlying client.Client,
						subResourceName string,
						obj client.Object,
						subResource client.Object,
						opts ...client.SubResourceCreateOption,
					) error {
						// client-go gives this error before the deadline. It is not a context error.
						if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 2*time.Second {
							return errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline")
						}
						return refuseEviction(ctx, underlying, subResourceName, obj, subResource, opts...)
					},
				}
			},
			wantErr: "failed to drain Kubernetes node worker-0: timed out after 1s waiting for pods to leave: " +
				"default/protected (eviction refused: The disruption budget protected needs 2 healthy pods and has 1 currently)",
		},
		{
			name: "parent context is canceled in an eviction",
			funcs: func(_ *testing.T, cancelParent func()) interceptor.Funcs {
				return interceptor.Funcs{
					SubResourceCreate: func(ctx context.Context, _ client.Client, _ string, _, _ client.Object, _ ...client.SubResourceCreateOption) error {
						cancelParent()
						return ctx.Err()
					},
				}
			},
			wantErr: "failed to drain Kubernetes node worker-0: drain stopped: context canceled",
		},
		{
			name:    "parent context is canceled in the first list",
			funcs:   cancelParentInList(1, true),
			wantErr: "failed to drain Kubernetes node worker-0: drain stopped: context canceled",
		},
		{
			name:    "parent context is canceled in a later list",
			funcs:   cancelParentInList(2, true),
			wantErr: "failed to drain Kubernetes node worker-0: drain stopped: context canceled",
		},
		{
			name:    "parent context is canceled after a list",
			funcs:   cancelParentInList(2, false),
			wantErr: "failed to drain Kubernetes node worker-0: drain stopped: context canceled",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDrainKubeNodeFixture(t, true,
				drainTestPod("protected", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "protected")),
			)
			var cancelParent context.CancelFunc
			fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, test.funcs(t, func() { cancelParent() }))
			fixture.provider.tenantClient = fixture.tenantClient

			env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
			env.RegisterActivityWithOptions(func(ctx context.Context, input DrainKubeNodeInput) error {
				ctx, cancelParent = context.WithCancel(ctx)
				defer cancelParent()
				return fixture.activities.drainKubeNode(ctx, input, time.Millisecond, time.Second)
			}, temporalactivity.RegisterOptions{Name: "TestDrainKubeNodeStops"})

			_, err := env.ExecuteActivity("TestDrainKubeNodeStops", fixture.input)

			require.ErrorContains(t, err, test.wantErr)
			assertPodExists(t, fixture.tenantClient, "default", "protected")
		})
	}
}

func TestDrainKubeNodeStopsAtTheDeadline(t *testing.T) {
	const timeout = time.Second
	for _, test := range []struct {
		name          string
		pods          []string
		slowFirstList bool
		slowEviction  bool
		leaveAtList   int
		wantErr       string
		wantEvictions int
	}{
		{
			name:          "deadline in the wait",
			pods:          []string{"a"},
			wantErr:       "timed out after 1s waiting for pods to leave: default/a (eviction refused: ",
			wantEvictions: 1,
		},
		{
			name:          "pod that leaves in the last wait",
			pods:          []string{"a"},
			leaveAtList:   2,
			wantEvictions: 1,
		},
		{
			name:         "deadline in an eviction",
			pods:         []string{"a", "b"},
			slowEviction: true,
			wantErr: "timed out after 1s waiting for pods to leave: " +
				"default/a (eviction refused: The disruption budget a needs 2 healthy pods and has 1 currently), default/b: ",
			wantEvictions: 1,
		},
		{
			name:          "first list that takes longer than the timeout",
			pods:          []string{"a"},
			slowFirstList: true,
			wantErr:       "timed out after 1s waiting for pods to leave: default/a (eviction refused: ",
			wantEvictions: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := make([]client.Object, 0, len(test.pods))
			for _, name := range test.pods {
				objects = append(objects, drainTestPod(name, corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", name)))
			}
			fixture := newDrainKubeNodeFixture(t, true, objects...)
			evictions, lists := 0, 0
			fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, interceptor.Funcs{
				List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists++
					if lists == 1 && test.slowFirstList {
						time.Sleep(timeout + 10*time.Millisecond)
					}
					if lists == test.leaveAtList {
						for _, name := range test.pods {
							require.NoError(t, underlying.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}))
						}
					}
					return underlying.List(ctx, list, opts...)
				},
				SubResourceCreate: func(
					ctx context.Context,
					underlying client.Client,
					subResourceName string,
					obj client.Object,
					subResource client.Object,
					opts ...client.SubResourceCreateOption,
				) error {
					evictions++
					if test.slowEviction {
						// The timeout starts before the first eviction, so it ends during this eviction.
						time.Sleep(timeout + 10*time.Millisecond)
					}
					return refuseEviction(ctx, underlying, subResourceName, obj, subResource, opts...)
				},
			})
			fixture.provider.tenantClient = fixture.tenantClient

			var drainErr error
			env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
			env.RegisterActivityWithOptions(func(ctx context.Context, input DrainKubeNodeInput) error {
				// No tick comes before the deadline.
				drainErr = fixture.activities.drainKubeNode(ctx, input, time.Hour, timeout)
				return drainErr
			}, temporalactivity.RegisterOptions{Name: "TestDrainKubeNodeWithoutTicks"})

			_, err := env.ExecuteActivity("TestDrainKubeNodeWithoutTicks", fixture.input)

			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				// The attempt logger reports outcome=timed_out for this error.
				require.ErrorIs(t, drainErr, context.DeadlineExceeded)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, test.wantEvictions, evictions)
			require.Equal(t, 2, lists, "one list must follow the deadline")
		})
	}
}

func TestDrainKubeNodeDoesNotRetryWhenARowIsMissing(t *testing.T) {
	for _, test := range []struct {
		name       string
		nodeErr    error
		clusterErr error
	}{
		{name: "node row is missing", nodeErr: db.ErrNotFound},
		{name: "cluster row is missing", clusterErr: db.ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodeID := uuid.New()
			clusterID := uuid.New()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, nodeID).Return(db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}, test.nodeErr)
			database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, test.clusterErr).Maybe()
			tx := &testutil.MockTx{}
			tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, db.ErrNotFound)
			database.GetTx = func() db.Tx { return tx }
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			a := &Activities{db: database}

			env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
			env.RegisterActivity(a.DrainKubeNode)
			_, err := env.ExecuteActivity(a.DrainKubeNode, DrainKubeNodeInput{NodeID: nodeID})

			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			require.True(t, appErr.NonRetryable(), "a missing row must end the retries of an unlimited drain")
		})
	}
}

func TestDescribeDrainingPods(t *testing.T) {
	pod := drainTestPod("web", corev1.PodRunning, nil)
	terminatingPod := drainTestPod("web", corev1.PodRunning, nil)
	terminatingPod.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
	refused := map[client.ObjectKey]error{client.ObjectKeyFromObject(pod): pdbRefusal("web")}
	failed := map[client.ObjectKey]error{
		client.ObjectKeyFromObject(pod): apierrors.NewInternalError(errors.New("this pod has more than one PodDisruptionBudget")),
	}
	manyPods := make([]corev1.Pod, 0, drainErrorMaxPods+2)
	for i := range drainErrorMaxPods + 2 {
		manyPods = append(manyPods, *drainTestPod(fmt.Sprintf("pod-%d", i), corev1.PodRunning, nil))
	}

	for _, test := range []struct {
		name           string
		pods           []corev1.Pod
		evictionErrors map[client.ObjectKey]error
		want           string
	}{
		{
			name:           "pod with a refused eviction",
			pods:           []corev1.Pod{*pod},
			evictionErrors: refused,
			want:           "default/web (eviction refused: The disruption budget web needs 2 healthy pods and has 1 currently)",
		},
		{
			name:           "pod with a failed eviction",
			pods:           []corev1.Pod{*pod},
			evictionErrors: failed,
			want:           "default/web (eviction failed: Internal error occurred: this pod has more than one PodDisruptionBudget)",
		},
		{
			name: "pod with an eviction refused for a reason other than a PodDisruptionBudget",
			pods: []corev1.Pod{*pod},
			evictionErrors: map[client.ObjectKey]error{
				client.ObjectKeyFromObject(pod): forbiddenRefusal("web", corev1.NamespaceTerminatingCause),
			},
			want: `default/web (eviction refused: pods "web" is forbidden: eviction refused)`,
		},
		{
			name:           "deletion timestamp replaces an earlier refusal",
			pods:           []corev1.Pod{*terminatingPod},
			evictionErrors: refused,
			want:           "default/web (deletion timestamp 2026-11-02T10:00:00Z)",
		},
		{
			name: "more pods than the maximum",
			pods: manyPods,
			want: strings.Join([]string{
				"default/pod-0", "default/pod-1", "default/pod-2", "default/pod-3", "default/pod-4",
				"default/pod-5", "default/pod-6", "default/pod-7", "default/pod-8", "default/pod-9", "and 2 more",
			}, ", "),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, describeDrainingPods(test.pods, test.evictionErrors))
		})
	}
}

func TestDrainKubeNodeShowsItsProgressInTheDeletingCondition(t *testing.T) {
	for _, test := range []struct {
		name         string
		controlPlane bool
		pods         []client.Object
		wantLastErr  string
	}{
		{
			name: "worker drain that completes",
		},
		{
			name:         "control-plane drain that fails",
			controlPlane: true,
			pods:         []client.Object{drainTestPod("blocked", corev1.PodRunning, controlledBy("apps/v1", "ReplicaSet", "blocked"))},
			wantLastErr:  " -- last error: failed to drain Kubernetes node worker-0: failed to evict pods: pod default/blocked: Internal error occurred",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDrainKubeNodeFixture(t, true, test.pods...)
			fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, interceptor.Funcs{
				SubResourceCreate: func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error {
					return apierrors.NewInternalError(errors.New("this pod has more than one PodDisruptionBudget"))
				},
			})
			fixture.provider.tenantClient = fixture.tenantClient

			txNode := fixture.node
			txNode.ControlPlane = test.controlPlane
			var updates []db.Node
			tx := &testutil.MockTx{}
			tx.On("GetNode", mock.Anything, fixture.node.ID).Return(txNode, nil)
			tx.On("GetClusterForUpdate", mock.Anything, fixture.cluster.ID).Return(fixture.cluster, nil).Maybe()
			tx.On("GetNodeForUpdate", mock.Anything, fixture.node.ID).Return(txNode, nil)
			tx.On("UpdateNode", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
				updates = append(updates, *args.Get(1).(*db.Node))
			}).Return(db.Node{}, nil)
			fixture.database.GetTx = func() db.Tx { return tx }
			fixture.database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

			env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
			env.RegisterActivity(fixture.activities.DrainKubeNode)

			_, err := env.ExecuteActivity(fixture.activities.DrainKubeNode, fixture.input)

			require.Equal(t, test.wantLastErr != "", err != nil, "error: %v", err)
			require.NotEmpty(t, updates)
			require.Equal(t, db.ConditionStatusTrue, updates[0].Conditions.Deleting.Status)
			require.Equal(t, "Draining Kubernetes node", updates[0].Conditions.Deleting.Message)
			if test.wantLastErr == "" {
				require.Len(t, updates, 1)
				return
			}
			require.Len(t, updates, 2)
			require.True(t, strings.HasPrefix(updates[1].Conditions.Deleting.Message, "Draining Kubernetes node (1 attempt has failed so far, "))
			require.Contains(t, updates[1].Conditions.Deleting.Message, test.wantLastErr)
		})
	}
}

func TestDrainKubeNodeDoesNotStopWhenTheConditionUpdateFails(t *testing.T) {
	fixture := newDrainKubeNodeFixture(t, true)
	tx := &testutil.MockTx{}
	tx.On("GetNode", mock.Anything, fixture.node.ID).Return(db.Node{}, errors.New("database is unavailable"))
	fixture.database.GetTx = func() db.Tx { return tx }
	fixture.database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(fixture.activities.DrainKubeNode)

	_, err := env.ExecuteActivity(fixture.activities.DrainKubeNode, fixture.input)

	require.NoError(t, err)
	tx.AssertCalled(t, "GetNode", mock.Anything, fixture.node.ID)
	var node corev1.Node
	require.NoError(t, fixture.tenantClient.Get(context.Background(), client.ObjectKey{Name: fixture.node.Name}, &node))
	require.Equal(t, "true", node.Labels[drainedNodeLabel])
}

func TestDrainKubeNodeSucceedsWhenTenantNodeIsAlreadyAbsent(t *testing.T) {
	fixture := newDrainKubeNodeFixture(t, false)

	require.NoError(t, executeDrainKubeNodeActivity(t, fixture.activities, fixture.input))
	require.Equal(t, 1, fixture.provider.calls)
	fixture.database.AssertExpectations(t)
}

func TestDrainKubeNodeSkipsWhenClusterIsDeleting(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(db.Node{
		ID: nodeID, ClusterID: clusterID, Name: "worker-0",
	}, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID:   clusterID,
		Name: "cluster-a",
		Conditions: db.ClusterConditions{
			Deleting: db.NewCondition(true, "Cluster deletion in progress"),
		},
	}, nil)

	// No tenant client provider: reaching for one would panic, proving the
	// activity returns before it needs the tenant API.
	a := &Activities{db: database}

	require.NoError(t, executeDrainKubeNodeActivity(t, a, DrainKubeNodeInput{NodeID: nodeID}))
	database.AssertExpectations(t)
}

type drainKubeNodeFixture struct {
	activities   *Activities
	tenantClient client.WithWatch
	provider     *fakeTenantClientProvider
	database     *testutil.MockDB
	node         db.Node
	cluster      db.Cluster
	input        DrainKubeNodeInput
}

// newDrainKubeNodeFixture returns a fixture that drains the node worker-0. The
// fake tenant API has the objects, and also the Node if includeKubeNode is true.
func newDrainKubeNodeFixture(t *testing.T, includeKubeNode bool, objects ...client.Object) drainKubeNodeFixture {
	t.Helper()

	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, policyv1.AddToScheme(scheme))

	tenantObjects := make([]client.Object, 0, len(objects)+1)
	if includeKubeNode {
		tenantObjects = append(tenantObjects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}})
	}
	tenantObjects = append(tenantObjects, objects...)

	tenantClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tenantObjects...).
		WithIndex(&corev1.Pod{}, podNodeNameField, func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).
		Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}

	return drainKubeNodeFixture{
		activities: &Activities{
			db:                   database,
			tenantClientProvider: provider,
		},
		tenantClient: tenantClient,
		provider:     provider,
		database:     database,
		node:         node,
		cluster:      cluster,
		input:        DrainKubeNodeInput{NodeID: nodeID},
	}
}

// drainTestPod returns a pod on the node worker-0 in the namespace default.
func drainTestPod(name string, phase corev1.PodPhase, ownerReferences []metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, OwnerReferences: ownerReferences},
		Spec:       corev1.PodSpec{NodeName: "worker-0"},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// controlledBy returns owner references that make the named object the
// controller of a pod.
func controlledBy(apiVersion, kind, name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, Controller: ptr.To(true)}}
}

// refusePodEvictions makes the tenant API refuse all evictions, as for a
// PodDisruptionBudget that allows no disruption.
func refusePodEvictions(fixture *drainKubeNodeFixture) {
	fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, interceptor.Funcs{SubResourceCreate: refuseEviction})
	fixture.provider.tenantClient = fixture.tenantClient
}

// waitLongerThanTheDrain waits longer than the 1 s drain of these tests, or until
// ctx ends.
func waitLongerThanTheDrain(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(1200 * time.Millisecond):
		return nil
	}
}

// refuseEviction refuses an eviction, as for a PodDisruptionBudget that allows
// no disruption.
func refuseEviction(_ context.Context, _ client.Client, _ string, obj, _ client.Object, _ ...client.SubResourceCreateOption) error {
	return pdbRefusal(obj.GetName())
}

// cancelParentInList returns interceptor functions that refuse every eviction and
// cancel the parent context in the list with the given number. If failList is
// true, that list fails with the error of the context, as client-go does. A later
// list fails the test.
func cancelParentInList(number int, failList bool) func(t *testing.T, cancelParent func()) interceptor.Funcs {
	return func(t *testing.T, cancelParent func()) interceptor.Funcs {
		t.Helper()
		lists := 0
		return interceptor.Funcs{
			SubResourceCreate: refuseEviction,
			List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				lists++
				if lists > number {
					t.Errorf("list %d after the parent context was canceled in list %d", lists, number)
				}
				if lists == number {
					cancelParent()
					if failList {
						return ctx.Err()
					}
				}
				return underlying.List(ctx, list, opts...)
			},
		}
	}
}

// pdbRefusal returns the error of the API server for an eviction that the named
// PodDisruptionBudget refuses.
func pdbRefusal(budget string) error {
	err := apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 0)
	err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{
		Type:    policyv1.DisruptionBudgetCause,
		Message: fmt.Sprintf("The disruption budget %s needs 2 healthy pods and has 1 currently", budget),
	})
	return err
}

// forbiddenRefusal returns a 403 error of the API server for an eviction of the
// named pod, with the cause.
func forbiddenRefusal(name string, cause metav1.CauseType) error {
	err := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, name, errors.New("eviction refused"))
	err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{Type: cause})
	return err
}

func executeDrainKubeNodeActivity(
	t *testing.T,
	a *Activities,
	input DrainKubeNodeInput,
) error {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	testActivity := func(ctx context.Context, input DrainKubeNodeInput) error {
		return a.drainKubeNode(ctx, input, time.Millisecond, time.Second)
	}
	env.RegisterActivityWithOptions(testActivity, temporalactivity.RegisterOptions{Name: "TestDrainKubeNode"})

	_, err := env.ExecuteActivity("TestDrainKubeNode", input)
	return err
}

func assertPodExists(t *testing.T, tenantClient client.Client, namespace, name string) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &pod))
}

func assertPodNotFound(t *testing.T, tenantClient client.Client, namespace, name string) {
	t.Helper()
	var pod corev1.Pod
	err := tenantClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &pod)
	require.True(t, apierrors.IsNotFound(err))
}
