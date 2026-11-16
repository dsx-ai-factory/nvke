// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Envtest setup runs fixture cases against a real API server and controller manager.

package controllers

import (
	"context"
	"net/http/httptest"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/cluster-api-provider-nico/pkg/test/fixture"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/fake"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/nvkecontroller"
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
	completed := make(map[*fixture.Case]chan struct{})
	return fixture.CaseSet{
		Description:          description,
		DirPrefix:            dirPrefix,
		MaskExpectedMetadata: true,
		SchemeFn:             newScheme,
		EnvironmentFn:        newEnvironment,
		CompareObjects: func() []client.ObjectList {
			return []client.ObjectList{&internalv1.NVKEClusterList{}}
		},
		Setup: func(ctx SpecContext, tc *fixture.Case, _ fixture.CaseSet) {
			tc.Client = client.WithFieldOwner(tc.Client, "management-engine-envtest")
			Expect(tc.CreateObjects(ctx)).To(Succeed())
			backend := fake.New()
			if input, exists := tc.Input("input_nvke_controller.yaml"); exists {
				Expect(backend.SeedFromYAML(input)).To(Succeed())
			}
			if status, exists := tc.Input("input_status.yaml"); exists {
				var source internalv1.NVKECluster
				Expect(tc.Client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testCluster}, &source)).To(Succeed())
				Expect(yaml.UnmarshalStrict([]byte(status), &source.Status)).To(Succeed())
				Expect(tc.Client.Status().Update(ctx, &source)).To(Succeed())
			}
			server := httptest.NewServer(backend.Handler())
			DeferCleanup(server.Close)
			nvkeControllerClient, err := nvkecontroller.New(nvkecontroller.Config{Endpoint: server.URL})
			Expect(err).NotTo(HaveOccurred())
			tc.AddGolden("expected_nvke_controller.yaml", func(context.Context) (string, error) {
				return backend.Dump()
			})
			setupWithManager(ctx, tc, nvkeControllerClient, completed[tc])
		},
		DefineSteps: func(tc *fixture.Case, set fixture.CaseSet) {
			done := make(chan struct{}, 1)
			completed[tc] = done
			defineSteps(tc, set)
			It("waits for reconciliation to complete", func(ctx SpecContext) {
				waitCtx, cancel := context.WithTimeout(ctx, timeout)

				// Block until the current reconcile, if any, completes to ensure all
				// writes to the object under test have completed
				defer cancel()
				select {
				case <-done:
				case <-waitCtx.Done():
					Fail("waiting for reconciliation: " + waitCtx.Err().Error())
				}
			})
		},
	}
}

func setupWithManager(ctx SpecContext, tc *fixture.Case, nvkeControllerClient NVKEControllerClient, completed chan struct{}) {
	mgr, err := ctrl.NewManager(tc.Config, ctrl.Options{
		Scheme:     tc.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	Expect(err).NotTo(HaveOccurred())
	reconciler := &NVKEClusterReconciler{
		Client:                     mgr.GetClient(),
		NVKEController:             nvkeControllerClient,
		PollInterval:               pollInterval,
		InfraTemplateConfigMapName: "infra-template",
	}
	wrap := func(next reconcile.Reconciler) reconcile.Reconciler {
		return reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
			// Lock the tests from firing a golden update while a reconcile is underway
			select {
			case <-completed:
			default:
			}

			result, err := next.Reconcile(ctx, req)

			select {
			case completed <- struct{}{}:
			default:
			}
			return result, err
		})
	}
	Expect(reconciler.SetupWithManager(mgr, wrap)).To(Succeed())
	tc.StartManager(ctx, mgr)
}
