// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package oidc contains the small pieces the live OIDC acceptance test needs.
package oidc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

const (
	startupMessage = "Loaded nke-controller-config"
	defaultRestart = "e2e.nke.nvidia.com/oidc-restart"
	maxLogBytes    = 1 << 20
	chartConfigMap = "nke-controller-config"
	chartConfigKey = "config.yaml"
	// Match controller/internal/controllerconfig.MaxOIDCProviders in this standalone module.
	maxOIDCProviders = 5
)

// Provider is the part of controller configuration copied into new clusters.
type Provider struct {
	Name           string            `json:"name"`
	IssuerURL      string            `json:"issuerURL"`
	Audiences      []string          `json:"audiences"`
	UsernameClaim  string            `json:"usernameClaim"`
	UsernamePrefix string            `json:"usernamePrefix"`
	GroupsClaim    string            `json:"groupsClaim,omitempty"`
	GroupsPrefix   string            `json:"groupsPrefix,omitempty"`
	RequiredClaims map[string]string `json:"requiredClaims,omitempty"`
	CABundle       string            `json:"caBundle"`
}

// SiteConfig names the chart objects and bounds rollout waits.
type SiteConfig struct {
	Namespace       string
	Deployment      string
	Container       string
	ReadyTimeout    time.Duration
	PollInterval    time.Duration
	ResetForwarders func()
	ReadPodLogs     func(context.Context, string, string, string) ([]byte, error)
}

// Snapshot holds the exact fields Site changes. Its contents stay in memory.
type Snapshot struct {
	config     configState
	deployment deploymentState
}

type configState struct {
	present bool
	value   []byte
}

type deploymentState struct {
	annotationPresent bool
	annotationValue   string
}

// Site changes only the controller fields owned by this acceptance test.
type Site struct {
	client kubernetes.Interface
	config SiteConfig

	mu               sync.Mutex
	original         *Snapshot
	expectedConfig   configState
	expectedDeploy   deploymentState
	targetGeneration int64
}

// NewSite creates a controller configuration editor.
func NewSite(client kubernetes.Interface, config SiteConfig) (*Site, error) {
	if client == nil {
		return nil, errors.New("kubernetes client is required")
	}
	if config.Namespace == "" || config.Deployment == "" || config.Container == "" {
		return nil, errors.New("controller namespace, Deployment, and container are required")
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = 10 * time.Minute
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 3 * time.Second
	}
	return &Site{client: client, config: config}, nil
}

// SharedIssuer is the issuer the shared provider renders from: the issuer and
// discovery URLs in the controller's current configuration, and the PEM bundle
// in the CA Secret it names.
func (s *Site) SharedIssuer(ctx context.Context) (*apiserverv1.Issuer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.currentConfig(ctx)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err = yaml.Unmarshal(current.value, &root); err != nil {
		return nil, errors.New("decode controller config: invalid YAML")
	}
	oidcConfig, err := childMap(root, "oidc")
	if err != nil {
		return nil, err
	}
	management, err := childMap(oidcConfig, "managementOIDCAuth")
	if err != nil {
		return nil, err
	}
	issuer := &apiserverv1.Issuer{}
	issuer.URL, _ = management["issuerURL"].(string)
	if discoveryURL, _ := management["discoveryURL"].(string); discoveryURL != "" {
		issuer.DiscoveryURL = &discoveryURL
	}
	ref, _ := management["caBundleSecretRef"].(map[string]any)
	namespace, _ := ref["namespace"].(string)
	name, _ := ref["name"].(string)
	key, _ := ref["key"].(string)
	if namespace == "" && name == "" && key == "" {
		return issuer, nil
	}
	secret, err := s.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get shared CA Secret %s/%s: %w", namespace, name, err)
	}
	bundle := secret.Data[key]
	if len(bytes.TrimSpace(bundle)) == 0 {
		return nil, fmt.Errorf("shared CA Secret %s/%s has no %q bundle", namespace, name, key)
	}
	issuer.CertificateAuthority = string(bundle)
	return issuer, nil
}

// Snapshot records the raw ConfigMap value and restart annotation.
func (s *Site) Snapshot(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.original != nil {
		return Snapshot{}, errors.New("restore the active OIDC site edit before taking another snapshot")
	}

	cm, err := s.client.CoreV1().ConfigMaps(s.config.Namespace).Get(ctx, chartConfigMap, metav1.GetOptions{})
	if err != nil {
		return Snapshot{}, fmt.Errorf("get controller ConfigMap: %w", err)
	}
	deployment, err := s.client.AppsV1().Deployments(s.config.Namespace).Get(ctx, s.config.Deployment, metav1.GetOptions{})
	if err != nil {
		return Snapshot{}, fmt.Errorf("get controller Deployment: %w", err)
	}
	config := readConfigState(cm)
	deployed, err := readDeploymentState(deployment, s.config.Container)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{config: config, deployment: deployed}
	s.original = &snapshot
	s.expectedConfig = config
	s.expectedDeploy = deployed
	return snapshot, nil
}

// Apply replaces fleet OIDC providers and controls the management SPIRE provider.
func (s *Site) Apply(ctx context.Context, providers []Provider, shared bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.original == nil {
		return errors.New("snapshot the site before applying OIDC configuration")
	}

	currentConfig, err := s.currentConfig(ctx)
	if err != nil {
		return err
	}
	if !equalConfig(currentConfig, s.expectedConfig) {
		return errors.New("controller ConfigMap key changed outside the OIDC test")
	}
	if !currentConfig.present {
		return fmt.Errorf("controller ConfigMap key %q is absent", chartConfigKey)
	}
	patched, err := patchConfig(currentConfig.value, providers, shared)
	if err != nil {
		return err
	}
	desiredConfig := configState{present: true, value: patched}
	if updateErr := s.updateConfig(ctx, currentConfig, desiredConfig); updateErr != nil {
		return updateErr
	}
	s.expectedConfig = desiredConfig

	currentDeploy, err := s.currentDeployment(ctx)
	if err != nil {
		return err
	}
	if !equalDeployment(currentDeploy, s.expectedDeploy) {
		return errors.New("controller restart annotation changed outside the OIDC test")
	}
	desiredDeploy := currentDeploy
	desiredDeploy.annotationPresent = true
	desiredDeploy.annotationValue = fmt.Sprintf("%d", time.Now().UnixNano())
	applied, generation, err := s.updateDeployment(ctx, currentDeploy, desiredDeploy)
	if err != nil {
		return err
	}
	s.expectedDeploy = applied
	s.targetGeneration = generation
	return s.waitReadyLocked(ctx)
}

// WaitForStartupProviderCount checks the current controller pods, not stale replicas.
func (s *Site) WaitForStartupProviderCount(ctx context.Context, expected int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pollCtx, cancel := context.WithTimeout(ctx, s.config.ReadyTimeout)
	defer cancel()
	err := wait.PollUntilContextCancel(pollCtx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		deployment, err := s.client.AppsV1().Deployments(s.config.Namespace).Get(ctx, s.config.Deployment, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get controller Deployment: %w", err)
		}
		selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		if err != nil {
			return false, fmt.Errorf("read controller selector: %w", err)
		}
		pods, err := s.client.CoreV1().Pods(s.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return false, fmt.Errorf("list controller pods: %w", err)
		}
		wanted := int32(1)
		if deployment.Spec.Replicas != nil {
			wanted = *deployment.Spec.Replicas
		}
		if wanted < 1 {
			return false, errors.New("controller Deployment has no desired replicas to witness")
		}
		matched := int32(0)
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Annotations[defaultRestart] != s.expectedDeploy.annotationValue || !containerReady(pod, s.config.Container) {
				continue
			}
			matched++
			logs, readErr := s.podLogs(ctx, pod.Name)
			if readErr != nil {
				return false, readErr
			}
			count, found := providerCount(logs)
			if !found {
				return false, nil
			}
			if count != expected {
				return false, fmt.Errorf("controller pod %s loaded %d OIDC providers, want %d", pod.Name, count, expected)
			}
		}
		return matched == wanted, nil
	})
	if err != nil {
		err = deadlineExceededAfter(pollCtx, err)
		return fmt.Errorf("wait up to %s for controller %s/%s pods to report startup OIDC provider count %d: %w",
			s.config.ReadyTimeout, s.config.Namespace, s.config.Deployment, expected, err)
	}
	return nil
}

// deadlineExceededAfter reports a request that failed once ctx's deadline
// had passed as context.DeadlineExceeded. client-go's rate limiter refuses
// a request whose wait would cross the deadline with its own error, which
// does not wrap context.DeadlineExceeded, before ctx itself is done.
func deadlineExceededAfter(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
	}
	return err
}

// Restore puts the exact original bytes and deployment fields back.
func (s *Site) Restore(ctx context.Context, snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.original == nil || !equalConfig(snapshot.config, s.original.config) || !equalDeployment(snapshot.deployment, s.original.deployment) {
		return errors.New("snapshot does not belong to the active OIDC site edit")
	}
	currentConfig, err := s.currentConfig(ctx)
	if err != nil {
		return err
	}
	if !equalConfig(currentConfig, s.expectedConfig) {
		return errors.New("controller ConfigMap key changed outside the OIDC test")
	}
	if updateErr := s.updateConfig(ctx, currentConfig, snapshot.config); updateErr != nil {
		return updateErr
	}
	s.expectedConfig = snapshot.config

	currentDeploy, err := s.currentDeployment(ctx)
	if err != nil {
		return err
	}
	if !equalDeployment(currentDeploy, s.expectedDeploy) {
		return errors.New("controller restart annotation changed outside the OIDC test")
	}
	applied, generation, err := s.updateDeployment(ctx, currentDeploy, snapshot.deployment)
	if err != nil {
		return err
	}
	s.expectedDeploy = applied
	s.targetGeneration = generation
	if err := s.waitReadyLocked(ctx); err != nil {
		return err
	}
	s.original = nil
	return nil
}

func (s *Site) waitReadyLocked(ctx context.Context) error {
	err := wait.PollUntilContextTimeout(ctx, s.config.PollInterval, s.config.ReadyTimeout, true, func(ctx context.Context) (bool, error) {
		config, err := s.currentConfig(ctx)
		if err != nil {
			return false, err
		}
		if !equalConfig(config, s.expectedConfig) {
			return false, errors.New("controller ConfigMap key changed during rollout")
		}
		deployment, err := s.client.AppsV1().Deployments(s.config.Namespace).Get(ctx, s.config.Deployment, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get controller Deployment rollout: %w", err)
		}
		state, err := readDeploymentState(deployment, s.config.Container)
		if err != nil {
			return false, err
		}
		if !equalDeployment(state, s.expectedDeploy) {
			return false, errors.New("controller restart annotation changed during rollout")
		}
		if deployment.Generation > s.targetGeneration {
			s.targetGeneration = deployment.Generation
		}
		desired := int32(1)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		ready := deployment.Status.ObservedGeneration >= s.targetGeneration &&
			deployment.Status.Replicas == desired && deployment.Status.UpdatedReplicas == desired &&
			deployment.Status.ReadyReplicas == desired && deployment.Status.AvailableReplicas == desired
		return ready, nil
	})
	if err != nil {
		return fmt.Errorf("wait for controller OIDC rollout: %w", err)
	}
	if s.config.ResetForwarders != nil {
		s.config.ResetForwarders()
	}
	return nil
}

func (s *Site) currentConfig(ctx context.Context) (configState, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.config.Namespace).Get(ctx, chartConfigMap, metav1.GetOptions{})
	if err != nil {
		return configState{}, fmt.Errorf("get controller ConfigMap: %w", err)
	}
	return readConfigState(cm), nil
}

func (s *Site) currentDeployment(ctx context.Context) (deploymentState, error) {
	deployment, err := s.client.AppsV1().Deployments(s.config.Namespace).Get(ctx, s.config.Deployment, metav1.GetOptions{})
	if err != nil {
		return deploymentState{}, fmt.Errorf("get controller Deployment: %w", err)
	}
	return readDeploymentState(deployment, s.config.Container)
}

func (s *Site) updateConfig(ctx context.Context, expected, desired configState) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.client.CoreV1().ConfigMaps(s.config.Namespace).Get(ctx, chartConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		current := readConfigState(cm)
		if !equalConfig(current, expected) {
			return errors.New("controller ConfigMap key changed outside the OIDC test")
		}
		writeConfigState(cm, desired)
		_, err = s.client.CoreV1().ConfigMaps(s.config.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func (s *Site) updateDeployment(ctx context.Context, expected, desired deploymentState) (deploymentState, int64, error) {
	var applied deploymentState
	var generation int64
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := s.client.AppsV1().Deployments(s.config.Namespace).Get(ctx, s.config.Deployment, metav1.GetOptions{})
		if err != nil {
			return err
		}
		current, err := readDeploymentState(deployment, s.config.Container)
		if err != nil {
			return err
		}
		if !equalDeployment(current, expected) {
			return errors.New("controller restart annotation changed outside the OIDC test")
		}
		if writeErr := writeDeploymentState(deployment, s.config.Container, desired); writeErr != nil {
			return writeErr
		}
		updated, err := s.client.AppsV1().Deployments(s.config.Namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		generation = updated.Generation
		applied, err = readDeploymentState(updated, s.config.Container)
		return err
	})
	return applied, generation, err
}

func (s *Site) podLogs(ctx context.Context, pod string) ([]byte, error) {
	if s.config.ReadPodLogs != nil {
		return s.config.ReadPodLogs(ctx, s.config.Namespace, pod, s.config.Container)
	}
	stream, err := s.client.CoreV1().Pods(s.config.Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: s.config.Container}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("read controller pod %s logs: %w", pod, err)
	}
	defer stream.Close()
	output, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	if err != nil {
		return nil, fmt.Errorf("read controller pod %s logs: %w", pod, err)
	}
	return output, nil
}

func patchConfig(input []byte, providers []Provider, shared bool) ([]byte, error) {
	var root map[string]any
	if err := yaml.Unmarshal(input, &root); err != nil {
		return nil, errors.New("decode controller config: invalid YAML")
	}
	oidcConfig, err := childMap(root, "oidc")
	if err != nil {
		return nil, err
	}
	management, err := childMap(oidcConfig, "managementOIDCAuth")
	if err != nil {
		return nil, err
	}
	issuerURL, _ := management["issuerURL"].(string)
	if shared && strings.TrimSpace(issuerURL) == "" {
		return nil, errors.New("managementOIDCAuth.issuerURL is required when managementOIDCAuth.enabled is true")
	}
	if len(providers) > maxOIDCProviders {
		return nil, fmt.Errorf("providers has %d entries, more than the %d-entry cap", len(providers), maxOIDCProviders)
	}
	management["enabled"] = shared
	oidcConfig["providers"] = providers
	output, err := yaml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode controller config: %w", err)
	}
	return output, nil
}

func childMap(parent map[string]any, key string) (map[string]any, error) {
	value, ok := parent[key]
	if !ok {
		return nil, fmt.Errorf("controller config has no %q mapping", key)
	}
	child, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("controller config field %q is not a mapping", key)
	}
	return child, nil
}

func readConfigState(cm *corev1.ConfigMap) configState {
	value, present := cm.Data[chartConfigKey]
	return configState{present: present, value: []byte(value)}
}

func writeConfigState(cm *corev1.ConfigMap, state configState) {
	delete(cm.Data, chartConfigKey)
	if !state.present {
		return
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[chartConfigKey] = string(state.value)
}

func readDeploymentState(deployment *appsv1.Deployment, containerName string) (deploymentState, error) {
	if err := validateContainer(deployment, containerName); err != nil {
		return deploymentState{}, err
	}
	state := deploymentState{}
	state.annotationValue, state.annotationPresent = deployment.Spec.Template.Annotations[defaultRestart]
	return state, nil
}

func writeDeploymentState(deployment *appsv1.Deployment, containerName string, state deploymentState) error {
	if err := validateContainer(deployment, containerName); err != nil {
		return err
	}
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	if state.annotationPresent {
		deployment.Spec.Template.Annotations[defaultRestart] = state.annotationValue
	} else {
		delete(deployment.Spec.Template.Annotations, defaultRestart)
	}
	return nil
}

func validateContainer(deployment *appsv1.Deployment, name string) error {
	for i := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[i].Name == name {
			return nil
		}
	}
	return fmt.Errorf("controller Deployment has no container %q", name)
}

func providerCount(logs []byte) (int, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(logs))
	scanner.Buffer(make([]byte, 4096), maxLogBytes)
	for scanner.Scan() {
		var event struct {
			Message string `json:"msg"`
			Count   *int   `json:"oidcProviders"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Message == startupMessage && event.Count != nil {
			return *event.Count, true
		}
	}
	return 0, false
}

func containerReady(pod *corev1.Pod, name string) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == name {
			return status.Ready
		}
	}
	return false
}

func equalConfig(left, right configState) bool {
	return left.present == right.present && bytes.Equal(left.value, right.value)
}

func equalDeployment(left, right deploymentState) bool {
	return left.annotationPresent == right.annotationPresent && left.annotationValue == right.annotationValue
}
