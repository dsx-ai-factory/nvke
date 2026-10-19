// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvke

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	// ControllerConfigMap is the ConfigMap the controller reads its settings
	// from, once, at startup.
	ControllerConfigMap = "nvke-controller-config"
	controllerConfigKey = "config.yaml"

	// restartedAtAnnotation is the pod-template annotation `kubectl rollout
	// restart` sets.
	restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

	// oidcEnabledKey turns on tenant OIDC JWT auth for clusters created from
	// then on.
	oidcEnabledKey = "oidc.managementOIDCAuth.enabled"
	// permissiveModeKey decides whether a failed authentication check rejects
	// the request or is only counted.
	permissiveModeKey = "permissiveMode"
)

// ControllerSetting returns the value at key, a dotted config.yaml path such as
// oidc.managementOIDCAuth.enabled, in data, the data of the controller's
// nke-controller-config ConfigMap.
func ControllerSetting(data map[string]string, key string) (any, error) {
	settings, err := parseControllerConfig(data)
	if err != nil {
		return nil, err
	}
	value, _, err := lookup(settings, key)
	return value, err
}

// OIDCEnabled reports whether the controller has tenant OIDC JWT auth turned on
// (oidc.managementOIDCAuth.enabled in nke-controller-config). The OIDC
// validations only apply when it does, so the suite uses this to skip them on
// deployments where OIDC is off.
func OIDCEnabled(ctx context.Context, c client.Client) (bool, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: controller.Namespace(), Name: ControllerConfigMap}, &cm); err != nil {
		return false, fmt.Errorf("getting ConfigMap %s/%s: %w", controller.Namespace(), ControllerConfigMap, err)
	}
	value, err := ControllerSetting(cm.Data, oidcEnabledKey)
	if err != nil {
		return false, err
	}
	return value == true, nil
}

// SetOIDCEnabled sets oidc.managementOIDCAuth.enabled in nke-controller-config,
// restarts the controller and waits for the rollout. The issuer, discovery and
// CA settings the enable workflow needs are left in place, so toggling this
// flag alone moves the controller between "new clusters are created OIDC-off"
// and "OIDC is active" without a redeploy.
//
// The brownfield OIDC spec uses this to construct its precondition: a cluster on
// an OIDC-enabled site is always born OIDC-on, so the spec creates its cluster with
// the flag off, then flips it back on before enabling.
func SetOIDCEnabled(ctx context.Context, c client.Client, enabled bool) error {
	_, _, err := setControllerSetting(ctx, c, oidcEnabledKey, enabled)
	return err
}

// SetPermissiveMode sets permissiveMode in nke-controller-config, restarts the
// controller and waits for the rollout, returning the value it replaced so a
// caller can put it back. The second result reports whether the ConfigMap was
// written. It stays true when the restart after the write fails, and the caller
// must put the old value back then too: the enforcing specs read the mode from
// the ConfigMap, so a permissive value left there makes them skip.
func SetPermissiveMode(ctx context.Context, c client.Client, permissive bool) (bool, bool, error) {
	previous, written, err := setControllerSetting(ctx, c, permissiveModeKey, permissive)
	return previous == true, written, err
}

// setControllerSetting sets one key of nke-controller-config and restarts the
// controller, which reads the file only at startup. It returns the value it
// replaced and whether it wrote the ConfigMap, including when the restart that
// follows the write fails.
func setControllerSetting(ctx context.Context, c client.Client, key string, value any) (any, bool, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: controller.Namespace(), Name: ControllerConfigMap}, &cm); err != nil {
		return nil, false, fmt.Errorf("getting ConfigMap %s/%s: %w", controller.Namespace(), ControllerConfigMap, err)
	}
	settings, err := parseControllerConfig(cm.Data)
	if err != nil {
		return nil, false, err
	}
	previous, parent, err := lookup(settings, key)
	if err != nil {
		return nil, false, err
	}
	parent[key[strings.LastIndex(key, ".")+1:]] = value

	updated, err := yaml.Marshal(settings)
	if err != nil {
		return nil, false, fmt.Errorf("encoding %s: %w", ControllerConfigMap, err)
	}
	cm.Data[controllerConfigKey] = string(updated)
	if err := c.Update(ctx, &cm); err != nil {
		return nil, false, fmt.Errorf("setting %s=%v in %s: %w", key, value, ControllerConfigMap, err)
	}
	if err := restartController(ctx, c); err != nil {
		return previous, true, err
	}
	return previous, true, nil
}

// restartController rolls the controller's pods so they read the current
// nke-controller-config, and waits for the rollout.
//
// Where Reloader runs, the ConfigMap edit makes it update the Deployment too, so
// that update can land between the read and the write here; a conflict is
// retried on a fresh read. The rollout wait also covers a Reloader update that
// lands while it waits, since that bumps the generation it checks.
func restartController(ctx context.Context, c client.Client) error {
	key := client.ObjectKey{Namespace: controller.Namespace(), Name: controller.Deployment()}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var deploy appsv1.Deployment
		if err := c.Get(ctx, key, &deploy); err != nil {
			return fmt.Errorf("getting NVKE deployment %s: %w", key, err)
		}
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = map[string]string{}
		}
		deploy.Spec.Template.Annotations[restartedAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		return c.Update(ctx, &deploy)
	})
	if err != nil {
		return fmt.Errorf("restarting the NVKE deployment: %w", err)
	}
	if err := waitForDeploymentRollout(ctx, c, controller.Namespace(), controller.Deployment()); err != nil {
		return err
	}

	// The rollout replaced the pod the shared forwards point at. Their local
	// listeners stay up, so the next request through one reaches a pod that is
	// gone and fails with EOF; only after that does the session end and rebuild.
	// Dropping the sessions here means the next request builds a new tunnel.
	//
	// Reset rather than Close: the API client in this package holds a forwarder
	// from before the rollout, and a closed one never reconnects.
	kube.ResetSharedForwarders()
	return nil
}

func parseControllerConfig(data map[string]string) (map[string]any, error) {
	raw, ok := data[controllerConfigKey]
	if !ok {
		return nil, fmt.Errorf("%s has no %s", ControllerConfigMap, controllerConfigKey)
	}
	var settings map[string]any
	if err := yaml.Unmarshal([]byte(raw), &settings); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ControllerConfigMap, err)
	}
	return settings, nil
}

// lookup returns the value at the dotted key and the map holding it.
func lookup(settings map[string]any, key string) (any, map[string]any, error) {
	parts := strings.Split(key, ".")
	node := settings
	for _, part := range parts[:len(parts)-1] {
		next, ok := node[part].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s has no %s", ControllerConfigMap, key)
		}
		node = next
	}
	value, ok := node[parts[len(parts)-1]]
	if !ok {
		return nil, nil, fmt.Errorf("%s has no %s", ControllerConfigMap, key)
	}
	return value, node, nil
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

// SetNodeAgentUpdateMethod configures the isolated test controller installation method.
func SetNodeAgentUpdateMethod(ctx context.Context, c client.Client, method string) (string, bool, error) {
	previous, written, err := setControllerSetting(ctx, c, "nodeAgent.updateMethod", method)
	value, _ := previous.(string)
	return value, written, err
}

// RestartNodeAgentTestController restarts the isolated controller without changing desired state.
func RestartNodeAgentTestController(ctx context.Context, c client.Client) error {
	return restartController(ctx, c)
}
