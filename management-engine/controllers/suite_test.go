// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/dsx-ai-factory/cluster-api-provider-nico/pkg/test/fixture"
	internalv1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
	// +kubebuilder:scaffold:imports
)

const (
	testNamespace = "default"
	testCluster   = "cluster-1"
	timeout       = 15 * time.Second
	pollInterval  = 100 * time.Millisecond
)

func TestControllers(t *testing.T) {
	t.Setenv("USE_EXISTING_CLUSTER", "false")
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
})

func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(internalv1.AddToScheme(scheme)).To(Succeed())
	// +kubebuilder:scaffold:scheme
	return scheme
}

func newEnvironment(*fixture.Case) *envtest.Environment {
	return &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
}

func controllerCaseSet(description, dirPrefix string, defineSteps func(*fixture.Case, fixture.CaseSet)) fixture.CaseSet {
	return fixture.CaseSet{
		Description:          description,
		DirPrefix:            dirPrefix,
		MaskExpectedMetadata: true,
		SchemeFn:             newScheme,
		EnvironmentFn:        newEnvironment,
		CompareObjects: func() []client.ObjectList {
			return []client.ObjectList{&internalv1.NKEClusterList{}}
		},
		Setup: func(ctx SpecContext, tc *fixture.Case, _ fixture.CaseSet) {
			tc.Client = client.WithFieldOwner(tc.Client, "management-engine-envtest")
			Expect(tc.CreateObjects(ctx)).To(Succeed())
			setupWithManager(ctx, tc)
		},
		DefineSteps: defineSteps,
	}
}

func setupWithManager(ctx SpecContext, tc *fixture.Case) {
	mgr, err := ctrl.NewManager(tc.Config, ctrl.Options{
		Scheme:     tc.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect((&NKEClusterReconciler{Client: mgr.GetClient(), Scheme: tc.Scheme}).SetupWithManager(mgr)).To(Succeed())
	tc.StartManager(ctx, mgr)
}
