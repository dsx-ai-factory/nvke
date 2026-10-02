// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/cwe"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
)

const (
	oidcEnabledEnvVar = "NKE_OIDC_AUTH_ENABLED"
	enabledValue      = "true"

	// permissiveModeEnvVar is what NKE reads to decide whether a failed check
	// rejects the request or is only counted.
	permissiveModeEnvVar = "PERMISSIVE_MODE"
)

// OIDCEnabled reports whether the CWE deployment on the management cluster has
// tenant OIDC JWT auth turned on (NKE_OIDC_AUTH_ENABLED=true on its container). The OIDC
// validations only apply when CWE configures tenant OIDC auth, so the suite uses this
// to skip them on deployments where OIDC is off.
func OIDCEnabled(ctx context.Context, c client.Client) (bool, error) {
	var deploy appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: cwe.Namespace(), Name: cwe.Deployment()}, &deploy); err != nil {
		return false, fmt.Errorf("getting NKE deployment %s/%s: %w", cwe.Namespace(), cwe.Deployment(), err)
	}
	for _, container := range deploy.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name != oidcEnabledEnvVar {
				continue
			}
			if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
				return oidcEnabledFromConfigMap(ctx, c, deploy.Namespace, env.ValueFrom.ConfigMapKeyRef.Name, env.ValueFrom.ConfigMapKeyRef.Key)
			}
			return env.Value == enabledValue, nil
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
	_, err := setNKEContainerEnv(ctx, c, oidcEnabledEnvVar, strconv.FormatBool(enabled))
	return err
}

// SetPermissiveMode sets PERMISSIVE_MODE on the NKE container and waits for the
// rollout, returning the value it replaced so a caller can put it back.
//
// An absent variable comes back as "", and restoring that sets it to the empty
// string rather than removing it. NKE reads both as enforcing.
func SetPermissiveMode(ctx context.Context, c client.Client, value string) (string, error) {
	return setNKEContainerEnv(ctx, c, permissiveModeEnvVar, value)
}

// setNKEContainerEnv upserts an environment variable on the NKE container and
// waits for the rollout. It returns the value it replaced.
//
// The container is found by name, with no fallback. Guessing container 0 writes
// the variable onto whichever sidecar happens to be first, then waits out a
// rollout that changed nothing.
func setNKEContainerEnv(ctx context.Context, c client.Client, name, value string) (string, error) {
	var deploy appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: cwe.Namespace(), Name: cwe.Deployment()}, &deploy); err != nil {
		return "", fmt.Errorf("getting NKE deployment %s/%s: %w", cwe.Namespace(), cwe.Deployment(), err)
	}

	containers := deploy.Spec.Template.Spec.Containers
	idx := -1
	for i := range containers {
		if containers[i].Name == cwe.Deployment() {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("NKE deployment %s/%s has no container named %q",
			cwe.Namespace(), cwe.Deployment(), cwe.Deployment())
	}

	var previous string
	for _, env := range containers[idx].Env {
		if env.Name == name {
			previous = env.Value
			break
		}
	}

	setContainerEnv(&containers[idx], name, value)
	if err := c.Update(ctx, &deploy); err != nil {
		return "", fmt.Errorf("setting %s=%q on the NKE deployment: %w", name, value, err)
	}
	if err := waitForDeploymentRollout(ctx, c, cwe.Namespace(), cwe.Deployment()); err != nil {
		return "", err
	}

	// The rollout replaced the pod the shared forwards point at. Their local
	// listeners stay up, so the next request through one reaches a pod that is
	// gone and fails with EOF; only after that does the session end and rebuild.
	// Dropping the sessions here means the next request builds a new tunnel.
	//
	// Reset rather than Close: the API client in this package holds a forwarder
	// from before the rollout, and a closed one never reconnects.
	kube.ResetSharedForwarders()
	return previous, nil
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
	return cm.Data[key] == enabledValue, nil
}

func oidcEnabledFromConfigMapData(ctx context.Context, c client.Client, namespace, name string) (bool, bool, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		return false, false, fmt.Errorf("getting ConfigMap %s/%s: %w", namespace, name, err)
	}
	value, found := cm.Data[oidcEnabledEnvVar]
	return value == enabledValue, found, nil
}
