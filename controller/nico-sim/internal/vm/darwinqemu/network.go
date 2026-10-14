// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package darwinqemu

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/netutil"
)

// vmnetNetwork owns the helper that keeps the host gateway present before any
// VM exists. Instances use these same explicit network arguments.
type vmnetNetwork struct {
	*ownedProcess
	args []string
	cidr string
	dir  string
}

func networkArgs(cidr string) ([]string, error) {
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("vmnet subnet: %w", err)
	}
	ip := subnet.IP.To4()
	bits, _ := subnet.Mask.Size()
	if ip == nil || !ip.IsPrivate() || bits < 16 || bits > 30 {
		return nil, fmt.Errorf("vmnet requires a private IPv4 subnet with prefix /16 through /30")
	}
	gw, err := netutil.GatewayIP(subnet.String())
	if err != nil {
		return nil, err
	}
	last := binary.BigEndian.Uint32(ip) | ^binary.BigEndian.Uint32(subnet.Mask)
	end := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(end, last-1)
	return []string{"--start-address", gw, "--end-address", end.String(), "--subnet-mask", net.IP(subnet.Mask).String()}, nil
}

func (r *darwinQEMU) ensureNetwork(ctx context.Context, cidr string) error {
	if ctx == nil {
		return fmt.Errorf("vmnet network requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	args, err := networkArgs(cidr)
	if err != nil {
		return err
	}
	r.networkMu.Lock()
	defer r.networkMu.Unlock()
	if r.network != nil {
		if r.network.cidr != cidr {
			return fmt.Errorf("vmnet network is already owned for %q", r.network.cidr)
		}
		select {
		case <-r.network.done:
			return fmt.Errorf("owned vmnet helper exited unexpectedly: %v", r.network.err)
		default:
			return nil
		}
	}
	dir, err := os.MkdirTemp("", "nvke-vmnet-")
	if err != nil {
		return err
	}
	network, err := startNetwork(ctx, filepath.Join(filepath.Dir(r.vmnetClientPath), "vmnet-helper"), dir, cidr, args)
	if err != nil {
		// The helper owns and removes its socket/lock. The empty private root
		// is removed only after its command is joined.
		_ = os.Remove(dir)
		return err
	}
	r.network = network
	return nil
}

func startNetwork(ctx context.Context, helper, dir, cidr string, args []string) (*vmnetNetwork, error) {
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	defer childStdout.Close()
	log, err := os.OpenFile(filepath.Join(dir, "helper.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, helper, append([]string{"--operation-mode", "shared", "--socket", filepath.Join(dir, "vmnet.sock")}, args...)...)
	command.Stdout, command.Stderr = childStdout, log
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 5 * time.Second
	process, err := startOwnedProcess(command, log)
	if err != nil {
		return nil, fmt.Errorf("start vmnet network helper: %w", err)
	}
	childStdout.Close()
	network := &vmnetNetwork{ownedProcess: process, args: args, cidr: cidr, dir: dir}
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var observed struct {
			Start string `json:"vmnet_start_address"`
			Mask  string `json:"vmnet_subnet_mask"`
		}
		err := json.NewDecoder(io.LimitReader(stdout, 4096)).Decode(&observed)
		if err == nil {
			_, subnet, parseErr := net.ParseCIDR(cidr)
			gw, gatewayErr := netutil.GatewayIP(cidr)
			if parseErr != nil || gatewayErr != nil || observed.Start != gw || observed.Mask != net.IP(subnet.Mask).String() {
				err = fmt.Errorf("vmnet helper network differs from the declared subnet")
			}
		}
		result <- err
	}()
	select {
	case err = <-result:
	case <-startup.Done():
		err = startup.Err()
		stdout.Close()
		<-result
	}
	if err != nil {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		return nil, errors.Join(err, network.stop(cleanup))
	}
	return network, nil
}

// TeardownNetwork stops and joins only the helper created by this runtime.
// It never accepts a PID file or an externally supplied process description.
func (r *darwinQEMU) TeardownNetwork(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("native network teardown requires a context")
	}
	r.networkMu.Lock()
	defer r.networkMu.Unlock()
	var failures []error
	if err := ctx.Err(); err != nil {
		failures = append(failures, err)
	}
	for id, process := range r.machines {
		if err := process.stop(ctx); err != nil {
			failures = append(failures, fmt.Errorf("join owned VM %s: %w", id, err))
			continue
		}
		delete(r.machines, id)
	}
	for id, tpm := range r.tpms {
		if err := tpm.process.stop(ctx); err != nil {
			failures = append(failures, fmt.Errorf("join owned TPM %s: %w", id, err))
			continue
		}
		delete(r.tpms, id)
	}
	// Guest processes depend on this helper. Failed joins retain its live
	// ownership rather than tearing down networking beneath possible producers.
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	if r.network == nil {
		return errors.Join(failures...)
	}
	if err := r.network.stop(ctx); err != nil {
		return errors.Join(append(failures, err)...)
	}
	r.network = nil
	return errors.Join(failures...)
}
