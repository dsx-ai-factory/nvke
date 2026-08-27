// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiexternal "sigs.k8s.io/cluster-api/controllers/external"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/constants"
)

type legacyCAPIObjects struct {
	cluster       *clusterv1.Cluster
	infraCluster  *unstructured.Unstructured
	machines      []clusterv1.Machine
	infraMachines []*unstructured.Unstructured
	secrets       []corev1.Secret
}

func (o *legacyCAPIObjects) containsInfraMachine(name string) bool {
	for _, infraMachine := range o.infraMachines {
		if infraMachine.GetName() == name {
			return true
		}
	}
	return false
}

func migrateLegacyCAPINamespaceObjects(ctx context.Context, k8sClient ctrlclient.Client, clusterName string) (bool, error) {
	legacyNamespace := capi.LegacyClusterNamespace(clusterName)

	legacy, err := getLegacyCAPINamespaceObjects(ctx, k8sClient, legacyNamespace, clusterName)
	if err != nil {
		return false, fmt.Errorf("load legacy CAPI objects: %w", err)
	}

	if legacy == nil {
		// Namespace is gone, or already Terminating (delete in flight).
		return false, nil
	}

	// Critical logic:
	// Move CAPI objects from cluster-<name> into nke-system without CAPNICo/CAPI
	// delete reconciling, which would destroy live NICo instances.
	//
	//  1. Pause the source (capi pause annotation, and Cluster.spec.paused).
	//  2. SSA copied CAPI objects into nke-system.
	//     - Applied CAPI objects are still paused.
	//     - Terminating objects are not copied (do not resurrect).
	//     - OwnerRefs and status are stripped (CAPI rewrites them after unpause).
	//  3. Delete the source:
	//     - Delete while paused.
	//     - Strip finalizers so the CRs actually delete.
	//     - Delete the namespace.
	//  4. Return
	//     - Unpause/adoption runs on a later pass once legacy is nil.
	if err := pauseLegacyCAPINamespaceObjects(ctx, k8sClient, legacy); err != nil {
		return false, fmt.Errorf("pause legacy CAPI objects: %w", err)
	}
	if err := copyLegacyCAPINamespaceObjects(ctx, k8sClient, legacy); err != nil {
		return false, fmt.Errorf("copy legacy CAPI objects: %w", err)
	}
	if err := deleteLegacyCAPINamespaceObjects(ctx, k8sClient, legacyNamespace, legacy); err != nil {
		return false, fmt.Errorf("delete legacy CAPI objects: %w", err)
	}

	// Migration occurred and was successful.
	return true, nil
}

func getLegacyCAPINamespaceObjects(ctx context.Context, k8sClient ctrlclient.Client, namespace, clusterName string) (*legacyCAPIObjects, error) {
	namespaceObj := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, ctrlclient.ObjectKey{Name: namespace}, namespaceObj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil //nolint:nilnil // no legacy namespace means this cluster is already in nke-system
		}
		return nil, fmt.Errorf("get legacy Namespace: %w", err)
	}
	if !namespaceObj.DeletionTimestamp.IsZero() {
		// Delete already in flight. Pause/copy cannot do more
		// (copy skips terminating objects). Waiting for NotFound
		// would leave nke-system paused if the namespace sticks.
		return nil, nil //nolint:nilnil // terminating namespace is not live source material
	}

	legacy := &legacyCAPIObjects{}

	cluster := &clusterv1.Cluster{}
	if err := k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: clusterName}, cluster); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get legacy Cluster: %w", err)
		}
		cluster = nil
	}
	legacy.cluster = cluster

	var infraCluster *unstructured.Unstructured
	if legacy.cluster != nil && cluster.Spec.InfrastructureRef.IsDefined() {
		var err error
		infraCluster, err = capiexternal.GetObjectFromContractVersionedRef(ctx, k8sClient, cluster.Spec.InfrastructureRef, namespace)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("get legacy infrastructure cluster: %w", err)
			}
			infraCluster = nil
		}
	}
	legacy.infraCluster = infraCluster

	machineList := &clusterv1.MachineList{}
	if err := k8sClient.List(ctx, machineList, ctrlclient.InNamespace(namespace), ctrlclient.MatchingLabels{clusterv1.ClusterNameLabel: clusterName}); err != nil {
		return nil, fmt.Errorf("list legacy Machines: %w", err)
	}

	legacy.machines = machineList.Items
	for _, machine := range machineList.Items {
		ref := machine.Spec.InfrastructureRef
		if !ref.IsDefined() {
			continue
		}

		infraMachine, err := capiexternal.GetObjectFromContractVersionedRef(ctx, k8sClient, ref, namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get legacy infrastructure machine %s: %w", machine.Name, err)
		}
		legacy.infraMachines = append(legacy.infraMachines, infraMachine)
	}

	// InfrastructureRef only finds NicoMachines still pointed at by a Machine.
	// Also list by cluster-name label so leftovers in this namespace (empty
	// ref, or Machine already gone) are included in the set we return.
	nicoMachines := &nicov1.NicoMachineList{}
	if err := k8sClient.List(ctx, nicoMachines, ctrlclient.InNamespace(namespace), ctrlclient.MatchingLabels{clusterv1.ClusterNameLabel: clusterName}); err != nil {
		return nil, fmt.Errorf("list legacy NicoMachines: %w", err)
	}
	for _, nicoMachine := range nicoMachines.Items {
		if legacy.containsInfraMachine(nicoMachine.Name) {
			continue // already accounted for by InfrastructureRef
		}

		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&nicoMachine)
		if err != nil {
			return nil, fmt.Errorf("convert legacy NicoMachine %s: %w", nicoMachine.Name, err)
		}

		unstructuredNicoMachine := &unstructured.Unstructured{Object: obj}
		unstructuredNicoMachine.SetAPIVersion(nicov1.GroupVersion.String())
		unstructuredNicoMachine.SetKind("NicoMachine")
		legacy.infraMachines = append(legacy.infraMachines, unstructuredNicoMachine)
	}

	secretList := &corev1.SecretList{}
	if err := k8sClient.List(ctx, secretList, ctrlclient.InNamespace(namespace), ctrlclient.MatchingLabels{clusterv1.ClusterNameLabel: clusterName}); err != nil {
		return nil, fmt.Errorf("list legacy bootstrap Secrets: %w", err)
	}
	legacy.secrets = append(legacy.secrets, secretList.Items...)
	return legacy, nil
}

func pauseLegacyCAPINamespaceObjects(ctx context.Context, k8sClient ctrlclient.Client, legacy *legacyCAPIObjects) error {
	var all []ctrlclient.Object
	if legacy.cluster != nil {
		legacy.cluster.Spec.Paused = ptr.To(true) // Cluster objects also have pause in the spec.
		all = append(all, legacy.cluster)
	}
	if legacy.infraCluster != nil {
		all = append(all, legacy.infraCluster)
	}
	for i := range legacy.machines {
		all = append(all, &legacy.machines[i])
	}
	for _, obj := range legacy.infraMachines {
		all = append(all, obj)
	}
	for _, obj := range all {
		capi.SetPausedAnnotation(obj, true)
		if err := k8sClient.Update(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("pause legacy %T %s: %w", obj, obj.GetName(), err)
		}
	}
	// Notice legacy.secrets are not paused. Secrets have no capi pause contract.
	return nil
}

func copyLegacyCAPINamespaceObjects(ctx context.Context, k8sClient ctrlclient.Client, legacy *legacyCAPIObjects) error {
	if legacy == nil {
		return nil
	}

	// Referenced objects before referrers so Cluster/Machine validating webhooks
	// can Get infrastructureRef and bootstrap DataSecretName.
	//
	// OwnerRefs are stripped on apply (source UIDs must be updated as they change in nke-system)
	// CAPI rewrites them after unpause.
	//
	// Controllers are paused during copy, so order only matters for admission.
	if legacy.infraCluster != nil {
		if err := applyMigratedObject(ctx, k8sClient, legacy.infraCluster, true); err != nil {
			return err
		}
	}
	if legacy.cluster != nil {
		if err := applyMigratedObject(ctx, k8sClient, legacy.cluster, true); err != nil {
			return err
		}
	}

	for _, infraMachine := range legacy.infraMachines {
		if err := applyMigratedObject(ctx, k8sClient, infraMachine, true); err != nil {
			return err
		}
	}
	for _, secret := range legacy.secrets {
		// TenantAccess already publishes nke-system/<name>-kubeconfig. Do not
		// SSA the CAPI admin kubeconfig over it. Source delete still
		// removes the legacy copy from the collected set.
		if strings.HasSuffix(secret.Name, "-kubeconfig") {
			continue
		}
		if err := applyMigratedObject(ctx, k8sClient, &secret, false); err != nil {
			return err
		}
	}
	for _, machine := range legacy.machines {
		if err := applyMigratedObject(ctx, k8sClient, &machine, true); err != nil {
			return err
		}
	}
	return nil
}

func applyMigratedObject(ctx context.Context, k8sClient ctrlclient.Client, obj ctrlclient.Object, paused bool) error {
	// Do not resurrect a terminating source object in nke-system. Delete still
	// removes it from the legacy namespace.
	if ts := obj.GetDeletionTimestamp(); ts != nil && !ts.IsZero() {
		return nil
	}

	copied, ok := obj.DeepCopyObject().(ctrlclient.Object)
	if !ok {
		return fmt.Errorf("deep copy %T does not implement client.Object", obj)
	}
	obj = copied

	obj.SetNamespace(constants.NamespaceNKESystem)
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetGeneration(0)
	obj.SetCreationTimestamp(metav1.Time{})
	obj.SetDeletionTimestamp(nil)
	obj.SetDeletionGracePeriodSeconds(nil)
	obj.SetFinalizers(nil)
	obj.SetOwnerReferences(nil)
	obj.SetManagedFields(nil)

	if paused {
		capi.SetPausedAnnotation(obj, true)
	}

	// Status is a subresource. SSA on the main object cannot write it, and
	// source status (Ready, instanceID) must not ride along in the apply
	// body. Controllers refill status after unpause; NicoMachine imports
	// from spec.providerID if status.instanceID is empty.
	//
	// So clear/delete status before applying in the new namespace.
	switch typed := obj.(type) {
	case *clusterv1.Cluster:
		typed.Status = clusterv1.ClusterStatus{}
		if paused {
			typed.Spec.Paused = ptr.To(true)
		}
	case *clusterv1.Machine:
		typed.Status = clusterv1.MachineStatus{}
	case *unstructured.Unstructured:
		// NicoMachine/NicoCluster come in as unstructured.Unstructured.
		delete(typed.Object, "status")
	}

	// Get/List can leave TypeMeta empty on typed objects. client.Apply marshals
	// apiVersion/kind from the object; the apiserver rejects the apply without them.
	gvk, err := apiutil.GVKForObject(obj, k8sClient.Scheme())
	if err != nil {
		return fmt.Errorf("resolve GVK for migrated %T %s: %w", obj, obj.GetName(), err)
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)

	if err := k8sClient.Patch(ctx, obj, ctrlclient.Apply, ctrlclient.FieldOwner(constants.FieldManagerCWE), ctrlclient.ForceOwnership); err != nil {
		return fmt.Errorf("apply migrated %T %s: %w", obj, obj.GetName(), err)
	}
	return nil
}

func deleteLegacyCAPINamespaceObjects(ctx context.Context, k8sClient ctrlclient.Client, namespace string, legacy *legacyCAPIObjects) error {
	if legacy != nil {
		// The objects should already be paused, but pause again so delete
		// on a legacy object, especially Machine/NicoMachine, is never unpaused.
		if err := pauseLegacyCAPINamespaceObjects(ctx, k8sClient, legacy); err != nil {
			return err
		}

		for _, obj := range legacy.infraMachines {
			if err := forceDeletePausedObject(ctx, k8sClient, obj); err != nil {
				return err
			}
		}
		for i := range legacy.machines {
			if err := forceDeletePausedObject(ctx, k8sClient, &legacy.machines[i]); err != nil {
				return err
			}
		}

		if legacy.infraCluster != nil {
			if err := forceDeletePausedObject(ctx, k8sClient, legacy.infraCluster); err != nil {
				return err
			}
		}
		if legacy.cluster != nil {
			if err := forceDeletePausedObject(ctx, k8sClient, legacy.cluster); err != nil {
				return err
			}
		}
	}

	namespaceObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := k8sClient.Delete(ctx, namespaceObj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy namespace %s: %w", namespace, err)
	}

	return nil
}

// forceDeletePausedObject deletes a paused object.
// Normally paused objects get stuck deleting because they are waiting for finalizers to be removed.
// This function removes the finalizers so the object can be deleted.
// Do not use this function on non-paused objects.
func forceDeletePausedObject(ctx context.Context, k8sClient ctrlclient.Client, obj ctrlclient.Object) error {
	current, ok := obj.DeepCopyObject().(ctrlclient.Object)
	if !ok {
		return fmt.Errorf("deep copy %T does not implement client.Object", obj)
	}

	// Verify the object is paused one last time before deleting.
	if err := k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(obj), current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get legacy %T %s: %w", obj, obj.GetName(), err)
	}
	if _, paused := current.GetAnnotations()[clusterv1.PausedAnnotation]; !paused {
		return fmt.Errorf("refusing to force-delete unpaused %T %s", obj, obj.GetName())
	}

	if err := k8sClient.Delete(ctx, current); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy %T %s: %w", obj, obj.GetName(), err)
	}

	// Force delete e.g. remove finalizers.
	if err := k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(obj), current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get deleting legacy %T %s: %w", obj, obj.GetName(), err)
	}
	current.SetFinalizers(nil)
	if err := k8sClient.Update(ctx, current); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove finalizers from legacy %T %s: %w", obj, obj.GetName(), err)
	}

	return nil
}
