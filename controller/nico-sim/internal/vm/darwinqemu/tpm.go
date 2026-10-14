// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package darwinqemu

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var errNativeTPMUnavailable = errors.New("native software TPM is not installed")

// nativeTPM binds the certificate produced by swtpm_setup to the exact state
// directory served to QEMU. It is retained only by the runtime that started it.
type nativeTPM struct {
	process *ownedProcess
	socket  string
	cert    string
}

func prepareNativeTPM(ctx context.Context, work string) (*nativeTPM, error) {
	setup, err := exec.LookPath("swtpm_setup")
	if err != nil {
		return nil, errNativeTPMUnavailable
	}
	emulator, err := exec.LookPath("swtpm")
	if err != nil {
		return nil, fmt.Errorf("TPM setup is available but its emulator is missing: %w", err)
	}
	localca, err := exec.LookPath("swtpm_localca")
	if err != nil {
		return nil, fmt.Errorf("TPM certificate tool is missing: %w", err)
	}
	state := filepath.Join(work, "tpm-state")
	if err := os.Mkdir(state, 0o700); err != nil {
		return nil, fmt.Errorf("create fresh TPM state: %w", err)
	}
	ca := filepath.Join(work, "tpm-ca")
	if err := os.Mkdir(ca, 0o700); err != nil {
		return nil, err
	}
	certs := filepath.Join(work, "tpm-certs")
	if err := os.Mkdir(certs, 0o700); err != nil {
		return nil, err
	}
	localcaConfig := filepath.Join(work, "swtpm-localca.conf")
	caBytes := fmt.Sprintf("statedir = %s\nsigningkey = %s\nissuercert = %s\ncertserial = %s\n", ca,
		filepath.Join(ca, "signkey.pem"), filepath.Join(ca, "issuercert.pem"), filepath.Join(ca, "serial"))
	options := filepath.Join(work, "swtpm-localca.options")
	setupConfig := filepath.Join(work, "swtpm-setup.conf")
	for path, content := range map[string]string{
		localcaConfig: caBytes,
		options:       "--platform-manufacturer NVKE\n--platform-version diagnostic\n--platform-model QEMU\n",
		setupConfig:   fmt.Sprintf("create_certs_tool=%s\ncreate_certs_tool_config=%s\ncreate_certs_tool_options=%s\n", localca, localcaConfig, options),
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return nil, err
		}
	}
	setupCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(setupCtx, setup, "--tpm2", "--tpmstate", state, "--config", setupConfig,
		"--create-ek-cert", "--create-platform-cert", "--write-ek-cert-files", certs)
	command.WaitDelay = 5 * time.Second
	if out, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("prepare native TPM certificate: %w: %s", err, truncateForDebug(string(out), 1000))
	}
	der, err := os.ReadFile(filepath.Join(certs, "ek-rsa2048.crt"))
	if err != nil {
		return nil, fmt.Errorf("read prepared TPM EK certificate: %w", err)
	}
	if _, err := x509.ParseCertificate(der); err != nil {
		return nil, fmt.Errorf("prepared TPM EK certificate is invalid: %w", err)
	}
	// A short private socket root keeps the Unix path within Darwin's limit.
	sockets, err := os.MkdirTemp("", "nvke-tpm-")
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(sockets, "control.sock")
	log, err := os.OpenFile(filepath.Join(work, "swtpm.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	command = exec.Command(emulator, "socket", "--tpm2", "--tpmstate", "dir="+state,
		"--ctrl", "type=unixio,path="+socket+",mode=0600", "--flags", "not-need-init")
	command.Stdout, command.Stderr = log, log
	process, err := startOwnedProcess(command, log)
	if err != nil {
		return nil, fmt.Errorf("start native TPM: %w", err)
	}
	ready, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	for {
		if info, err := os.Stat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			return &nativeTPM{process: process, socket: socket, cert: base64.StdEncoding.EncodeToString(der)}, nil
		}
		select {
		case <-process.done:
			return nil, fmt.Errorf("native TPM exited before readiness: %v", process.err)
		case <-ready.Done():
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer done()
			_ = process.stop(cleanup)
			return nil, ready.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (t *nativeTPM) qemuArgs(qemuPath string) []string {
	device := "tpm-tis"
	if filepath.Base(qemuPath) == "qemu-system-aarch64" {
		device = "tpm-tis-device"
	}
	return []string{
		"-chardev", "socket,id=nvke-tpm,path=" + t.socket,
		"-tpmdev", "emulator,id=nvke-tpm,chardev=nvke-tpm", "-device", device + ",tpmdev=nvke-tpm",
	}
}
