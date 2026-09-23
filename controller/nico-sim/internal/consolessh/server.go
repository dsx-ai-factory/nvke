// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package consolessh provides an SSH server that streams VM console logs.
// Clients connect with the instance ID as the SSH username; the server
// locates the corresponding console log file on disk and sends its contents
// back over the SSH channel.
package consolessh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// maxLogBackups is the number of rotated log files virtlogd keeps (matches
// the max_backups setting in virtlogd.conf).
const maxLogBackups = 3

// consoleLogNames are the known console log file names produced by each VM backend.
var consoleLogNames = []string{
	"console.log",          // darwin-qemu
	"disk.qcow2.ttyS0.log", // libvirt serial 0
	"disk.qcow2.ttyS1.log", // libvirt serial 1
}

// Server is an SSH server that streams VM console logs to connecting clients.
type Server struct {
	instancesDir string
	logger       *slog.Logger
	listener     net.Listener
	cancel       context.CancelFunc // cancels all active connections on shutdown
	wg           sync.WaitGroup
}

// New creates a new console SSH server. instancesDir is the directory
// containing per-instance subdirectories with console log files.
func New(instancesDir string, logger *slog.Logger) *Server {
	return &Server{
		instancesDir: instancesDir,
		logger:       logger.With("component", "console-ssh"),
	}
}

// Start begins accepting SSH connections on the given address. Use ":0" for
// an OS-assigned port. Returns the actual port the server is listening on.
func (s *Server) Start(ctx context.Context, addr string) (int, error) {
	signer, err := generateHostKey()
	if err != nil {
		return 0, fmt.Errorf("generate host key: %w", err)
	}

	sshCfg := &ssh.ServerConfig{
		NoClientAuth: true, //nolint:gosec // nico-sim is a local dev/test tool
	}
	sshCfg.AddHostKey(signer)

	// Use SO_REUSEADDR so a new process can bind to the port even when
	// connections from the previous process are still in TIME_WAIT.
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			})
		},
	}
	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("listen %s: %w", addr, err)
	}
	s.listener = listener
	port := listener.Addr().(*net.TCPAddr).Port

	// connCtx is cancelled on shutdown, which tears down active SSH
	// sessions (streamConsoleLogFollow) so the connections close promptly
	// instead of lingering until the remote client disconnects.
	connCtx, connCancel := context.WithCancel(context.Background())
	s.cancel = connCancel

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()
		connCancel()
		listener.Close()
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.acceptLoop(connCtx, sshCfg)
	}()

	s.logger.Info("Console SSH server started", "listenAddr", listener.Addr().String())
	return port, nil
}

// Wait blocks until all server goroutines have finished.
func (s *Server) Wait() {
	s.wg.Wait()
}

func (s *Server) acceptLoop(ctx context.Context, sshCfg *ssh.ServerConfig) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handleConnection(ctx, conn, sshCfg)
	}
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn, sshCfg *ssh.ServerConfig) {
	defer conn.Close()

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, sshCfg)
	if err != nil {
		s.logger.Debug("SSH handshake failed", "remoteAddr", conn.RemoteAddr(), "error", err)
		return
	}
	defer sshConn.Close()

	// Cancel when the SSH connection closes OR the server shuts down,
	// so streaming goroutines stop promptly in either case.
	connCtx, connCancel := context.WithCancel(ctx)
	go func() {
		sshConn.Wait()
		connCancel()
	}()

	instanceID := sshConn.User()
	s.logger.Info("Console log request", "instanceID", instanceID, "remoteAddr", conn.RemoteAddr())

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only session channels supported")
			continue
		}

		channel, requests, err := newChan.Accept()
		if err != nil {
			continue
		}

		go s.handleSession(connCtx, channel, requests, instanceID)
	}
}

func (s *Server) handleSession(connCtx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, instanceID string) {
	defer channel.Close()

	for req := range requests {
		switch req.Type {
		case "exec", "shell":
			if req.WantReply {
				req.Reply(true, nil)
			}
			// Default is follow (tail -f) mode. Client sends "dump" for one-shot.
			dump := false
			if req.Type == "exec" && len(req.Payload) >= 4 {
				// SSH exec payload is a uint32 length-prefixed string.
				cmdLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 | int(req.Payload[2])<<8 | int(req.Payload[3])
				if cmdLen <= len(req.Payload)-4 {
					cmd := string(req.Payload[4 : 4+cmdLen])
					dump = cmd == "dump"
				}
			}
			if dump {
				s.streamConsoleLog(channel, instanceID)
			} else {
				s.streamConsoleLogFollow(connCtx, channel, instanceID)
			}
			// Send exit-status 0.
			channel.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (s *Server) streamConsoleLog(w io.Writer, instanceID string) {
	logPath := s.findConsoleLog(instanceID)
	if logPath == "" {
		fmt.Fprintf(w, "no console log found for instance %s\n", instanceID)
		return
	}

	// Read rotated log files first (oldest to newest: .log.2, .log.1, .log.0)
	// then the current log file. virtlogd rotates at max_size and keeps
	// max_backups rotated copies.
	for i := maxLogBackups - 1; i >= 0; i-- {
		rotated := fmt.Sprintf("%s.%d", logPath, i)
		if rf, err := os.Open(rotated); err == nil {
			io.Copy(w, rf)
			rf.Close()
		}
	}

	f, err := os.Open(logPath)
	if err != nil {
		fmt.Fprintf(w, "failed to open console log: %v\n", err)
		return
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		s.logger.Debug("Error streaming console log", "instanceID", instanceID, "error", err)
	}
}

// streamConsoleLogFollow tails a console log file, streaming new data as it
// appears until the client disconnects or the context is cancelled.
func (s *Server) streamConsoleLogFollow(ctx context.Context, w io.Writer, instanceID string) {
	logPath := s.findConsoleLog(instanceID)
	if logPath == "" {
		fmt.Fprintf(w, "no console log found for instance %s\n", instanceID)
		return
	}

	f, err := os.Open(logPath)
	if err != nil {
		fmt.Fprintf(w, "failed to open console log: %v\n", err)
		return
	}
	defer f.Close()

	var totalRead, totalWritten int64
	lastLog := time.Now()
	buf := make([]byte, 4096)
	for {
		nr, err := f.Read(buf)
		if nr > 0 {
			totalRead += int64(nr)
			if _, werr := w.Write(buf[:nr]); werr != nil {
				s.logger.Debug("Console stream client disconnected",
					"instanceID", instanceID,
					"totalRead", totalRead,
					"totalWritten", totalWritten,
				)
				return // client disconnected
			}
			totalWritten += int64(nr)
		}

		// Periodic progress log — every 10s during active streaming,
		// or every 30s during idle (EOF) polling.
		now := time.Now()
		if nr > 0 && now.Sub(lastLog) >= 10*time.Second ||
			nr == 0 && now.Sub(lastLog) >= 30*time.Second {
			offset, _ := f.Seek(0, io.SeekCurrent)
			fileInfo, _ := os.Stat(logPath)
			fileSize := int64(0)
			if fileInfo != nil {
				fileSize = fileInfo.Size()
			}
			s.logger.Debug("Console stream progress",
				"instanceID", instanceID,
				"fileOffset", offset,
				"fileSize", fileSize,
				"totalRead", totalRead,
				"totalWritten", totalWritten,
				"phase", map[bool]string{true: "streaming", false: "polling"}[nr > 0],
			)
			lastLog = now
		}

		if err == io.EOF {
			// Detect file truncation (e.g. QEMU chardev 2MiB rotation):
			// if the file shrank below our read offset, seek back to the start.
			offset, _ := f.Seek(0, io.SeekCurrent)
			if info, serr := os.Stat(logPath); serr == nil && info.Size() < offset {
				s.logger.Info("Console log truncated, seeking to start",
					"instanceID", instanceID,
					"fileOffset", offset,
					"fileSize", info.Size(),
				)
				f.Seek(0, io.SeekStart)
			}
			select {
			case <-ctx.Done():
				s.logger.Debug("Console stream client disconnected (context)",
					"instanceID", instanceID,
					"totalRead", totalRead,
					"totalWritten", totalWritten,
				)
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			s.logger.Debug("Error streaming console log", "instanceID", instanceID, "error", err)
			return
		}
	}
}

func (s *Server) findConsoleLog(instanceID string) string {
	instanceDir := filepath.Join(s.instancesDir, instanceID)
	var bestPath string
	var bestModTime time.Time
	for _, name := range consoleLogNames {
		path := filepath.Join(instanceDir, name)
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			if bestPath == "" || info.ModTime().After(bestModTime) {
				bestPath = path
				bestModTime = info.ModTime()
			}
		}
	}
	return bestPath
}

func generateHostKey() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}
