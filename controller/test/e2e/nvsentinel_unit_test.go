// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestPodsNotReady(t *testing.T) {
	pod := func(name string, phase corev1.PodPhase, ready bool) corev1.Pod {
		p := mokkaTestPod("a", ready)
		p.Name, p.Status.Phase = name, phase
		return p
	}
	for _, tc := range []struct {
		name  string
		pods  []corev1.Pod
		found []string
	}{
		{name: "no pods yet", found: []string{"no pods yet"}},
		{
			name: "running pods Ready and a finished Job pod",
			pods: []corev1.Pod{pod("store", corev1.PodRunning, true), pod("setup", corev1.PodSucceeded, false)},
		},
		{
			name:  "a running pod that is not Ready",
			pods:  []corev1.Pod{pod("store", corev1.PodRunning, true), pod("drainer", corev1.PodRunning, false)},
			found: []string{"drainer is not Ready"},
		},
		{
			name: "a failed attempt of a Job that was retried",
			pods: []corev1.Pod{pod("setup-1", corev1.PodFailed, false), pod("setup-2", corev1.PodSucceeded, false)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.found, podsNotReady(tc.pods))
		})
	}
}

func TestJobsIncomplete(t *testing.T) {
	job := func(name string, condition batchv1.JobConditionType) batchv1.Job {
		j := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if condition != "" {
			j.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}}
		}
		return j
	}
	for _, tc := range []struct {
		name  string
		jobs  []batchv1.Job
		found []string
	}{
		{name: "no Jobs"},
		{name: "a completed Job", jobs: []batchv1.Job{job("setup", batchv1.JobComplete)}},
		{name: "a Job still running", jobs: []batchv1.Job{job("setup", "")}, found: []string{"setup has not completed"}},
		{name: "a Job out of retries", jobs: []batchv1.Job{job("setup", batchv1.JobFailed)}, found: []string{"setup has not completed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.found, jobsIncomplete(tc.jobs))
		})
	}
}

func TestNVSentinelValues(t *testing.T) {
	t.Log("Testing: the embedded values cordon on a hot GPU and need no StorageClass")
	var values struct {
		GPUHealthMonitor struct {
			DCGMFieldsMonitoring struct {
				GPUTempLimitStoreOnly *bool `json:"gpuTempLimitStoreOnly"`
			} `json:"dcgmFieldsMonitoring"`
		} `json:"gpu-health-monitor"`
		MongoDBStore struct {
			PSMDB struct {
				Replsets struct {
					RS0 struct {
						VolumeSpec map[string]any `json:"volumeSpec"`
					} `json:"rs0"`
				} `json:"replsets"`
			} `json:"psmdb-db"`
		} `json:"mongodb-store"`
	}
	require.NoError(t, yaml.Unmarshal(nvsentinelValues, &values))
	require.Equal(t, new(bool), values.GPUHealthMonitor.DCGMFieldsMonitoring.GPUTempLimitStoreOnly)
	require.Equal(t, map[string]any{"emptyDir": map[string]any{}, "pvc": nil}, values.MongoDBStore.PSMDB.Replsets.RS0.VolumeSpec)
}
