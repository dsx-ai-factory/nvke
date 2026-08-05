// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nke

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	cweNamespace      = "cloud-workflow-engine"
	cweDeployment     = "cloud-workflow-engine"
	oidcEnabledEnvVar = "NKE_OIDC_AUTH_ENABLED"
)

// OIDCEnabled reports whether the CWE deployment on the management cluster has
// tenant OIDC JWT auth turned on (NKE_OIDC_AUTH_ENABLED=true on its container). The OIDC
// validations only apply when CWE configures tenant OIDC auth, so the suite uses this
// to skip them on deployments where OIDC is off.
func OIDCEnabled(ctx context.Context, c client.Client) (bool, error) {
	var deploy appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: cweNamespace, Name: cweDeployment}, &deploy); err != nil {
		return false, fmt.Errorf("getting CWE deployment %s/%s: %w", cweNamespace, cweDeployment, err)
	}
	for _, container := range deploy.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name != oidcEnabledEnvVar {
				continue
			}
			if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
				return oidcEnabledFromConfigMap(ctx, c, deploy.Namespace, env.ValueFrom.ConfigMapKeyRef.Name, env.ValueFrom.ConfigMapKeyRef.Key)
			}
			return env.Value == "true", nil
		}
		for _, envFrom := range container.EnvFrom {
			if envFrom.ConfigMapRef == nil {
				continue
			}
			enabled, found, err := oidcEnabledFromConfigMapData(ctx, c, deploy.Namespace, envFrom.ConfigMapRef.Name)
			if err != nil {
				if envFrom.ConfigMapRef.Optional != nil && *envFrom.ConfigMapRef.Optional && apierrors.IsNotFound(err) {
					continue
				}
				return false, err
			}
			if found {
				return enabled, nil
			}
		}
	}
	return false, nil
}

// SetOIDCEnabled overrides NKE_OIDC_AUTH_ENABLED on the CWE deployment's app
// container with a direct env value (which takes precedence over any
// ConfigMap-sourced value) and waits for the rollout to finish. The OIDC issuer /
// discovery / CA env the enable workflow needs are left in place, so toggling this
// flag alone moves CWE between "new clusters are created OIDC-off" and "OIDC is
// active" without a redeploy.
//
// The brownfield OIDC spec uses this to construct its precondition: a cluster on
// an OIDC-enabled site is always born OIDC-on, so the spec creates its cluster with
// the flag off, then flips it back on before enabling.
func SetOIDCEnabled(ctx context.Context, c client.Client, enabled bool) error {
	var deploy appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: cweNamespace, Name: cweDeployment}, &deploy); err != nil {
		return fmt.Errorf("getting CWE deployment %s/%s: %w", cweNamespace, cweDeployment, err)
	}
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return fmt.Errorf("CWE deployment %s/%s has no containers", cweNamespace, cweDeployment)
	}
	idx := 0
	for i := range containers {
		if containers[i].Name == cweDeployment {
			idx = i
			break
		}
	}
	setContainerEnv(&containers[idx], oidcEnabledEnvVar, strconv.FormatBool(enabled))
	if err := c.Update(ctx, &deploy); err != nil {
		return fmt.Errorf("setting %s=%t on CWE deployment: %w", oidcEnabledEnvVar, enabled, err)
	}
	return waitForDeploymentRollout(ctx, c, cweNamespace, cweDeployment)
}

// setContainerEnv upserts a plain-value env var, clearing any ValueFrom so the
// literal value wins over a ConfigMap/Secret source.
func setContainerEnv(container *corev1.Container, name, value string) {
	for i := range container.Env {
		if container.Env[i].Name == name {
			container.Env[i].Value = value
			container.Env[i].ValueFrom = nil
			return
		}
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: value})
}

// waitForDeploymentRollout blocks until the deployment has observed its latest
// generation and every replica is updated and available — the conditions
// `kubectl rollout status` waits on.
func waitForDeploymentRollout(ctx context.Context, c client.Client, namespace, name string) error {
	return wait.PollUntilContextTimeout(ctx, 3*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		var deploy appsv1.Deployment
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &deploy); err != nil {
			return false, fmt.Errorf("getting deployment %s/%s: %w", namespace, name, err)
		}
		if deploy.Generation != deploy.Status.ObservedGeneration {
			return false, nil
		}
		desired := int32(1)
		if deploy.Spec.Replicas != nil {
			desired = *deploy.Spec.Replicas
		}
		return deploy.Status.UpdatedReplicas == desired &&
			deploy.Status.Replicas == desired &&
			deploy.Status.AvailableReplicas == desired, nil
	})
}

func oidcEnabledFromConfigMap(ctx context.Context, c client.Client, namespace, name, key string) (bool, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		return false, fmt.Errorf("getting ConfigMap %s/%s: %w", namespace, name, err)
	}
	return cm.Data[key] == "true", nil
}

func oidcEnabledFromConfigMapData(ctx context.Context, c client.Client, namespace, name string) (bool, bool, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		return false, false, fmt.Errorf("getting ConfigMap %s/%s: %w", namespace, name, err)
	}
	value, found := cm.Data[oidcEnabledEnvVar]
	return value == "true", found, nil
}
