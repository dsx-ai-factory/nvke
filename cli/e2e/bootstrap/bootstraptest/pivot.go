// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// EnvPivotCluster names the cluster whose management plane the bootstrap
// pivots onto the site. When set, TestE2E_BootstrapSite checks the pivot with
// VerifyPivot once the bootstrap succeeds.
const EnvPivotCluster = "NVKECTL_E2E_PIVOT_CLUSTER"

// Where the NVKE stack keeps the cluster's Cluster API objects and runs the controller.
const (
	capiGroup           = "cluster.x-k8s.io"
	managementNamespace = "nvke-system"
	controllerNamespace = "nvke-controller"
	controllerName      = "nvke-controller"
)

// PivotCluster is one side of the pivot.
type PivotCluster struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
}

// VerifyPivot checks, once, that the site holds the Cluster API Cluster
// clusterName, the seed no longer does, and the seed's NVKE controller is
// scaled to zero. It does not wait: it is called after bootstrap site has
// reported the pivot complete. The error names every problem found.
func VerifyPivot(ctx context.Context, seed, site PivotCluster, clusterName string) error {
	gvr, err := capiClusterResource(site.Kube)
	if err != nil {
		return err
	}
	var problems []string
	onSite, err := hasCAPICluster(ctx, site.Dynamic, gvr, clusterName)
	if err != nil {
		return fmt.Errorf("site: %w", err)
	}
	if !onSite {
		problems = append(problems, fmt.Sprintf("the site does not hold the Cluster API Cluster %s/%s", managementNamespace, clusterName))
	}
	onSeed, err := hasCAPICluster(ctx, seed.Dynamic, gvr, clusterName)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if onSeed {
		problems = append(problems, fmt.Sprintf("the seed still holds the Cluster API Cluster %s/%s", managementNamespace, clusterName))
	}
	d, err := seed.Kube.AppsV1().Deployments(controllerNamespace).Get(ctx, controllerName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("seed: reading deployment %s/%s: %w", controllerNamespace, controllerName, err)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
		problems = append(problems, fmt.Sprintf("the seed controller %s/%s is not scaled to zero", controllerNamespace, controllerName))
	}
	if len(problems) > 0 {
		return fmt.Errorf("the pivot did not move the management plane to the site: %s", strings.Join(problems, "; "))
	}
	return nil
}

// capiClusterResource returns the Cluster API Cluster resource at the version
// the API server prefers.
func capiClusterResource(kube kubernetes.Interface) (schema.GroupVersionResource, error) {
	groups, err := kube.Discovery().ServerGroups()
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("listing API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == capiGroup {
			return schema.GroupVersionResource{Group: capiGroup, Version: g.PreferredVersion.Version, Resource: "clusters"}, nil
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("the API server does not serve %s", capiGroup)
}

func hasCAPICluster(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, name string) (bool, error) {
	_, err := dyn.Resource(gvr).Namespace(managementNamespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("reading the Cluster API Cluster %s/%s: %w", managementNamespace, name, err)
	}
}
