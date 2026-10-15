// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"net"
	"net/url"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	forgesdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
)

const (
	sshUser     = "testuser"
	sshPassword = "testpass"

	// PhoneHomeYAML is a cloud-config snippet that makes cloud-init POST to
	// the metadata server when all modules finish. Append to userData when
	// PhoneHomeEnabled is set on the create request.
	PhoneHomeYAML = "\nphone_home:\n    post: all\n    tries: 10\n    url: http://169.254.169.254:7777/latest/meta-data/phone_home\n"
)

// TestSSHConfig returns an ssh.ClientConfig suitable for integration tests.
// VMs must be created with RebootFlowUserData for this to work.
func TestSSHConfig() *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.Password(sshPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // integration test only
		Timeout:         5 * time.Second,
	}
}

// SSHRun dials the host on port 22, executes cmd, and returns combined output.
func SSHRun(host string, cfg *ssh.ClientConfig, cmd string) (string, error) {
	client, err := ssh.Dial("tcp", net.JoinHostPort(host, "22"), cfg)
	if err != nil {
		return "", err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	out, err := session.CombinedOutput(cmd)
	return string(out), err
}

// WaitForSSHCommand retries cmd over SSH until it succeeds or the timeout
// elapses. This is necessary because the readiness probe only checks TCP
// port 22; cloud-init may still be configuring the user when sshd accepts
// connections.
func WaitForSSHCommand(t *testing.T, host string, cfg *ssh.ClientConfig, cmd string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := SSHRun(host, cfg, cmd)
		if err == nil {
			return out
		}
		t.Logf("SSH command %q not ready yet: %v, retrying...", cmd, err)
		time.Sleep(3 * time.Second)
	}

	t.Fatalf("SSH command %q on %s did not succeed within %s", cmd, host, timeout)
	return ""
}

// InstanceIP extracts the first IP address from a ready instance's first interface.
func InstanceIP(t *testing.T, inst forgesdk.Instance) string {
	t.Helper()
	ifaces := inst.GetInterfaces()
	if len(ifaces) == 0 || len(ifaces[0].GetIpAddresses()) == 0 {
		t.Fatalf("instance %s has no IP address", inst.GetId())
	}
	return ifaces[0].GetIpAddresses()[0]
}

// RebootFlowUserData returns a cloud-config that sets up password-based SSH
// access and a boot counter. bootcmd runs on every boot, so
// /var/tmp/boot-count accumulates one "boot" line per kernel start.
func RebootFlowUserData(hostname string) string {
	return "#cloud-config\n" +
		"hostname: " + hostname + "\n" +
		"manage_etc_hosts: true\n" +
		"ssh_pwauth: true\n" +
		"users:\n" +
		"  - name: " + sshUser + "\n" +
		"    plain_text_passwd: " + sshPassword + "\n" +
		"    lock_passwd: false\n" +
		"    sudo: ALL=(ALL) NOPASSWD:ALL\n" +
		"    shell: /bin/bash\n" +
		"bootcmd:\n" +
		"  - [\"sh\", \"-c\", \"echo boot >> /var/tmp/boot-count\"]\n"
}

// connectivityFlowUserData returns a cloud-config that sets up password-based
// SSH access and starts a one-shot netcat listener on the given port. The
// listener writes whatever it receives to outFile and exits. It is started
// via runcmd so it is already listening by the time the readiness probe passes.
func connectivityFlowUserData(hostname, port, outFile string) string {
	return "#cloud-config\n" +
		"hostname: " + hostname + "\n" +
		"manage_etc_hosts: true\n" +
		"ssh_pwauth: true\n" +
		"users:\n" +
		"  - name: " + sshUser + "\n" +
		"    plain_text_passwd: " + sshPassword + "\n" +
		"    lock_passwd: false\n" +
		"    sudo: ALL=(ALL) NOPASSWD:ALL\n" +
		"    shell: /bin/bash\n" +
		"runcmd:\n" +
		"  - [\"sh\", \"-c\", \"nc -l -p " + port + " -w 120 > " + outFile + " 2>/dev/null &\"]\n"
}

// FetchConsoleLogViaSSH connects to the SSH URL from SerialConsoleUrl and
// returns the console log content. The URL format is ssh://instanceID@host:port.
func FetchConsoleLogViaSSH(t *testing.T, consoleURL string) string {
	t.Helper()

	u, err := url.Parse(consoleURL)
	if err != nil {
		t.Fatalf("parse console URL %q: %v", consoleURL, err)
	}

	cfg := &ssh.ClientConfig{
		User:            u.User.Username(),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // integration test only
		Timeout:         10 * time.Second,
	}

	client, err := ssh.Dial("tcp", u.Host, cfg)
	if err != nil {
		t.Fatalf("SSH dial %s: %v", u.Host, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("SSH session: %v", err)
	}
	defer session.Close()

	output, err := session.CombinedOutput("dump")
	if err != nil {
		t.Fatalf("SSH command: %v", err)
	}

	return string(output)
}
