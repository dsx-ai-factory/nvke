// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package darwinqemu

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/vm/toolpath"
)

// swtpmPaths holds the on-disk locations for one instance's software TPM.
type swtpmPaths struct {
	StateDir   string // <instanceDir>/tpm — the only recovery-critical artifact for an FDE guest
	CtrlSock   string // control socket, under os.TempDir() (macOS sun_path cap, same scheme as QMPSock)
	PIDFile    string // <instanceDir>/swtpm.pid — swtpm's own, separate from QEMU's
	EKCertPath string // <instanceDir>/tpm-ek.crt — base64-encoded DER, written once at seed time
	LogPath    string // <instanceDir>/swtpm.log
}

func swtpmPathsForInstance(instanceDir, instanceID string) swtpmPaths {
	return swtpmPaths{
		StateDir:   filepath.Join(instanceDir, "tpm"),
		CtrlSock:   filepath.Join(os.TempDir(), swtpmSocketName(instanceID)),
		PIDFile:    filepath.Join(instanceDir, "swtpm.pid"),
		EKCertPath: filepath.Join(instanceDir, "tpm-ek.crt"),
		LogPath:    filepath.Join(instanceDir, "swtpm.log"),
	}
}

// swtpmSocketName mirrors qmpSocketName's truncation: macOS caps
// sockaddr_un.sun_path at 104 bytes, so the ctrl socket cannot live under a
// deep instancesDir path the way the TPM state directory does.
func swtpmSocketName(instanceID string) string {
	shortID := instanceID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return fmt.Sprintf("nvm-tpm-%s.sock", shortID)
}

// swtpmAvailable reports whether swtpm and swtpm_setup are on PATH, or
// configured via NICO_VMS_SWTPM_PATH / NICO_VMS_SWTPM_SETUP_PATH. A host
// without them skips TPM support entirely — same degrade-silently contract
// as the libvirt backend's setupTPM.
func swtpmAvailable() (swtpmBin, swtpmSetupBin string, ok bool) {
	swtpmBin = toolpath.Find("swtpm", "NICO_VMS_SWTPM_PATH")
	swtpmSetupBin = toolpath.Find("swtpm_setup", "NICO_VMS_SWTPM_SETUP_PATH")
	return swtpmBin, swtpmSetupBin, swtpmBin != "" && swtpmSetupBin != ""
}

// tpmStateSeeded reports whether swtpm_setup has already run for this
// instance. swtpm_setup must never run twice against a bound FDE guest: a
// second run mints a different EK and permanently locks it out of its own
// disk once the passphrase slot has been removed. A Stat error other than
// "not found" (EACCES, EIO, ...) is returned as an error rather than
// silently treated as "unseeded": misreading it as false would seed right
// over a directory whose real state we simply failed to read.
func tpmStateSeeded(paths swtpmPaths) (bool, error) {
	_, err := os.Stat(filepath.Join(paths.StateDir, "tpm2-00.permall"))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// ensureSwtpmState seeds swtpm state for the instance exactly once and
// returns the base64-encoded DER EK certificate. If state already exists —
// from a prior seed, or nico-sim adopting an instance across a restart — it
// returns the cached cert instead of reseeding. Any seeding failure removes
// the state dir: QEMU has not been launched yet, so no guest can have sealed
// anything to a half-seeded TPM.
func ensureSwtpmState(ctx context.Context, swtpmSetupBin string, paths swtpmPaths) (encoded string, err error) {
	seeded, err := tpmStateSeeded(paths)
	if err != nil {
		return "", fmt.Errorf("check swtpm state at %s: %w", paths.StateDir, err)
	}
	if seeded {
		certData, err := os.ReadFile(paths.EKCertPath)
		if err != nil {
			return "", fmt.Errorf("swtpm state exists but EK cert is missing at %s: %w", paths.EKCertPath, err)
		}
		return strings.TrimSpace(string(certData)), nil
	}

	defer func() {
		if err != nil {
			_ = os.RemoveAll(paths.StateDir)
		}
	}()
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		return "", fmt.Errorf("create swtpm state dir %s: %w", paths.StateDir, err)
	}

	instanceDir := filepath.Dir(paths.StateDir)
	certDir, err := os.MkdirTemp(instanceDir, "tpm-certs-")
	if err != nil {
		return "", fmt.Errorf("create swtpm cert scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(certDir) }()

	setupConf, err := writeSwtpmSetupConfig(instanceDir, swtpmSetupBin)
	if err != nil {
		return "", fmt.Errorf("write swtpm_setup config: %w", err)
	}

	cmd := exec.CommandContext(ctx, swtpmSetupBin,
		"--tpm2",
		"--tpmstate", paths.StateDir,
		"--config", setupConf,
		"--create-ek-cert",
		"--create-platform-cert",
		"--write-ek-cert-files", certDir,
	)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		return "", fmt.Errorf("swtpm_setup failed: %w: %s", runErr, string(output))
	}

	// Read the EK certificate (DER format) written by swtpm_setup. Same
	// candidate order as the libvirt backend; rsa2048 is always produced by
	// default so it matches first.
	var certData []byte
	for _, name := range []string{"ek-rsa2048.crt", "ek-rsa3072.crt", "ek-ecc384.crt"} {
		if data, readErr := os.ReadFile(filepath.Join(certDir, name)); readErr == nil {
			certData = data
			break
		}
	}
	if certData == nil {
		return "", fmt.Errorf("swtpm_setup produced no EK certificate in %s", certDir)
	}

	encoded = base64.StdEncoding.EncodeToString(certData)
	if err := os.WriteFile(paths.EKCertPath, []byte(encoded), 0o644); err != nil {
		return "", fmt.Errorf("write EK cert %s: %w", paths.EKCertPath, err)
	}
	return encoded, nil
}

// resolveSwtpmLocalCA finds swtpm_localca alongside the resolved
// swtpm_setup binary first, falling back to PATH. Checking the sibling
// path first (rather than a bare exec.LookPath) means an explicit
// NICO_VMS_SWTPM_SETUP_PATH override is honored consistently: the
// swtpm_localca that ships with that same install is preferred over
// whatever else happens to be on PATH.
func resolveSwtpmLocalCA(swtpmSetupBin string) (string, error) {
	sibling := filepath.Join(filepath.Dir(swtpmSetupBin), "swtpm_localca")
	if info, err := os.Stat(sibling); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return sibling, nil
	}
	if p, err := exec.LookPath("swtpm_localca"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("swtpm_localca not found next to swtpm_setup (%s) or on PATH", swtpmSetupBin)
}

// writeSwtpmSetupConfig writes a swtpm_setup config that points
// swtpm_localca at a writable state directory under instanceDir.
//
// This is necessary: verified against the flox-packaged swtpm 0.11.0 build,
// swtpm_setup's bundled default config (baked into the package at build
// time) points swtpm_localca at /var/lib/swtpm-localca, which an
// unprivileged user cannot write — swtpm_localca fails with "Could not
// create directory for 'statedir': Permission denied" and swtpm_setup exits
// 0 anyway, leaving no state and no cert. A Homebrew build has the same
// problem with a different hardcoded path. Resolving swtpm_localca
// ourselves (rather than trusting either package's baked-in default) works
// across both installs.
func writeSwtpmSetupConfig(instanceDir, swtpmSetupBin string) (string, error) {
	localca, err := resolveSwtpmLocalCA(swtpmSetupBin)
	if err != nil {
		return "", err
	}

	localcaDir := filepath.Join(instanceDir, "swtpm-localca")
	if err := os.MkdirAll(localcaDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", localcaDir, err)
	}

	localcaConf := filepath.Join(instanceDir, "swtpm-localca.conf")
	localcaContent := fmt.Sprintf("statedir = %s\nsigningkey = %s\nissuercert = %s\ncertserial = %s\n",
		localcaDir,
		filepath.Join(localcaDir, "signkey.pem"),
		filepath.Join(localcaDir, "issuercert.pem"),
		filepath.Join(localcaDir, "certserial"),
	)
	if err := os.WriteFile(localcaConf, []byte(localcaContent), 0o644); err != nil {
		return "", err
	}

	localcaOpts := filepath.Join(instanceDir, "swtpm-localca.options")
	optsContent := "--platform-manufacturer NVIDIA\n--platform-version 2.1\n--platform-model nico-sim\n"
	if err := os.WriteFile(localcaOpts, []byte(optsContent), 0o644); err != nil {
		return "", err
	}

	setupConf := filepath.Join(instanceDir, "swtpm_setup.conf")
	setupContent := fmt.Sprintf("create_certs_tool = %s\ncreate_certs_tool_config = %s\ncreate_certs_tool_options = %s\n",
		localca, localcaConf, localcaOpts,
	)
	if err := os.WriteFile(setupConf, []byte(setupContent), 0o644); err != nil {
		return "", err
	}
	return setupConf, nil
}

// startSwtpm launches swtpm as a self-daemonizing process bound to the
// instance's ctrl socket, and waits for the socket to appear so QEMU's
// eager -chardev connect (verified: it fails immediately against a missing
// socket) does not race it. swtpm writes its own PID file natively.
//
// If a prior call already got a daemon running — for example CreateAndStart
// crashing after swtpm started but before the instance's MachineID was
// persisted — this reuses it rather than blindly removing its PID file and
// ctrl socket and starting a second daemon against the same tpmstate, which
// would leave the first one live but unreachable.
func startSwtpm(ctx context.Context, swtpmBin string, paths swtpmPaths) error {
	if pid, err := readPIDFile(paths.PIDFile); err == nil {
		if alive, _ := isPIDAlive(pid); alive {
			if _, statErr := os.Stat(paths.CtrlSock); statErr == nil {
				return nil
			}
			// Alive but unreachable (its ctrl socket is gone): stop it
			// before starting a fresh one so we don't end up with two
			// daemons on the same tpmstate.
			if stopErr := stopPIDFileProcess(paths.PIDFile); stopErr != nil {
				return fmt.Errorf("stop orphaned swtpm (pid %d) before restart: %w", pid, stopErr)
			}
		}
	}

	_ = os.Remove(paths.CtrlSock)
	_ = os.Remove(paths.PIDFile)

	cmd := exec.CommandContext(ctx, swtpmBin, "socket",
		"--tpm2",
		"--tpmstate", "dir="+paths.StateDir,
		"--ctrl", "type=unixio,path="+paths.CtrlSock,
		"--pid", "file="+paths.PIDFile,
		"--log", "file="+paths.LogPath+",level=1",
		"--daemon",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("swtpm socket failed to start: %w: %s", err, string(output))
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(paths.CtrlSock); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Readiness never arrived: unwind the daemon we just started rather
	// than leaving an unreachable process behind for the next attempt to
	// collide with.
	_ = stopSwtpm(paths)
	return fmt.Errorf("swtpm ctrl socket %s did not appear within 10s", paths.CtrlSock)
}

// stopSwtpm stops a running swtpm process using its own PID file, and
// returns an error if it could not be confirmed dead. Callers must invoke
// this only after QEMU has exited: swtpm is deliberately not in
// QEMU/vmnet-run's process group (it must survive a vmnet-run teardown), so
// it needs its own, separate shutdown. The PID file and ctrl socket are
// removed only once termination is confirmed, so a caller (Delete) never
// reports success — and lets the state dir be reclaimed — while a live
// daemon is still holding it open.
func stopSwtpm(paths swtpmPaths) error {
	if err := stopPIDFileProcess(paths.PIDFile); err != nil {
		return fmt.Errorf("stop swtpm (pidfile %s): %w", paths.PIDFile, err)
	}
	_ = os.Remove(paths.PIDFile)
	_ = os.Remove(paths.CtrlSock)
	return nil
}

// tpmQEMUArgs wires the swtpm ctrl socket up as a QEMU TPM device. The
// device name differs by architecture: aarch64 uses tpm-tis-device, x86_64
// uses tpm-tis (libvirt's domain_xml.go makes the same distinction). On
// HVF+aarch64, QEMU 11's PPI RAM region cannot be mapped by HVF, so ppi=off
// is appended — but only when the running QEMU's tpm-tis-device actually
// exposes that property, so older QEMU builds are not broken by an unknown
// property.
func tpmQEMUArgs(ctx context.Context, qemu string, paths swtpmPaths) []string {
	device := "tpm-tis"
	if goruntime.GOARCH == "arm64" {
		device = "tpm-tis-device"
		if tpmTisHasPPI(ctx, qemu) {
			device += ",ppi=off"
		}
	}
	return []string{
		"-chardev", "socket,id=chrtpm,path=" + paths.CtrlSock,
		"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
		"-device", device + ",tpmdev=tpm0",
	}
}

func tpmTisHasPPI(ctx context.Context, qemu string) bool {
	out, err := exec.CommandContext(ctx, qemu, "-device", "tpm-tis-device,help").CombinedOutput()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ppi=") {
			return true
		}
	}
	return false
}
