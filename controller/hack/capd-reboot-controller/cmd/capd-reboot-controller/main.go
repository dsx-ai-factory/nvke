// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package main runs a dev-only controller that reconciles CAPD DockerMachines and
// watches their owner CAPI Machines for reboot requests. When a Machine carries a
// reboot annotation, the controller simulates a reboot and annotates that Machine
// as reboot-complete.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiutil "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	capdv1 "sigs.k8s.io/cluster-api/test/infrastructure/docker/api/v1beta2"
)

const (
	annotationRebootCompleteTimestamp = "nvke.nvidia.com/reboot-complete-timestamp"
	annotationReboot                  = "nvke.nvidia.com/reboot"

	// The controller writes the reboot request under both domains while sites
	// move over, and an older build wrote only these. Read, never written.
	legacyAnnotationReboot                  = "nke.nvidia.com/reboot"
	legacyAnnotationRebootCompleteTimestamp = "nke.nvidia.com/reboot-complete-timestamp"

	reasonRebootComplete           = "RebootComplete"
	fieldOwnerCapdRebootController = "capd-reboot-controller"
	defaultSleepDuration           = 10 * time.Second
)

// version is set at build time via -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	versionFlag := flag.Bool("version", false, "Print the version and exit")
	flag.Parse()

	if *versionFlag {
		_, _ = fmt.Fprintf(os.Stdout, "capd-reboot-controller %s\n", version)
		os.Exit(0)
	}

	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))

	cfg := ctrl.GetConfigOrDie()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := capdv1.AddToScheme(scheme); err != nil {
		panic(err)
	}

	sleepDuration, err := time.ParseDuration(os.Getenv("CAPD_REBOOT_CONTROLLER_SLEEP_DURATION"))
	if err != nil {
		ctrllog.Log.Info("invalid sleep duration; using default", "sleepDuration", defaultSleepDuration)
		sleepDuration = defaultSleepDuration
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	dockerMachineType := &capdv1.DockerMachine{} //nolint:staticcheck // CAPD v1.13.6 still creates DockerMachine; migrate with #88.
	err = ctrl.NewControllerManagedBy(mgr).
		For(dockerMachineType).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(capiutil.MachineToInfrastructureMapFunc(capdv1.GroupVersion.WithKind("DockerMachine"))),
		).
		Complete(&reconciler{Client: mgr.GetClient(), sleepDuration: sleepDuration})
	if err != nil {
		panic(err)
	}

	ctrllog.Log.Info("starting capd-reboot-controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		panic(err)
	}
}

type reconciler struct {
	client.Client
	sleepDuration time.Duration
}

func (r *reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	dockerMachine := &capdv1.DockerMachine{} //nolint:staticcheck // CAPD v1.13.6 still creates DockerMachine; migrate with #88.
	if err := r.Get(ctx, req.NamespacedName, dockerMachine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	machine, err := capiutil.GetOwnerMachine(ctx, r.Client, dockerMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner machine: %w", err)
	}
	if machine == nil {
		log.V(2).Info("no owner Machine found for DockerMachine; skipping")
		return ctrl.Result{}, nil
	}

	rebootAnnotation := cmp.Or(machine.Annotations[annotationReboot], machine.Annotations[legacyAnnotationReboot])
	if rebootAnnotation == "" {
		log.V(2).Info("no Machine reboot annotation found; skipping")
		return ctrl.Result{}, nil
	}

	log.Info("Machine reboot annotation found", "rebootAnnotation", rebootAnnotation)

	if rebootAlreadySignaled(machine, rebootAnnotation) {
		log.V(1).Info("reboot already signaled for this remediation; skipping")
		return ctrl.Result{}, nil
	}

	log.Info("simulating reboot for CAPD machine", "machine", req.Name, "sleepDuration", r.sleepDuration.String())
	select {
	case <-ctx.Done():
		return ctrl.Result{}, fmt.Errorf("context canceled while simulating reboot: %w", ctx.Err())
	case <-time.After(r.sleepDuration):
	}

	rebootCompleteTimestamp := time.Now().UTC().Format(time.RFC3339)
	applyMachine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: machine.Namespace,
			Name:      machine.Name,
			Annotations: map[string]string{
				annotationRebootCompleteTimestamp: rebootCompleteTimestamp,
			},
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
	}

	if err := r.Patch(ctx, applyMachine, client.Apply, client.FieldOwner(fieldOwnerCapdRebootController), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply machine annotation: %w", err)
	}

	log.Info("annotated machine with reboot-complete timestamp", "machine", req.Name, "timestamp", rebootCompleteTimestamp)

	return ctrl.Result{}, nil
}

func rebootAlreadySignaled(machine *clusterv1.Machine, rebootAnnotation string) bool {
	if machine.Annotations == nil || rebootAnnotation == "" {
		return false
	}

	// A reboot this controller completed before the nvke.nvidia.com rename
	// signaled under the legacy key. Ignoring it would replay the
	// simulated reboot and overwrite the completion already recorded.
	raw := cmp.Or(machine.Annotations[annotationRebootCompleteTimestamp], machine.Annotations[legacyAnnotationRebootCompleteTimestamp])
	if raw == "" {
		return false
	}

	rebootCompletionTimestamp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}

	// This is a best effort to parse the timestamp, the controller does currently put the value as a timestamp, but this is not necessarily guaranteed.
	rebootStartedTime, err := time.Parse(time.RFC3339, rebootAnnotation)
	if err != nil {
		return false
	}

	// If the reboot completion timestamp is after the reboot started time, then the reboot has already been signaled.
	return !rebootCompletionTimestamp.Before(rebootStartedTime)
}
