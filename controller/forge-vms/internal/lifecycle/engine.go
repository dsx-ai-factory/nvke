// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle contains the forge-vms reconcile engine.
package lifecycle

import (
	"context"
	"crypto/sha1"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/cloudinit"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/config"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/ipxe"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/netutil"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/state"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/vm"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/vm/artifacts"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/vm/darwinqemu"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/vm/libvirt"
)

type Engine struct {
	cfg         config.Config
	store       *state.Store
	logger      *slog.Logger
	runtime     vm.Runtime
	net         NetworkManager
	artifacts   artifacts.Builder
	bootPatcher *ipxe.BootPatcher
	imageCache  *ipxe.ImageCache

	taskCh chan string
	wg     sync.WaitGroup

	lockMap sync.Map // map[string]*sync.Mutex

	queuedMu sync.Mutex
	queued   map[string]struct{}
}

func New(cfg config.Config, store *state.Store, logger *slog.Logger) *Engine {
	rt := newRuntime(cfg)
	patcher := &ipxe.BootPatcher{
		Logger:      logger.With("component", "bootpatcher"),
		CacheDir:    cfg.ImageCacheDirectory(),
		DebugFSPath: findDebugFS(),
		QEMUImgPath: cfg.QEMUImgPath,
	}
	return &Engine{
		cfg:     cfg,
		store:   store,
		logger:  logger.With("component", "controller"),
		runtime: rt,
		net:     newNetworkManager(cfg, rt),
		artifacts: artifacts.Builder{
			QEMUImgPath:      cfg.QEMUImgPath,
			CloudLocalDSPath: cfg.CloudLocalDSPath,
			BaseImage:        cfg.BaseImage,
			MockMode:         cfg.EffectiveVMBackend() == config.VMBackendMock,
		},
		bootPatcher: patcher,
		imageCache:  ipxe.NewImageCache(cfg.ImageCacheDirectory(), logger.With("component", "imagecache")),
		taskCh:      make(chan string, cfg.ReconcileWorkers*16),
		queued:      make(map[string]struct{}),
	}
}

func (e *Engine) Start(ctx context.Context) error {
	if err := e.net.EnsureSetup(ctx); err != nil {
		return fmt.Errorf("network setup failed: %w", err)
	}

	// Detect VMs that exist in the hypervisor but not in the database.
	e.detectOrphanedVMs(ctx)

	for i := 0; i < e.cfg.ReconcileWorkers; i++ {
		e.wg.Add(1)
		go e.worker(ctx, i)
	}

	e.wg.Add(1)
	go e.dispatcher(ctx)

	return nil
}

// detectOrphanedVMs lists all VMs known to the hypervisor and compares them
// against the database. VMs that exist in the hypervisor but have no matching
// database record are considered orphaned. By default a warning is logged for
// each orphan. When FORGE_VMS_CLEANUP_ORPHANED_VMS=true the orphans are
// deleted from the hypervisor.
func (e *Engine) detectOrphanedVMs(ctx context.Context) {
	hypervisorMachines, err := e.runtime.ListMachines(ctx)
	if err != nil {
		e.logger.Error("Failed to list hypervisor machines for orphan detection", "error", err)
		return
	}
	if len(hypervisorMachines) == 0 {
		return
	}

	knownMachines, err := e.store.ListActiveMachineIDs(ctx)
	if err != nil {
		e.logger.Error("Failed to list active machine IDs for orphan detection", "error", err)
		return
	}

	for _, machineID := range hypervisorMachines {
		if _, exists := knownMachines[machineID]; exists {
			continue
		}

		if e.cfg.CleanupOrphanedVMs {
			e.logger.Info("Cleaning up orphaned VM", "machineID", machineID)
			if delErr := e.runtime.Delete(ctx, machineID); delErr != nil {
				e.logger.Error("Failed to clean up orphaned VM", "machineID", machineID, "error", delErr)
			} else {
				e.logger.Info("Orphaned VM cleaned up successfully", "machineID", machineID)
			}
		} else {
			e.logger.Warn("Orphaned VM detected: exists in hypervisor but not in database",
				"machineID", machineID,
				"hint", "set FORGE_VMS_CLEANUP_ORPHANED_VMS=true to auto-cleanup")
		}
	}
}

func (e *Engine) Wait() {
	e.wg.Wait()
}

// MetadataAddrs returns all addresses the metadata server should bind.
func (e *Engine) MetadataAddrs() []string {
	return e.net.MetadataAddrs()
}

// Teardown cleans up network-mode-specific resources. Call after Wait().
func (e *Engine) Teardown(ctx context.Context) {
	if err := e.net.Teardown(ctx); err != nil {
		e.logger.Error("network teardown failed", "error", err)
	}
}

func (e *Engine) dispatcher(ctx context.Context) {
	defer e.wg.Done()

	ticker := time.NewTicker(e.cfg.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			close(e.taskCh)
			return
		case <-ticker.C:
			candidates, err := e.store.ListReconcileCandidates(ctx)
			if err != nil {
				e.logger.Error("failed to list reconcile candidates", "error", err)
				continue
			}
			for _, id := range candidates {
				e.enqueueTask(ctx, id)
			}
		}
	}
}

func (e *Engine) worker(ctx context.Context, workerIdx int) {
	defer e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-e.taskCh:
			if !ok {
				return
			}
			err := e.reconcileInstance(ctx, id)
			// TODO: improve this, it should probably only mark it as done if the reconcile succeeded
			// or requeue it if it failed with an error.
			e.markTaskDone(id)
			if err != nil {
				e.logger.Error("reconcile failed", "instanceID", id, "worker", workerIdx, "error", err)
			}
		}
	}
}

func (e *Engine) enqueueTask(ctx context.Context, instanceID string) bool {
	e.queuedMu.Lock()
	if _, exists := e.queued[instanceID]; exists {
		e.queuedMu.Unlock()
		return true
	}
	e.queued[instanceID] = struct{}{}
	e.queuedMu.Unlock()

	select {
	case <-ctx.Done():
		e.markTaskDone(instanceID)
		return false
	case e.taskCh <- instanceID:
		return true
	}
}

func (e *Engine) markTaskDone(instanceID string) {
	e.queuedMu.Lock()
	delete(e.queued, instanceID)
	e.queuedMu.Unlock()
}

func (e *Engine) getInstanceLock(instanceID string) *sync.Mutex {
	v, _ := e.lockMap.LoadOrStore(instanceID, &sync.Mutex{})
	return v.(*sync.Mutex) //nolint:forcetypeassert // controlled local type
}

func (e *Engine) reconcileInstance(ctx context.Context, instanceID string) error {
	lock := e.getInstanceLock(instanceID)
	if !lock.TryLock() {
		return nil
	}
	defer lock.Unlock()

	instanceWithInterfaces, err := e.store.GetInstanceByID(ctx, instanceID)
	if err != nil {
		if err == state.ErrNotFound {
			return nil
		}
		return err
	}

	log := e.logger.With("instanceName", instanceWithInterfaces.Instance.Name, "instanceID", instanceID)

	instance := instanceWithInterfaces.Instance
	if instance.DesiredState == state.DesiredStateAbsent {
		return e.reconcileAbsent(ctx, instanceWithInterfaces, log)
	}
	if instance.ObservedState == state.ObservedStateError {
		return nil
	}

	if instance.RebootRequested {
		return e.reconcileReboot(ctx, instance, log)
	}

	switch instance.ObservedState {
	case state.ObservedStateNew, state.ObservedStateProvisioning:
		return e.reconcileProvisioning(ctx, instanceWithInterfaces, log)
	case state.ObservedStateRunning, state.ObservedStateRebooting:
		return e.reconcileRunning(ctx, instanceWithInterfaces, log)
	case state.ObservedStateReady:
		return e.reconcileReady(ctx, instanceWithInterfaces.Instance, log)
	case state.ObservedStateStopped:
		// TODO: consider auto-recovery of stopped instances
		return nil
	default:
		return nil
	}
}

func (e *Engine) reconcileAbsent(ctx context.Context, in state.InstanceWithInterfaces, log *slog.Logger) error {
	instance := in.Instance
	log.Info("Marking instance as deleting")
	if err := e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
		Status:        ptr("deleting"),
		ObservedState: ptr(state.ObservedStateDeleting),
	}); err != nil {
		return err
	}

	// Remove DHCP host/lease entry.
	nicMAC := nicMACForInstance(instance.ID)
	removeDHCPHostEntry(e.cfg.DHCPHostsDir, e.cfg.DHCPLeaseFile, instance.ID, nicMAC)

	if instance.MachineID != "" {
		log.Info("Deleting VM")
		if err := e.runtime.Delete(ctx, instance.MachineID); err != nil {
			log.Warn("Failed to delete VM domain; continuing with state cleanup", "machineID", instance.MachineID, "error", err)
		}
		log.Info("VM deleted")
	}

	log.Info("Hard deleting instance from database")
	if err := e.store.DeleteInstanceHard(ctx, instance.ID); err != nil {
		return fmt.Errorf("failed to hard delete instance %s: %w", instance.ID, err)
	}

	// Clean up the instance working directory (disk overlay, seed ISO, console logs).
	workingDir := filepath.Join(e.cfg.WorkingDirectory(), instance.ID)
	if err := os.RemoveAll(workingDir); err != nil {
		log.Warn("Failed to remove instance directory", "path", workingDir, "error", err)
	} else {
		log.Info("Removed instance directory", "path", workingDir)
	}
	return nil
}

func (e *Engine) reconcileProvisioning(ctx context.Context, in state.InstanceWithInterfaces, log *slog.Logger) error {
	instance := in.Instance
	// If machine already exists (e.g. after a process restart), check its state.
	if instance.MachineID != "" {
		log.Info("Machine exists, checking if it is running", "machineID", instance.MachineID)
		running, err := e.runtime.IsRunning(ctx, instance.MachineID)
		if err == nil && running {
			log.Info("Machine is running, patching instance state to running", "machineID", instance.MachineID)
			patch := state.InstanceStatePatch{
				Status:        ptr("running"),
				ObservedState: ptr(state.ObservedStateRunning),
				ErrorReason:   ptr(""),
			}
			if instance.TpmEkCertificate == "" {
				ekCert, ekErr := e.runtime.TPMEKCert(ctx, instance.MachineID)
				if ekErr != nil {
					return fmt.Errorf("failed to read TPM EK certificate: %w", ekErr)
				}
				if ekCert != "" {
					patch.TpmEkCertificate = &ekCert
				}
			}
			return e.store.PatchInstanceState(ctx, instance.ID, patch)
		}
		log.Info("Machine exists but is not running, deleting VM", "machineID", instance.MachineID)
		// TODO: this should probably just mark the vm as errored, we don't know what happened here and if it's safe to recreate
		_ = e.runtime.Delete(ctx, instance.MachineID)
		log.Info("VM deleted")
	}

	nicMAC := nicMACForInstance(instance.ID)
	prefixLen := netutil.CIDRPrefixLength(e.cfg.BridgeCIDR)
	gw, err := netutil.GatewayIP(e.cfg.BridgeCIDR)
	if err != nil {
		return e.markError(ctx, instance.ID, fmt.Errorf("failed to derive gateway IP: %w", err))
	}

	// Allocate IP or configure DHCP based on networking mode.
	var firstIP string
	if e.net.NeedsDHCP(instance) && !e.artifacts.MockMode {
		// DHCP: no IP yet. VM will get one from dnsmasq. Register MAC + host entry.
		e.registerDHCPInstance(ctx, instance, in.Interfaces, nicMAC, log)
	} else {
		// Static: allocate IP via IPAM (idempotent — skips if already allocated).
		firstIP, _ = firstInterfaceIP(in.Interfaces)
		if firstIP == "" && len(in.Interfaces) > 0 {
			ips, allocErr := e.store.AllocateAndStoreIPs(ctx, in.Interfaces[0], instance.ID)
			if allocErr != nil {
				return e.markError(ctx, instance.ID, fmt.Errorf("IPAM allocation failed: %w", allocErr))
			}
			if len(ips) > 0 {
				firstIP = ips[0]
			}
			log.Info("Allocated static IP", "ip", firstIP)
		}
	}

	// Resolve boot image: parse iPXE script, download image, inject bootcmd.
	baseImage, useUEFI, userData, err := e.resolveBootImage(ctx, instance, log)
	if err != nil {
		return e.markError(ctx, instance.ID, err)
	}

	// Verify that a usable base image exists. iPXE instances supply their own
	// (downloaded by resolveBootImage); non-iPXE instances fall back to the
	// configured default. If neither is available, fail the instance.
	if !e.artifacts.MockMode {
		effectiveImage := baseImage
		if effectiveImage == "" {
			effectiveImage = e.cfg.BaseImage
		}
		if _, statErr := os.Stat(effectiveImage); statErr != nil {
			return e.markError(ctx, instance.ID, fmt.Errorf("no bootable image available: base image %q does not exist", effectiveImage))
		}
	}

	// Inject diagnostic SSH user if enabled.
	if e.cfg.DiagSSHEnabled {
		userData = cloudinit.InjectDiagUser(userData, e.cfg.DiagSSHUser, e.cfg.DiagSSHPassword)
	}

	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}

	// Resolve vCPUs, memory, and disk from instance type, falling back to global config.
	vCPUs := e.cfg.VMVCPUs
	memoryMiB := e.cfg.VMMemoryMiB
	diskGiB := e.cfg.VMDiskGiB
	if instance.InstanceTypeID != "" {
		itype, itErr := e.store.GetInstanceTypeByID(ctx, instance.InstanceTypeID)
		if itErr != nil {
			return fmt.Errorf("failed to resolve instance type %s: %w", instance.InstanceTypeID, itErr)
		}
		if itype.CPUCount > 0 {
			vCPUs = itype.CPUCount
		}
		if itype.MemoryGiB > 0 {
			memoryMiB = itype.MemoryGiB * 1024
		}
		if itype.DiskGiB > 0 {
			diskGiB = itype.DiskGiB
		}
	}

	// Build artifacts. IPAddress empty → DHCP network-config in seed ISO.
	workingDir := filepath.Join(e.cfg.WorkingDirectory(), instance.ID)
	artifactsOut, err := e.artifacts.Build(ctx, artifacts.BuildInput{
		WorkingDir:   workingDir,
		InstanceID:   instance.ID,
		Hostname:     instance.Name,
		UserData:     userData,
		NICMAC:       nicMAC,
		IPAddress:    firstIP,
		GatewayIP:    gw,
		PrefixLength: prefixLen,
		BaseImage:    baseImage,
		DiskGiB:      diskGiB,
	})
	if err != nil {
		return e.markError(ctx, instance.ID, fmt.Errorf("artifact creation failed: %w", err))
	}

	domainName := "forge-vm-" + instance.ID
	log.Info("Creating and starting VM",
		"domainName", domainName,
		"instanceID", instance.ID,
		"bridgeName", e.cfg.BridgeName,
		"vCPUs", vCPUs, "memoryMiB", memoryMiB,
		"diskPath", artifactsOut.DiskPath,
		"seedISOPath", artifactsOut.SeedISOPath,
		"nicMAC", nicMAC,
		"arch", arch,
		"uefi", useUEFI,
	)
	machineID, err := e.runtime.CreateAndStart(ctx, vm.InstanceSpec{
		InstanceID:  instance.ID,
		DomainName:  domainName,
		BridgeName:  e.cfg.BridgeName,
		NetworkName: e.cfg.LibvirtNetwork,
		VCPUs:       vCPUs,
		MemoryMiB:   memoryMiB,
		DiskPath:    artifactsOut.DiskPath,
		SeedISOPath: artifactsOut.SeedISOPath,
		NICMAC:      nicMAC,
		Arch:        arch,
		UEFI:        useUEFI,
	})
	if err != nil {
		return e.markError(ctx, instance.ID, fmt.Errorf("vm create/start failed: %w", err))
	}

	// Persist machine ID immediately so crash recovery can find the VM
	// even if the EK cert read below fails.
	if err := e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
		MachineID: &machineID,
	}); err != nil {
		return err
	}

	patch := state.InstanceStatePatch{
		Status:        ptr("running"),
		ObservedState: ptr(state.ObservedStateRunning),
		ErrorReason:   ptr(""),
	}
	ekCert, ekErr := e.runtime.TPMEKCert(ctx, machineID)
	if ekErr != nil {
		return fmt.Errorf("failed to read TPM EK certificate: %w", ekErr)
	}
	if ekCert != "" {
		patch.TpmEkCertificate = &ekCert
	}

	log.Info("Patching instance state to running", "machineID", machineID)
	if err := e.store.PatchInstanceState(ctx, instance.ID, patch); err != nil {
		return fmt.Errorf("marking instance as running: %w", err)
	}

	if instance.UsesDHCP() {
		log.Info("Waiting for DHCP lease", "mac", nicMAC)
		return nil
	}
	return nil
}

func (e *Engine) reconcileRunning(ctx context.Context, in state.InstanceWithInterfaces, log *slog.Logger) error {
	instance := in.Instance
	if instance.MachineID == "" {
		return e.markError(ctx, instance.ID, fmt.Errorf("instance has no machine id in running state"))
	}

	running, err := e.runtime.IsRunning(ctx, instance.MachineID)
	if err != nil {
		return e.markError(ctx, instance.ID, fmt.Errorf("failed checking machine state: %w", err))
	}
	if !running {
		// TODO: this should probably just mark the vm as errored,
		// we don't know what happened here and if it's safe to recreate
		// Our code only marks a machine as running after it calls create vm and that succeeds.
		// So this should not happen unless the vm was manually deleted or it somehow got into a bad state.
		// In which case, the use probably wants to know about it and investigate.

		// Drift detected: DB says running but runtime disagrees.
		// Fall back to provisioning so next tick re-creates the VM.
		log.Warn("Drift detected: machine not running, re-provisioning",
			"instanceID", instance.ID, "machineID", instance.MachineID)
		return e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
			Status:        ptr("provisioning"),
			ObservedState: ptr(state.ObservedStateProvisioning),
			MachineID:     ptr(""),
			ErrorReason:   ptr(""),
		})
	}

	ready := true
	if e.runtime.SupportsTCPProbe() {
		ip, err := firstInterfaceIP(in.Interfaces)
		if err != nil {
			return fmt.Errorf("getting first interface IP: %w", err)
		}

		if ip == "" {
			ip, err := e.resolveInstanceIPFromLease(ctx, instance, in.Interfaces)
			if err != nil {
				return e.markError(ctx, instance.ID, err)
			}
			if ip == "" {
				return nil // Waiting for DHCP lease; retry next reconcile tick.
			}

			nicMAC := nicMACForInstance(instance.ID)

			// Lease found — store the confirmed IP and pin it in the dnsmasq host
			// entry so the VM always receives this address on future renewals.
			if err := e.store.UpdateInterfaceIPs(ctx, in.Interfaces[0].ID, []string{ip}); err != nil {
				return fmt.Errorf("store lease IP: %w", err)
			}
			e.net.WriteReservation(instance.ID, nicMAC, instance.Name, ip, log)
			log.Info("Confirmed VM IP from DHCP lease", "ip", ip, "mac", nicMAC)

			if instance.PhoneHomeEnabled {
				log.Info("Waiting for cloud-init phone_home", "ipAddress", ip)
			} else {
				log.Info("Waiting for SSH probe to succeed", "ipAddress", ip)
			}
		}

		if instance.PhoneHomeEnabled {
			// Phone-home readiness: wait for cloud-init to POST phone_home
			// instead of just SSH port being open. This means "ready" truly
			// indicates cloud-init finished all runcmd steps.
			ready = instance.PhonedHomeAt != nil
			if ready {
				log.Info("Cloud-init phone_home was received, machine is ready")
			}
		} else {
			ready = probeSSH(ip, 2*time.Second)
			if ready {
				log.Info("SSH probe succeeded, machine is ready")
			}
		}
	} else {
		log.Info("Machine does not support TCP probe, assuming ready")
	}

	if ready {
		log.Info("Machine is ready, marking instance as ready")
		return e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
			Status:          ptr("ready"),
			ObservedState:   ptr(state.ObservedStateReady),
			RebootRequested: ptrBool(false),
			ErrorReason:     ptr(""),
		})
	}

	if e.cfg.ReadinessTimeout > 0 && e.storeAge(instance) > e.cfg.ReadinessTimeout {
		log.Info("Readiness timeout reached before tcp probe succeeded, marking instance as errored")
		return e.markError(ctx, instance.ID, fmt.Errorf("readiness timeout reached before tcp probe succeeded"))
	}

	return nil
}

func (e *Engine) reconcileReady(ctx context.Context, instance state.InstanceRow, log *slog.Logger) error {
	if instance.MachineID == "" {
		return nil
	}

	machineState, err := e.runtime.GetState(ctx, instance.MachineID)
	if err != nil {
		log.Warn("Failed to query VM state, skipping health check", "machineID", instance.MachineID, "error", err)
		return nil
	}

	switch machineState {
	case vm.MachineStateRunning:
		return nil
	case vm.MachineStatePaused:
		log.Warn("VM paused, marking instance as stopped", "machineID", instance.MachineID)
		return e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
			ObservedState: ptr(state.ObservedStateStopped),
			Status:        ptr("suspended"),
			ErrorReason:   ptr("VM paused"),
		})
	case vm.MachineStateShutOff:
		log.Warn("VM shut off, marking instance as stopped", "machineID", instance.MachineID)
		return e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
			ObservedState: ptr(state.ObservedStateStopped),
			Status:        ptr("stopped"),
			ErrorReason:   ptr("VM shut off"),
		})
	case vm.MachineStateCrashed:
		log.Error("VM crashed, marking instance as error", "machineID", instance.MachineID)
		return e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
			ObservedState: ptr(state.ObservedStateError),
			Status:        ptr("error"),
			ErrorReason:   ptr("VM crashed"),
		})
	case vm.MachineStateNotFound:
		log.Error("VM no longer exists in hypervisor, marking instance as error", "machineID", instance.MachineID)
		return e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
			ObservedState: ptr(state.ObservedStateError),
			Status:        ptr("error"),
			ErrorReason:   ptr("VM no longer exists in hypervisor"),
		})
	default:
		return nil
	}
}

func (e *Engine) reconcileReboot(ctx context.Context, instance state.InstanceRow, log *slog.Logger) error {
	if instance.MachineID == "" {
		log.Error("Cannot reboot instance without id, marking instance as errored")
		return e.markError(ctx, instance.ID, fmt.Errorf("cannot reboot instance without machine id"))
	}

	log.Info("Marking instance as rebooting")
	if err := e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
		Status:          ptr("rebooting"),
		ObservedState:   ptr(state.ObservedStateRebooting),
		RebootRequested: ptrBool(false),
	}); err != nil {
		return err
	}

	log.Info("Rebooting machine", "machineID", instance.MachineID)
	if err := e.runtime.Reboot(ctx, instance.MachineID); err != nil {
		return e.markError(ctx, instance.ID, fmt.Errorf("reboot failed: %w", err))
	}
	log.Info("Machine rebooted")

	log.Info("Marking instance as running, future reconciliation will wait for machine to be ready")
	return e.store.PatchInstanceState(ctx, instance.ID, state.InstanceStatePatch{
		Status:        ptr("running"),
		ObservedState: ptr(state.ObservedStateRunning),
	})
}

func (e *Engine) markError(ctx context.Context, instanceID string, reconcileErr error) error {
	errText := reconcileErr.Error()
	if err := e.store.PatchInstanceState(ctx, instanceID, state.InstanceStatePatch{
		Status:        ptr("error"),
		ObservedState: ptr(state.ObservedStateError),
		ErrorReason:   &errText,
	}); err != nil {
		return fmt.Errorf("reconcile error %q; additionally failed to persist error state: %w", reconcileErr, err)
	}
	return reconcileErr
}

func (e *Engine) storeAge(instance state.InstanceRow) time.Duration {
	return e.storeNow().Sub(instance.CreatedAt)
}

func (e *Engine) storeNow() time.Time {
	return time.Now().UTC()
}

// resolveBootImage determines the base image and UEFI setting for an instance.
// For iPXE instances, parses the script, downloads the image, and injects bootcmd.
// For non-iPXE instances, returns the default base image.
func (e *Engine) resolveBootImage(ctx context.Context, instance state.InstanceRow, log *slog.Logger) (baseImage string, useUEFI bool, userData string, err error) {
	userData = instance.UserData
	if instance.IpxeScript == "" {
		return "", false, userData, nil
	}

	spec, parseErr := ipxe.Parse(instance.IpxeScript)
	if parseErr != nil {
		return "", false, "", fmt.Errorf("ipxe script parse failed: %w", parseErr)
	}
	log.Info("Parsed iPXE script", "imageURL", spec.ImageURL, "updateGrubTmpl", spec.UpdateGrubTmpl)

	cachedImage, dlErr := e.imageCache.Ensure(ctx, spec.ImageURL)
	if dlErr != nil {
		return "", false, "", fmt.Errorf("ipxe image download failed: %w", dlErr)
	}

	bootcmdYAML := spec.GRUBBootcmdYAML()
	if bootcmdYAML != "" {
		userData = ipxe.PrependBootcmd(userData, bootcmdYAML)
		log.Info("Injected GRUB bootcmd into userData")
	}

	return cachedImage, true, userData, nil
}

// registerDHCPInstance stores the MAC in the database and writes a host
// reservation if the active network mode requires it.
func (e *Engine) registerDHCPInstance(ctx context.Context, instance state.InstanceRow, interfaces []state.InstanceInterfaceRow, nicMAC string, log *slog.Logger) {
	if len(interfaces) > 0 {
		if err := e.store.UpdateInterfaceMAC(ctx, interfaces[0].ID, nicMAC); err != nil {
			log.Warn("Failed to store MAC in interface", "error", err)
		}
	}
	e.net.WriteReservation(instance.ID, nicMAC, instance.Name, "", log)
}

func firstInterfaceIP(interfaces []state.InstanceInterfaceRow) (string, error) {
	if len(interfaces) == 0 {
		return "", fmt.Errorf("instance has no interfaces")
	}
	ips, err := interfaces[0].IPAddresses()
	if err != nil {
		return "", err
	}

	if len(ips) == 0 {
		return "", nil
	}

	return ips[0], nil
}

func probeSSH(ipAddress string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ipAddress, "22"), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func ptr(s string) *string { return &s }

func ptrBool(v bool) *bool { return &v }

func findDebugFS() string {
	candidates := []string{
		"/opt/homebrew/opt/e2fsprogs/sbin/debugfs",
		"/usr/local/opt/e2fsprogs/sbin/debugfs",
		"/usr/sbin/debugfs",
		"/sbin/debugfs",
		"debugfs",
	}
	for _, p := range candidates {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	return "debugfs"
}

// writeDHCPHostEntry writes a dnsmasq dhcp-host file so the VM gets an IP
// via DHCP. dnsmasq watches the hosts directory via inotify.
// id:* tells dnsmasq to ignore the client-id (DUID) and match by MAC only.
// When ip is non-empty the entry pins that address so the VM always receives
// the same IP across reboots and lease renewals.
func writeDHCPHostEntry(hostsDir, instanceID, mac, hostname, ip string) error {
	if hostsDir == "" {
		return nil
	}
	if err := os.MkdirAll(hostsDir, 0o755); err != nil {
		return fmt.Errorf("create dhcp hosts dir: %w", err)
	}
	var entry string
	if ip != "" {
		entry = fmt.Sprintf("id:*,%s,%s,%s\n", mac, ip, hostname)
	} else {
		entry = fmt.Sprintf("id:*,%s,%s\n", mac, hostname)
	}
	return os.WriteFile(filepath.Join(hostsDir, instanceID), []byte(entry), 0o644)
}

// resolveInstanceIPFromLease returns the IP to use for readiness probing.
// DHCP-mode instances start with no IP; the lease is checked each tick until
// an IP appears, then stored in the database.
func (e *Engine) resolveInstanceIPFromLease(ctx context.Context, instance state.InstanceRow, interfaces []state.InstanceInterfaceRow) (string, error) {
	if len(interfaces) == 0 {
		return "", fmt.Errorf("instance has no interfaces")
	}

	// Return stored IP if available (static instances always have one;
	// DHCP instances get one after their first lease confirmation).
	ip, _ := firstInterfaceIP(interfaces)
	if ip != "" {
		return ip, nil
	}

	nicMAC := nicMACForInstance(instance.ID)
	leaseIP, ok, err := e.net.LookupLease(ctx, nicMAC)
	if err != nil {
		return "", fmt.Errorf("DHCP lease lookup failed: %w", err)
	}
	if !ok {
		return "", nil // no lease yet; retry next tick
	}
	return leaseIP, nil
}

// removeDHCPHostEntry removes the dnsmasq dhcp-host file for an instance
// and purges its MAC from the lease file so the IP is immediately available.
func removeDHCPHostEntry(hostsDir, leaseFile, instanceID, mac string) {
	if hostsDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(hostsDir, instanceID))

	// Purge the MAC's lease from the dnsmasq lease file.
	// This prevents stale leases from blocking IP reuse on subsequent runs.
	if leaseFile == "" {
		return
	}
	data, err := os.ReadFile(leaseFile)
	if err != nil {
		return
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.Contains(line, mac) {
			continue
		}
		kept = append(kept, line)
	}
	_ = os.WriteFile(leaseFile, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}

func nicMACForInstance(instanceID string) string {
	sum := sha1.Sum([]byte(instanceID))
	// 0x00 -> globally unique unicast MAC prefix. OVS bridges inherit
	// the MAC of their first non-internal port, but only when the port
	// has a globally unique address (locally-administered bit NOT set).
	// Using 0x00 ensures the OVS bridge keeps the NIC's MAC after
	// configure-openvswitch runs, so the DHCP lease — and the node's
	// IP — stays the same. This matches real Forge bare-metal behavior
	// where physical NICs have vendor-assigned (globally unique) MACs.
	return fmt.Sprintf(
		"00:%02x:%02x:%02x:%02x:%02x",
		sum[0],
		sum[1],
		sum[2],
		sum[3],
		sum[4],
	)
}

func newRuntime(cfg config.Config) vm.Runtime {
	switch cfg.EffectiveVMBackend() {
	case config.VMBackendMock:
		return vm.NewMockRuntime()
	case config.VMBackendDarwinQEMU:
		return darwinqemu.New(cfg.EffectiveQEMUSystemPath(), cfg.WorkingDirectory())
	default:
		return libvirt.New(cfg.LibvirtURI)
	}
}
