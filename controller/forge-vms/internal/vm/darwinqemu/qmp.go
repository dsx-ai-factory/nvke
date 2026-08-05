// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package darwinqemu

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// QMPClient communicates with a QEMU instance over the QMP (QEMU Machine
// Protocol) unix socket. It is NOT safe for concurrent use.
type QMPClient struct {
	socketPath string
	conn       net.Conn
}

// qmpResponse is the top-level shape of every QMP message. Exactly one of
// the pointer fields is non-nil per message.
type qmpResponse struct {
	Return json.RawMessage        `json:"return,omitempty"`
	Error  *qmpError              `json:"error,omitempty"`
	Event  string                 `json:"event,omitempty"`
	QMP    map[string]interface{} `json:"QMP,omitempty"`
}

type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

// VMStatus is the parsed result of the QMP query-status command.
type VMStatus struct {
	Running    bool   `json:"running"`
	Singlestep bool   `json:"singlestep"`
	Status     string `json:"status"`
}

// NewQMPClient returns a client for the given QMP unix socket path.
// Call Connect to establish the connection and perform the QMP handshake.
func NewQMPClient(socketPath string) *QMPClient {
	return &QMPClient{socketPath: socketPath}
}

// Connect dials the QMP unix socket, reads the server greeting, and
// negotiates capabilities. After a successful Connect the client is
// ready to accept commands.
func (c *QMPClient) Connect() error {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("qmp dial %s: %w", c.socketPath, err)
	}
	c.conn = conn

	if _, err := c.readMessage(5 * time.Second); err != nil {
		c.conn.Close()
		c.conn = nil
		return fmt.Errorf("qmp greeting: %w", err)
	}

	if _, err := c.sendCommand("qmp_capabilities"); err != nil {
		c.conn.Close()
		c.conn = nil
		return fmt.Errorf("qmp capabilities negotiation: %w", err)
	}

	return nil
}

// Close closes the underlying connection. It is safe to call on a nil
// or already-closed client.
func (c *QMPClient) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// readMessage reads a single QMP JSON message from the socket.
func (c *QMPClient) readMessage(timeout time.Duration) (*qmpResponse, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		n, err := c.conn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		}
		var resp qmpResponse
		if jsonErr := json.Unmarshal(buf[:n], &resp); jsonErr != nil {
			return nil, fmt.Errorf("qmp unmarshal: %w (raw: %s)", jsonErr, string(buf[:n]))
		}
		return &resp, nil
	}
	return nil, fmt.Errorf("timed out after %s", timeout)
}

// sendCommand sends a QMP execute command and blocks until the
// corresponding return (or error) response arrives. Interleaved async
// events are silently discarded.
func (c *QMPClient) sendCommand(command string) (json.RawMessage, error) {
	payload := fmt.Sprintf(`{"execute":"%s"}`, command)
	if _, err := c.conn.Write([]byte(payload + "\n")); err != nil {
		return nil, fmt.Errorf("qmp send %s: %w", command, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := c.readMessage(remaining)
		if err != nil {
			return nil, fmt.Errorf("qmp %s response: %w", command, err)
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("qmp %s error: %s — %s", command, msg.Error.Class, msg.Error.Desc)
		}
		if msg.Return != nil || (msg.Event == "" && msg.QMP == nil) {
			return msg.Return, nil
		}
	}
	return nil, fmt.Errorf("qmp %s: timed out waiting for response", command)
}

// waitEvent reads QMP messages until an event with the given name
// arrives or the timeout expires. Non-matching events are discarded.
func (c *QMPClient) waitEvent(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := c.readMessage(remaining)
		if err != nil {
			return err
		}
		if msg.Event == name {
			return nil
		}
	}
	return fmt.Errorf("timed out waiting for %s event after %s", name, timeout)
}

// --- Layer 2: Atomic QMP commands ---

// SystemPowerdown requests a guest ACPI shutdown.
func (c *QMPClient) SystemPowerdown() error {
	_, err := c.sendCommand("system_powerdown")
	return err
}

// SystemReset resets the guest (hard reset, QEMU process stays alive).
func (c *QMPClient) SystemReset() error {
	_, err := c.sendCommand("system_reset")
	return err
}

// Continue resumes guest execution after it has been paused.
func (c *QMPClient) Continue() error {
	_, err := c.sendCommand("cont")
	return err
}

// Stop pauses guest execution.
func (c *QMPClient) Stop() error {
	_, err := c.sendCommand("stop")
	return err
}

// Quit terminates the QEMU process.
func (c *QMPClient) Quit() error {
	_, err := c.sendCommand("quit")
	return err
}

// QueryStatus returns the current run state of the guest.
func (c *QMPClient) QueryStatus() (*VMStatus, error) {
	raw, err := c.sendCommand("query-status")
	if err != nil {
		return nil, err
	}
	var status VMStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, fmt.Errorf("qmp query-status unmarshal: %w", err)
	}
	return &status, nil
}

// --- Layer 3: Composite operations ---

// GracefulReboot performs an ACPI power-down, waits for the guest to
// reach the SHUTDOWN state, then issues a reset and resumes execution.
// This preserves the QEMU process while giving the guest a chance to
// cleanly unmount filesystems.
func (c *QMPClient) GracefulReboot(shutdownTimeout time.Duration) error {
	if err := c.SystemPowerdown(); err != nil {
		return fmt.Errorf("graceful reboot powerdown: %w", err)
	}
	if err := c.waitEvent("SHUTDOWN", shutdownTimeout); err != nil {
		return fmt.Errorf("graceful reboot waiting for shutdown: %w", err)
	}
	if err := c.SystemReset(); err != nil {
		return fmt.Errorf("graceful reboot reset: %w", err)
	}
	if err := c.Continue(); err != nil {
		return fmt.Errorf("graceful reboot continue: %w", err)
	}
	return nil
}

// GracefulShutdown sends an ACPI power-down and waits for the guest to
// reach the SHUTDOWN state. The QEMU process remains alive (with
// -no-shutdown) but the guest is stopped.
func (c *QMPClient) GracefulShutdown(timeout time.Duration) error {
	if err := c.SystemPowerdown(); err != nil {
		return fmt.Errorf("graceful shutdown powerdown: %w", err)
	}
	if err := c.waitEvent("SHUTDOWN", timeout); err != nil {
		return fmt.Errorf("graceful shutdown waiting for shutdown: %w", err)
	}
	return nil
}
