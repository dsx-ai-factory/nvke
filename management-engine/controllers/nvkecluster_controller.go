// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Controller setup and reconcile dispatch coordinate lifecycle work and status persistence.

package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/finalizers"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/nvkecontroller"
)

const (
	nvkeClusterFinalizer    = "internal.nvke.nvidia.com/cluster-cleanup"
	defaultPollInterval     = 10 * time.Second
	clusterNameSuffix       = "-mgmt-eng"
	clusterNameMaxLength    = 55
	clusterTemplateKey      = "clusterTmpl"
	nodePoolTemplateKey     = "nodepoolTmpl"
	controlPlaneTemplateKey = "controlplaneTmpl"
)

// NVKEControllerClient is the existing REST API surface used by cluster reconciliation.
type NVKEControllerClient interface {
	FindByName(ctx context.Context, name string) (*api.Cluster, error)
	Create(ctx context.Context, request api.CreateClusterRequest) (*api.Cluster, error)
	Delete(ctx context.Context, id string) error
	FindNodePool(ctx context.Context, clusterName, name string) (*api.NodePool, error)
	CreateNodePool(ctx context.Context, request api.CreateNodePoolRequest) (*api.NodePool, error)
	UpdateNodePool(ctx context.Context, id string, request api.UpdateNodePoolRequest) (*api.NodePool, error)
	DeleteNodePool(ctx context.Context, id string) error
}

// NVKEClusterReconciler coordinates cluster provisioning from declarative intent.
type NVKEClusterReconciler struct {
	client.Client
	NVKEController             NVKEControllerClient
	PollInterval               time.Duration
	InfraTemplateConfigMapName string
}

// +kubebuilder:rbac:groups=internal.nvke.nvidia.com,resources=nvkeclusters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=internal.nvke.nvidia.com,resources=nvkeclusters/status,verbs=update;patch
// +kubebuilder:rbac:groups=internal.nvke.nvidia.com,resources=controlplaneclasses;clusterreleases;clusterreleasechannels;machineimages,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

func (r *NVKEClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("reconciling NVKECluster")

	var cluster internalv1.NVKECluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.NVKEController != nil {
		// Persist cleanup responsibility before making any provisioning request.
		added, err := finalizers.EnsureFinalizer(ctx, r.Client, &cluster, nvkeClusterFinalizer)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure nvkecluster finalizer: %w", err)
		}
		if added {
			return ctrl.Result{}, nil
		}
	}

	patchHelper, err := patch.NewHelper(&cluster, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("create nvkecluster patch helper: %w", err)
	}
	// Persist status on every return after this point.
	defer func() {
		if conditionErr := setNVKEClusterConditions(&cluster); conditionErr != nil {
			// If we have a terminal error but the set failed remove the terminal condition so we requeue
			for errors.Is(retErr, reconcile.TerminalError(nil)) {
				retErr = errors.Unwrap(retErr)
			}
			retErr = errors.Join(retErr, conditionErr)
		}

		patchOpts := []patch.Option{patch.WithOwnedConditions{Conditions: nvkeClusterOwnedConditions}}
		if retErr == nil && r.NVKEController != nil {
			// Persist the checkpoint with the remaining status
			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})
		}
		if patchErr := patchHelper.Patch(ctx, &cluster, patchOpts...); client.IgnoreNotFound(patchErr) != nil {
			// If we have a terminal error but the set failed remove the terminal condition so we requeue
			for errors.Is(retErr, reconcile.TerminalError(nil)) {
				retErr = errors.Unwrap(retErr)
			}
			retErr = errors.Join(retErr, fmt.Errorf("patch nvkecluster: %w", patchErr))
		}
	}()

	if r.NVKEController == nil {
		propagateNVKEControllerConditions(&cluster, nil)
		return ctrl.Result{}, nil
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &cluster)
	}
	return r.reconcileCluster(ctx, &cluster)
}

func (r *NVKEClusterReconciler) reconcileCluster(ctx context.Context, cluster *internalv1.NVKECluster) (ctrl.Result, error) {
	var infraTemplate corev1.ConfigMap
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: r.InfraTemplateConfigMapName}
	if err := r.Get(ctx, key, &infraTemplate); err != nil {
		err = fmt.Errorf("get infrastructure template ConfigMap %s: %w", key, err)
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	clusterTemplate := infraTemplate.Data[clusterTemplateKey]
	if strings.TrimSpace(clusterTemplate) == "" {
		err := fmt.Errorf("infrastructure template ConfigMap %s must contain non-empty data.%s", key, clusterTemplateKey)
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}
	nodePoolTemplate := infraTemplate.Data[nodePoolTemplateKey]
	if strings.TrimSpace(nodePoolTemplate) == "" {
		err := fmt.Errorf("infrastructure template ConfigMap %s must contain non-empty data.%s", key, nodePoolTemplateKey)
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	controlPlaneTemplate := infraTemplate.Data[controlPlaneTemplateKey]
	if strings.TrimSpace(controlPlaneTemplate) == "" {
		err := fmt.Errorf("infrastructure template ConfigMap %s must contain non-empty data.%s", key, controlPlaneTemplateKey)
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	variables, err := r.resolveReleaseTemplateVariables(ctx, cluster)
	if err != nil {
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}
	var clusterInfra api.InfraClusterProvider
	err = renderInfraTemplate(clusterTemplateKey, clusterTemplate, infraTemplateVariables(cluster, nil, variables), &clusterInfra)
	if err == nil && len(clusterInfra.Cluster) == 0 {
		err = fmt.Errorf("%s must contain a cluster manifest", clusterTemplateKey)
	}
	if err != nil {
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	observedCluster, err := r.ensureCluster(ctx, cluster, &clusterInfra, controlPlaneTemplate, variables)
	propagateNVKEControllerConditions(cluster, observedCluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure nvke controller cluster: %w", err)
	}
	var poolErr error
	desiredPools := make(map[internalv1.ResourceName]bool, len(cluster.Spec.NodePools))
	for i := range cluster.Spec.NodePools {
		pool := &cluster.Spec.NodePools[i]
		desiredPools[pool.Name] = true
		if err := r.reconcileNodePool(ctx, cluster, pool, nodePoolTemplate, observedCluster, variables); err != nil {
			poolErr = errors.Join(poolErr, fmt.Errorf("reconcile node pool %q: %w", pool.Name, err))
		}
	}
	remainingPools := make([]internalv1.NodePoolStatus, 0, len(cluster.Status.NodePools))
	for _, status := range cluster.Status.NodePools {
		if desiredPools[status.Name] {
			remainingPools = append(remainingPools, status)
			continue
		}
		deleted, err := r.reconcileNodePoolDelete(ctx, cluster, &status, observedCluster)
		if err != nil {
			poolErr = errors.Join(poolErr, fmt.Errorf("remove node pool %q: %w", status.Name, err))
		}
		if err != nil || !deleted {
			remainingPools = append(remainingPools, status)
		}
	}
	cluster.Status.NodePools = remainingPools
	if poolErr != nil {
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: poolErr.Error(),
		})
		return ctrl.Result{}, poolErr
	}
	conditions.Set(cluster, metav1.Condition{
		Type:   internalv1.ClusterReconciledCondition,
		Status: metav1.ConditionTrue,
		Reason: internalv1.ReconcileSucceededReason,
	})
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

func (r *NVKEClusterReconciler) resolveReleaseTemplateVariables(ctx context.Context, cluster *internalv1.NVKECluster) (map[string]any, error) {
	release, err := r.resolveClusterRelease(ctx, cluster)
	if err != nil {
		return nil, err
	}
	var image internalv1.MachineImage
	imageName := string(release.Spec.MachineImageRef.Name)
	if err := r.Get(ctx, client.ObjectKey{Name: imageName}, &image); err != nil {
		return nil, fmt.Errorf("get MachineImage %q for ClusterRelease %q: %w", imageName, release.Name, err)
	}
	kubernetesVersion, err := clusterReleaseKubernetesVersion(release)
	if err != nil {
		return nil, err
	}
	files := []internalv1.BootstrapFile{}
	commands := []string{}
	if bootstrap := release.Spec.MachineInit.Infra; bootstrap != nil {
		files = append(files, bootstrap.BootstrapFiles...)
		commands = append(commands, bootstrap.PreKubeadmCommands...)
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return nil, fmt.Errorf("marshal release bootstrap files: %w", err)
	}
	commandsJSON, err := json.Marshal(commands)
	if err != nil {
		return nil, fmt.Errorf("marshal release pre-kubeadm commands: %w", err)
	}
	// JSON encoding preserves the script's newlines and quotes inside the infra template.
	ipxeScript, err := json.Marshal(image.Spec.NICo.IPXEScript)
	if err != nil {
		return nil, fmt.Errorf("marshal image iPXE script: %w", err)
	}
	cluster.Status.ResolvedRelease = &internalv1.ReleaseReference{
		Name: internalv1.ResourceName(release.Name), Version: kubernetesVersion.String(),
	}
	return map[string]any{
		"BootstrapFilesJSON":     string(filesJSON),
		"PreKubeadmCommandsJSON": string(commandsJSON),
		"IPXEScript":             string(ipxeScript),
	}, nil
}

func (r *NVKEClusterReconciler) resolveClusterRelease(ctx context.Context, cluster *internalv1.NVKECluster) (*internalv1.ClusterRelease, error) {
	selection := cluster.Spec.Release.Channel
	var channel internalv1.ClusterReleaseChannel
	if err := r.Get(ctx, client.ObjectKey{Name: string(selection.Name)}, &channel); err != nil {
		return nil, fmt.Errorf("get ClusterReleaseChannel %q: %w", selection.Name, err)
	}
	if pinned := cluster.Status.ResolvedRelease; pinned != nil {
		pinnedVersion, err := version.ParseSemantic(pinned.Version)
		if err != nil {
			return nil, fmt.Errorf("parse pinned release version %q: %w", pinned.Version, err)
		}
		pinnedMinorVersion := fmt.Sprintf("%d.%d", pinnedVersion.Major(), pinnedVersion.Minor())
		inChannel := false
		for _, candidate := range channel.Spec.Releases[pinnedMinorVersion] {
			if candidate.Name == pinned.Name {
				inChannel = true
				break
			}
		}
		if inChannel && (selection.Version == "" || selection.Version == pinnedMinorVersion) {
			var release internalv1.ClusterRelease
			if err := r.Get(ctx, client.ObjectKey{Name: string(pinned.Name)}, &release); err != nil {
				return nil, fmt.Errorf("get pinned ClusterRelease %q: %w", pinned.Name, err)
			}
			return &release, nil
		}
	}
	channelVersion := selection.Version
	if channelVersion == "" {
		var latest *version.Version
		for key := range channel.Spec.Releases {
			parsed, err := version.ParseGeneric(key)
			if err != nil {
				return nil, fmt.Errorf("parse channel %q version %q: %w", channel.Name, key, err)
			}
			if latest == nil || parsed.GreaterThan(latest) {
				latest, channelVersion = parsed, key
			}
		}
	}
	candidates := channel.Spec.Releases[channelVersion]
	if len(candidates) == 0 {
		return nil, fmt.Errorf("channel %q has no releases for version %q", channel.Name, channelVersion)
	}

	var release internalv1.ClusterRelease
	name := candidates[0].Name
	if err := r.Get(ctx, client.ObjectKey{Name: string(name)}, &release); err != nil {
		return nil, fmt.Errorf("get ClusterRelease %q from channel %q: %w", name, channel.Name, err)
	}
	return &release, nil
}

func clusterReleaseKubernetesVersion(release *internalv1.ClusterRelease) (*version.Version, error) {
	raw := release.Annotations[internalv1.MachineInitKubernetesVersionAnnotation]
	parsed, err := version.ParseSemantic(raw)
	if err != nil {
		return nil, fmt.Errorf("ClusterRelease %q must declare an exact Kubernetes version in annotation %s: %w", release.Name, internalv1.MachineInitKubernetesVersionAnnotation, err)
	}
	return parsed, nil
}

func renderInfraTemplate(name, body string, variables map[string]any, target any) error {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(body)
	if err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, variables); err != nil {
		return fmt.Errorf("interpolate %s: %w", name, err)
	}
	if err := json.Unmarshal(rendered.Bytes(), target); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return nil
}

func infraTemplateVariables(cluster *internalv1.NVKECluster, pool *internalv1.NodePool, releaseVariables map[string]any) map[string]any {
	variables := maps.Clone(releaseVariables)
	if cluster.Spec.Network != nil && cluster.Spec.Network.VPCID != "" {
		variables["VPCID"] = cluster.Spec.Network.VPCID
	}
	if pool == nil {
		return variables
	}
	if pool.InstanceType != "" {
		variables["InstanceType"] = string(pool.InstanceType)
	}
	if pool.VPCSubnetID != "" {
		variables["VPCSubnetID"] = pool.VPCSubnetID
	}
	return variables
}

func (r *NVKEClusterReconciler) reconcileNodePool(ctx context.Context, cluster *internalv1.NVKECluster, pool *internalv1.NodePool, nodePoolTemplate string, observedCluster *api.Cluster, variables map[string]any) error {
	var status *internalv1.NodePoolStatus
	for i := range cluster.Status.NodePools {
		if cluster.Status.NodePools[i].Name == pool.Name {
			status = &cluster.Status.NodePools[i]
			break
		}
	}

	if status == nil {
		cluster.Status.NodePools = append(cluster.Status.NodePools, internalv1.NodePoolStatus{Name: pool.Name})
		status = &cluster.Status.NodePools[len(cluster.Status.NodePools)-1]
	}
	target := nodePoolConditionTarget{Object: cluster, Setter: status}

	observedPool, err := r.NVKEController.FindNodePool(ctx, observedCluster.Name, string(pool.Name))
	if errors.Is(err, nvkecontroller.ErrNotFound) {
		request, err := resolveNodePoolRequest(observedCluster.ID, pool)
		if err != nil {
			return fmt.Errorf("resolve node pool %q request: %w", pool.Name, err)
		}
		var infra api.InfraNodePoolProvider
		if err := renderInfraTemplate(nodePoolTemplateKey, nodePoolTemplate, infraTemplateVariables(cluster, pool, variables), &infra); err != nil {
			return err
		}
		if len(infra.MachineTemplate) == 0 {
			return fmt.Errorf("%s must contain a machineTemplate manifest", nodePoolTemplateKey)
		}
		request.Infra = &infra
		observedPool, err := r.NVKEController.CreateNodePool(ctx, request)
		if err != nil {
			return fmt.Errorf("create nvke controller node pool %q: %w", pool.Name, err)
		}
		status.ID = observedPool.ID.String()
		propagateNodePoolConditions(target, observedPool)
		return setNodePoolConditions(target)
	}

	if err != nil {
		return fmt.Errorf("find nvke controller node pool %q: %w", pool.Name, err)
	}
	if observedPool.ClusterID != observedCluster.ID {
		return fmt.Errorf("node pool %q does not belong to cluster %q", pool.Name, observedCluster.Name)
	}
	status.ID = observedPool.ID.String()
	if observedPool.Conditions.Deleting.Status == api.ConditionStatus("True") {
		propagateNodePoolConditions(target, observedPool)
		return setNodePoolConditions(target)
	}
	if pool.Replicas == nil || *pool.Replicas < 0 {
		return invalidNVKEControllerRequest("Node pool replicas must be a nonnegative count")
	}
	desiredCount := int(*pool.Replicas)
	if observedPool.DesiredNodeCount != desiredCount {
		updatedPool, err := r.NVKEController.UpdateNodePool(ctx, observedPool.ID.String(), api.UpdateNodePoolRequest{DesiredNodeCount: &desiredCount})
		if err != nil {
			return fmt.Errorf("scale nvke controller node pool %q to %d replicas: %w", pool.Name, desiredCount, err)
		}
		observedPool = updatedPool
	}
	propagateNodePoolConditions(target, observedPool)
	return setNodePoolConditions(target)
}

func (r *NVKEClusterReconciler) reconcileNodePoolDelete(ctx context.Context, cluster *internalv1.NVKECluster, pool *internalv1.NodePoolStatus, observedCluster *api.Cluster) (bool, error) {
	observedPool, err := r.NVKEController.FindNodePool(ctx, observedCluster.Name, string(pool.Name))
	if errors.Is(err, nvkecontroller.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("find nvke controller node pool %q for deletion: %w", pool.Name, err)
	}
	if observedPool.ClusterID != observedCluster.ID {
		return false, fmt.Errorf("node pool %q does not belong to cluster %q", pool.Name, observedCluster.Name)
	}
	pool.ID = observedPool.ID.String()
	if observedPool.Conditions.Deleting.Status != api.ConditionStatus("True") {
		if err := r.NVKEController.DeleteNodePool(ctx, observedPool.ID.String()); err != nil {
			return false, fmt.Errorf("delete nvke controller node pool %q: %w", pool.Name, err)
		}
	}
	target := nodePoolConditionTarget{Object: cluster, Setter: pool}
	propagateNodePoolConditions(target, observedPool)
	conditions.Set(target, metav1.Condition{
		Type:    internalv1.NVKEControllerDeletingCondition,
		Status:  metav1.ConditionTrue,
		Reason:  clusterv1.DeletingReason,
		Message: "Node pool removal is in progress",
	})
	return false, setNodePoolConditions(target)
}

func (r *NVKEClusterReconciler) ensureCluster(ctx context.Context, cluster *internalv1.NVKECluster, infra *api.InfraClusterProvider, controlPlaneTemplate string, variables map[string]any) (*api.Cluster, error) {
	name := ClusterRemoteName(cluster)
	observedCluster, err := r.NVKEController.FindByName(ctx, name)
	if err == nil {
		cluster.Status.ID = observedCluster.ID.String()
		return observedCluster, nil
	}
	if !errors.Is(err, nvkecontroller.ErrNotFound) {
		return nil, fmt.Errorf("find nvke controller cluster %q: %w", name, err)
	}

	request, err := r.resolveNVKEControllerRequest(ctx, cluster, controlPlaneTemplate, variables)
	if err != nil {
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: err.Error(),
		})
		if _, ok := errors.AsType[*InvalidSpecError](err); ok {
			err = reconcile.TerminalError(err)
		}
		return nil, fmt.Errorf("resolve controller request: %w", err)
	}
	request.Infra = infra
	observedCluster, err = r.NVKEController.Create(ctx, request)
	if err != nil {
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: err.Error(),
		})
		return nil, fmt.Errorf("create cluster: %w", err)
	}
	cluster.Status.ID = observedCluster.ID.String()
	return observedCluster, nil
}

// ClusterRemoteName returns the backing cluster name used by the management engine.
func ClusterRemoteName(cluster *internalv1.NVKECluster) string {
	hash := sha256.Sum256([]byte(cluster.Namespace + "/" + cluster.Name))
	suffix := fmt.Sprintf("-%x%s", hash[:4], clusterNameSuffix)
	name := strings.ReplaceAll(cluster.Name, ".", "-")
	name = name[:min(len(name), clusterNameMaxLength-len(suffix))]
	return strings.TrimRight(name, "-") + suffix
}

func (r *NVKEClusterReconciler) reconcileDelete(ctx context.Context, cluster *internalv1.NVKECluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, nvkeClusterFinalizer) {
		return ctrl.Result{}, nil
	}

	name := ClusterRemoteName(cluster)
	observedCluster, err := r.NVKEController.FindByName(ctx, name)
	propagateNVKEControllerConditions(cluster, observedCluster)
	if errors.Is(err, nvkecontroller.ErrNotFound) {
		controllerutil.RemoveFinalizer(cluster, nvkeClusterFinalizer)
		return ctrl.Result{}, nil
	}

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("find nvke controller cluster %q for deletion: %w", name, err)
	}
	cluster.Status.ID = observedCluster.ID.String()

	if observedCluster.Conditions.Deleting.Status != api.ConditionStatus("True") {
		if err := r.NVKEController.Delete(ctx, observedCluster.ID.String()); err != nil && !errors.Is(err, nvkecontroller.ErrNotFound) {
			return ctrl.Result{}, fmt.Errorf("delete nvke controller cluster %q: %w", name, err)
		}
	}
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

// Cluster intent resolution combines the spec with selected configuration for reconciliation.
// NVKE controller REST API specific calls will be replaced with the CAPI objects, but this is a temporary placeholder.

// InvalidSpecError identifies a request that needs corrected configuration.
type InvalidSpecError struct{ Message string }

func (e *InvalidSpecError) Error() string { return e.Message }

func (r *NVKEClusterReconciler) resolveNVKEControllerRequest(ctx context.Context, cluster *internalv1.NVKECluster, controlPlaneTemplate string, variables map[string]any) (api.CreateClusterRequest, error) {
	var empty api.CreateClusterRequest
	if err := validateNVKEControllerSpec(cluster); err != nil {
		return empty, fmt.Errorf("validate cluster spec: %w", err)
	}

	var request api.CreateClusterRequest
	request.Name = ClusterRemoteName(cluster)
	if cluster.UID == "" {
		return empty, invalidNVKEControllerRequest("A persisted cluster UID is required before provisioning")
	}

	request.CniPlugin = api.CreateClusterRequestCniPluginOvnKubernetes
	request.EtcdEncrypted = true
	pod, err := nvkeControllerNetworkPrefix(cluster.Spec.ClusterNetwork.Pods.CIDRBlocks)
	if err != nil {
		return empty, fmt.Errorf("resolve pod network: %w", err)
	}

	service, err := nvkeControllerNetworkPrefix(cluster.Spec.ClusterNetwork.Services.CIDRBlocks)
	if err != nil {
		return empty, fmt.Errorf("resolve service network: %w", err)
	}

	request.Network = &api.ClusterNetworkCreateConfig{PodCIDR: pod, ServiceCIDR: service}
	if err := r.resolveNVKEControllerClass(ctx, cluster, controlPlaneTemplate, variables, &request); err != nil {
		return empty, fmt.Errorf("resolve control-plane class: %w", err)
	}
	return request, nil
}

func resolveNodePoolRequest(clusterID uuid.UUID, pool *internalv1.NodePool) (api.CreateNodePoolRequest, error) {
	var request api.CreateNodePoolRequest
	if clusterID == uuid.Nil {
		return request, invalidNVKEControllerRequest("A resolved cluster ID is required before creating node pools")
	}
	if pool.Replicas == nil || *pool.Replicas < 0 {
		return request, invalidNVKEControllerRequest("Node pool replicas must be a nonnegative count")
	}
	request = api.CreateNodePoolRequest{
		ClusterID:                   clusterID,
		Name:                        string(pool.Name),
		DesiredNodeCount:            int(*pool.Replicas),
		NodeNamePrefix:              string(pool.Name),
		ProvisioningMonitorDisabled: new(true),
		SystemReservedMemory:        "1Gi",
	}
	if params := pool.UserParams; params != nil {
		request.NodeLabels = make(map[string]string, len(params.Labels))
		for key, value := range params.Labels {
			request.NodeLabels[key] = string(value)
		}
		request.NodeTaints = make([]api.NodeTaint, 0, len(params.Taints))
		for _, taint := range params.Taints {
			request.NodeTaints = append(request.NodeTaints, api.NodeTaint{
				Key:    string(taint.Key),
				Effect: api.NodeTaintEffect(taint.Effect),
				Value:  string(taint.Value),
			})
		}
	}
	return request, nil
}

func invalidNVKEControllerRequest(message string) error { return &InvalidSpecError{Message: message} }

func validateNVKEControllerSpec(cluster *internalv1.NVKECluster) error {
	if cluster.Spec.Authentication != nil {
		return invalidNVKEControllerRequest("Custom authentication is not supported")
	}
	if cluster.Spec.APIServerAccess.PublicAccessEnabled || len(cluster.Spec.APIServerAccess.PublicAccessCIDRs) != 0 {
		return invalidNVKEControllerRequest("Public API access configuration is not supported")
	}
	if runtime := cluster.Spec.AcceleratedRuntime; runtime != nil && (runtime.Name != "none" || runtime.Version != "") {
		return invalidNVKEControllerRequest("Accelerated runtime configuration is not supported")
	}
	if cluster.Spec.ClusterNetwork.ServiceDomain != "cluster.local" {
		return invalidNVKEControllerRequest("Only the cluster.local service domain is supported")
	}
	return nil
}

func nvkeControllerNetworkPrefix(blocks []internalv1.NetworkCIDR) (string, error) {
	if len(blocks) != 1 {
		return "", invalidNVKEControllerRequest("Exactly one IPv4 pod range and one IPv4 service range are required")
	}
	prefix, err := netip.ParsePrefix(string(blocks[0]))
	if err != nil || !prefix.Addr().Is4() {
		return "", invalidNVKEControllerRequest("Pod and service ranges must be valid IPv4 CIDRs")
	}
	// The NVKE controller remains authoritative for private ranges, overlap, size, and capacity validation.
	return prefix.Masked().String(), nil
}

// Status mapping presents provisioning observations and failures as public cluster conditions.

var nvkeClusterOwnedConditions = []string{
	clusterv1.AvailableCondition,
	internalv1.SyncedCondition,
	internalv1.ReadyCondition,
	internalv1.ControlPlaneReadyCondition,
	clusterv1.DeletingCondition,
	internalv1.NVKEControllerProvisionedCondition,
	internalv1.NVKEControllerProvisioningCondition,
	internalv1.NVKEControllerDeletingCondition,
	internalv1.NVKEControllerControlPlaneUpgradingCondition,
	internalv1.NVKEControllerCPNodesRecyclingCondition,
	internalv1.NVKEControllerUpdateFailedCondition,
	internalv1.ClusterReconciledCondition,
}

var nvkeControllerNegativeConditionTypes = []string{
	internalv1.NVKEControllerProvisioningCondition,
	internalv1.NVKEControllerDeletingCondition,
	internalv1.NVKEControllerControlPlaneUpgradingCondition,
	internalv1.NVKEControllerCPNodesRecyclingCondition,
	internalv1.NVKEControllerUpdateFailedCondition,
}

func propagateNVKEControllerConditions(cluster *internalv1.NVKECluster, observedCluster *api.Cluster) {
	if observedCluster == nil {
		observedCluster = &api.Cluster{}
	}
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerProvisionedCondition, &observedCluster.Conditions.Provisioned)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerProvisioningCondition, &observedCluster.Conditions.Provisioning)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerDeletingCondition, &observedCluster.Conditions.Deleting)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerControlPlaneUpgradingCondition, observedCluster.Conditions.ControlPlaneUpgrading)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerCPNodesRecyclingCondition, observedCluster.Conditions.CpNodesRecycling)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerUpdateFailedCondition, observedCluster.Conditions.UpdateFailed)
}

// Helper type so capi aggregation setters can work
type nodePoolConditionTarget struct {
	metav1.Object
	conditions.Setter
}

func propagateNodePoolConditions(pool conditions.Setter, observedPool *api.NodePool) {
	setNVKEControllerCondition(pool, internalv1.NVKEControllerProvisionedCondition, &observedPool.Conditions.Provisioned)
	setNVKEControllerCondition(pool, internalv1.NVKEControllerProvisioningCondition, &observedPool.Conditions.Provisioning)
	setNVKEControllerCondition(pool, internalv1.NVKEControllerDeletingCondition, &observedPool.Conditions.Deleting)
}

func setNVKEControllerCondition(target conditions.Setter, conditionType string, child *api.Condition) {
	if child == nil {
		conditions.Set(target, metav1.Condition{
			Type:   conditionType,
			Status: metav1.ConditionUnknown,
			Reason: internalv1.NotYetReportedReason,
		})
		return
	}
	status := metav1.ConditionStatus(child.Status)
	reason := internalv1.ReportedReason
	switch status {
	case metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown:
	default:
		status = metav1.ConditionUnknown
		reason = internalv1.NotYetReportedReason
	}
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: metav1.NewTime(child.LastTransitionTime),
		Reason:             reason,
		Message:            child.Message,
	}
	// Suppress healthy source messages so CAPI summaries only report problems.
	priority := conditions.GetDefaultMergePriorityFunc(nvkeControllerNegativeConditionTypes...)
	if priority(condition) == conditions.InfoMergePriority {
		condition.Message = ""
	}
	conditions.Set(target, condition)
}

func setNodePoolConditions(pool conditions.Setter) error {
	if err := conditions.SetSummaryCondition(
		pool,
		pool,
		internalv1.ReadyCondition,
		conditions.ForConditionTypes{
			internalv1.NVKEControllerProvisionedCondition,
			internalv1.NVKEControllerProvisioningCondition,
			internalv1.NVKEControllerDeletingCondition,
		},
		conditions.NegativePolarityConditionTypes(nvkeControllerNegativeConditionTypes),
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.GetPriorityFunc(conditions.GetDefaultMergePriorityFunc(nvkeControllerNegativeConditionTypes...)),
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					clusterv1.NotReadyReason,
					clusterv1.ReadyUnknownReason,
					clusterv1.ReadyReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("summarize node pool Ready condition: %w", err)
	}
	return nil
}

func setNVKEClusterConditions(cluster *internalv1.NVKECluster) error {
	if cluster.DeletionTimestamp.IsZero() {
		conditions.Set(cluster, metav1.Condition{
			Type:   clusterv1.DeletingCondition,
			Status: metav1.ConditionFalse,
			Reason: clusterv1.NotDeletingReason,
		})
	} else {
		conditions.Set(cluster, metav1.Condition{
			Type:    clusterv1.DeletingCondition,
			Status:  metav1.ConditionTrue,
			Reason:  clusterv1.DeletingReason,
			Message: "Cluster deletion is in progress",
		})
	}

	if err := conditions.SetSummaryCondition(
		cluster,
		cluster,
		internalv1.ControlPlaneReadyCondition,
		conditions.ForConditionTypes{
			internalv1.NVKEControllerProvisionedCondition,
			internalv1.NVKEControllerProvisioningCondition,
			internalv1.NVKEControllerDeletingCondition,
		},
		conditions.NegativePolarityConditionTypes(nvkeControllerNegativeConditionTypes),
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.GetPriorityFunc(conditions.GetDefaultMergePriorityFunc(nvkeControllerNegativeConditionTypes...)),
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					clusterv1.NotReadyReason,
					clusterv1.ReadyUnknownReason,
					clusterv1.ReadyReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster ControlPlaneReady condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(
		cluster,
		cluster,
		internalv1.SyncedCondition,
		conditions.ForConditionTypes{internalv1.ClusterReconciledCondition, internalv1.ControlPlaneReadyCondition},
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					internalv1.NotSyncedReason,
					internalv1.SyncUnknownReason,
					internalv1.SyncedReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster Synced condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(
		cluster,
		cluster,
		clusterv1.AvailableCondition,
		conditions.ForConditionTypes{internalv1.ControlPlaneReadyCondition},
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					clusterv1.NotAvailableReason,
					clusterv1.AvailableUnknownReason,
					clusterv1.AvailableReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster Available condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(
		cluster,
		cluster,
		internalv1.ReadyCondition,
		conditions.ForConditionTypes{
			clusterv1.AvailableCondition,
			internalv1.SyncedCondition,
			clusterv1.DeletingCondition,
		},
		conditions.NegativePolarityConditionTypes{clusterv1.DeletingCondition},
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.GetPriorityFunc(conditions.GetDefaultMergePriorityFunc(clusterv1.DeletingCondition)),
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					clusterv1.NotReadyReason,
					clusterv1.ReadyUnknownReason,
					clusterv1.ReadyReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster Ready condition: %w", err)
	}
	return nil
}

func (r *NVKEClusterReconciler) resolveNVKEControllerClass(ctx context.Context, cluster *internalv1.NVKECluster, controlPlaneTemplate string, variables map[string]any, request *api.CreateClusterRequest) error {
	name := cluster.Spec.ControlPlaneClass
	var class internalv1.ControlPlaneClass
	if err := r.Get(ctx, client.ObjectKey{Name: string(name)}, &class); err != nil {
		return fmt.Errorf("get ControlPlaneClass %q: %w", name, err)
	}
	config := class.Spec.ControlPlane
	if config.Replicas == nil || *config.Replicas < 1 {
		return invalidNVKEControllerRequest(fmt.Sprintf("ControlPlaneClass %q must specify at least one replica", name))
	}
	controlPlaneVariables := infraTemplateVariables(cluster, nil, variables)
	if len(config.InstanceTypePreferences) > 0 {
		controlPlaneVariables["InstanceType"] = config.InstanceTypePreferences[0]
	}
	var infra api.InfraControlPlaneProvider
	if err := renderInfraTemplate(controlPlaneTemplateKey, controlPlaneTemplate, controlPlaneVariables, &infra); err != nil {
		return err
	}
	if len(infra.MachineTemplate) == 0 {
		return fmt.Errorf("%s must contain a machineTemplate manifest", controlPlaneTemplateKey)
	}
	request.ControlPlaneKubernetesVersion = cluster.Status.ResolvedRelease.Version
	request.ControlPlaneConfig = &api.CreateClusterControlPlaneConfig{
		DesiredCount:                int(*config.Replicas),
		ProvisioningMonitorDisabled: new(false),
		Infra:                       &infra,
		MaxSurge:                    new(int(class.Spec.Rollout.Strategy.RollingUpdate.MaxSurge)),
		SystemReservedMemory:        config.SystemReservedMemory,
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NVKEClusterReconciler) SetupWithManager(mgr ctrl.Manager, wrappers ...func(reconcile.Reconciler) reconcile.Reconciler) error {
	if r.PollInterval <= 0 {
		r.PollInterval = defaultPollInterval
	}
	var reconciler reconcile.Reconciler = r
	for _, wrap := range wrappers {
		reconciler = wrap(reconciler)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&internalv1.NVKECluster{}).
		Named("nvkecluster").
		Complete(reconciler)
}
