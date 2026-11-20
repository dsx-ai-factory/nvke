// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"sigs.k8s.io/yaml"
)

// validateImageDir is where the validation image's Dockerfile and its make
// target live.
const validateImageDir = "controller/test/e2e"

// validateJUnit is the report the image writes into its artifacts directory.
const validateJUnit = "junit-test-report.xml"

// validateJSON is the image's Ginkgo JSON report, beside validateJUnit.
const validateJSON = "report.json"

// imageInputs are the variables validate sets on the image itself, so the
// environment's own values of them are not passed through.
var imageInputs = []string{"NVKE_VALIDATE_PROFILE", "NVKE_CONTROLLER_EXPECTED_VERSION", "NVKE_MGMT_CLUSTER_KUBERNETES_VERSION"}

type validateOptions struct {
	runDir, teleportCluster, kubeconfig, kubeContext string
	profile                                          string
	controllerVersion, kubernetesVersion             string
	image, only, out                                 string
	full, dryRun                                     bool
}

func validateCmd() *cobra.Command {
	var o validateOptions
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Run the validation image against a management cluster",
		Long: `Run the validation image against a management cluster from this checkout.

Name the management cluster one of three ways:

  --run-dir           a management cluster kept by qualify local --keep-site. The node, its
                      key, its kubeconfig and both versions come from the run,
                      and the node is reached through its jump host.
  --teleport-cluster  a management cluster reached through Teleport; run tsh login first.
  --kubeconfig        a management cluster any kubeconfig reaches.

--profile names the management cluster's ValidationProfile, which says what
the management cluster is. It may set the controller and Kubernetes versions;
--controller-version and --kubernetes-version override them for one run, and
are required without a profile.

The image is built from this checkout unless --image names one. Without
--full the run creates one object, the DNS check's probe pod, and deletes it,
so the identity needs to create and delete pods in the probe namespace. --full
adds the specs labeled creates, which make other short-lived objects. Any NVKE_* or NKE_E2E_*
variable in the environment reaches the image as it is.

Reports, the run log and the inputs used (no credentials) go to --out: by
default validate-<time> under --run-dir, or under the user cache.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidate(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.runDir, "run-dir", "", "the --run-dir of a qualify local --keep-site run whose management cluster to validate")
	f.StringVar(&o.teleportCluster, "teleport-cluster", "", "the Teleport kube cluster to validate")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "a kubeconfig that reaches the management cluster to validate")
	f.StringVar(&o.kubeContext, "context", "", "the --kubeconfig context to use (default: its current one)")
	f.StringVar(&o.profile, "profile", "", "the management cluster's validation profile")
	f.StringVar(&o.controllerVersion, "controller-version", "", "the version the controller should report (default with --run-dir: the run's)")
	f.StringVar(&o.kubernetesVersion, "kubernetes-version", "", "the Kubernetes version the management cluster was built with (default with --run-dir: the run's)")
	f.StringVar(&o.image, "image", "", "the validation image to run (default: build one from this checkout)")
	f.BoolVar(&o.full, "full", false, "also run the specs that create short-lived objects on the management cluster")
	f.StringVar(&o.only, "only", "", "a Ginkgo label expression the run is narrowed to")
	f.StringVar(&o.out, "out", "", "where the run's files are kept")
	f.BoolVar(&o.dryRun, "dry-run", false, "list the specs the run would run, and run none")
	cmd.MarkFlagsMutuallyExclusive("run-dir", "teleport-cluster", "kubeconfig")
	cmd.MarkFlagsOneRequired("run-dir", "teleport-cluster", "kubeconfig")
	return cmd
}

// keptRecord is what qualify local writes beside a kept node's key: the
// suite's provision.Record.
type keptRecord struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	User       string `json:"user"`
	JumpHost   string `json:"jumpHost"`
	JumpLogin  string `json:"jumpLogin"`
	Key        string `json:"key"`
	Kubeconfig string `json:"kubeconfig"`
	HostKey    string `json:"hostKey"`
}

// keptSite reads the management cluster node a qualify local run kept, and the versions
// the run built it with.
func keptSite(runDir string) (keptRecord, map[string]string, error) {
	records, err := filepath.Glob(filepath.Join(runDir, "node-keys", "*.json"))
	if err != nil {
		return keptRecord{}, nil, err
	}
	if len(records) != 1 {
		return keptRecord{}, nil, fmt.Errorf("%s: want one kept management cluster record in node-keys, found %d; was the run kept with --keep-site?", runDir, len(records))
	}
	data, err := os.ReadFile(records[0])
	if err != nil {
		return keptRecord{}, nil, err
	}
	var r keptRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return keptRecord{}, nil, fmt.Errorf("%s: %w", records[0], err)
	}
	switch {
	case net.ParseIP(r.Address) == nil:
		return keptRecord{}, nil, fmt.Errorf("%s: address %q is not an IP", records[0], r.Address)
	case r.User == "", r.Key == "", r.Kubeconfig == "":
		return keptRecord{}, nil, fmt.Errorf("%s names no user, key or kubeconfig", records[0])
	case r.JumpHost == "":
		return keptRecord{}, nil, fmt.Errorf("%s names no jump host", records[0])
	}
	if r.HostKey == "" {
		return keptRecord{}, nil, fmt.Errorf("%s names no host key to hold the node to; it comes from a run that recorded none, so keep the management cluster again", records[0])
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.HostKey)); err != nil {
		return keptRecord{}, nil, fmt.Errorf("%s: hostKey: %w", records[0], err)
	}
	env, err := readEnvFile(filepath.Join(runDir, "steps.env"))
	if err != nil {
		return keptRecord{}, nil, err
	}
	versions := map[string]string{}
	for _, line := range env {
		name, value, _ := strings.Cut(line, "=")
		versions[name] = value
	}
	return r, versions, nil
}

func runValidate(ctx context.Context, out io.Writer, o validateOptions) error {
	if set := runSwitchesSet(os.Environ()); len(set) > 0 {
		return fmt.Errorf("%s set in the environment: the image sets these, and a run that changes one can check nothing and still pass; unset them",
			strings.Join(set, ", "))
	}
	if o.runDir == "" && o.teleportCluster == "" && o.kubeconfig == "" {
		return errors.New("--run-dir, --teleport-cluster or --kubeconfig must name something; an empty one would fall back to the default kubeconfig")
	}
	if o.kubeContext != "" && o.kubeconfig == "" {
		return errors.New("--context names a --kubeconfig context; --run-dir and --teleport-cluster use the kubeconfig they make")
	}
	tools := []string{"docker", "kubectl"}
	switch {
	case o.runDir != "", o.teleportCluster != "":
		tools = append(tools, "tsh")
	}
	if o.image == "" {
		tools = append(tools, "git", "make")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is not on PATH; the run needs it", tool)
		}
	}

	name := "mgmt-cluster"
	var kept keptRecord
	switch {
	case o.runDir != "":
		var versions map[string]string
		var err error
		if kept, versions, err = keptSite(o.runDir); err != nil {
			return err
		}
		name = kept.Name
		o.controllerVersion = cmp.Or(o.controllerVersion, versions["SITE_NVKE_CONTROLLER_VERSION"])
		o.kubernetesVersion = cmp.Or(o.kubernetesVersion, versions["KUBERNETES_VERSION"])
	case o.teleportCluster != "":
		name = o.teleportCluster
	case o.kubeconfig != "":
		name = strings.TrimSuffix(filepath.Base(o.kubeconfig), filepath.Ext(o.kubeconfig))
	}
	if o.profile == "" && (o.controllerVersion == "" || o.kubernetesVersion == "") {
		return errors.New("--controller-version and --kubernetes-version are required without a --profile that sets them: they are what the checks hold the management cluster to")
	}
	if o.profile != "" {
		var err error
		if o.profile, err = filepath.Abs(o.profile); err != nil {
			return err
		}
		if _, err := os.Stat(o.profile); err != nil {
			return fmt.Errorf("--profile: %w", err)
		}
	}

	stamp := time.Now().Format("20060102-150405")
	if o.out == "" {
		if o.runDir != "" {
			o.out = filepath.Join(o.runDir, "validate-"+stamp)
		} else {
			cache, err := os.UserCacheDir()
			if err != nil {
				return err
			}
			o.out = filepath.Join(cache, "nvke", "validate", name+"-"+stamp)
		}
	}
	var err error
	if o.out, err = filepath.Abs(o.out); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(o.out, "artifacts"), 0o700); err != nil {
		return err
	}
	// A reused --out keeps the last run's reports, which would be taken
	// for this run's if this one wrote none.
	for _, report := range []string{validateJUnit, validateJSON} {
		if err := os.Remove(filepath.Join(o.out, "artifacts", report)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	fmt.Fprintf(out, "run directory: %s\n", o.out)

	if err := waitForDocker(ctx); err != nil {
		return err
	}
	if o.image == "" {
		if o.image, err = buildValidateImage(ctx, out); err != nil {
			return err
		}
	}

	// The suite deletes what it made through the connection after an
	// interrupt reaches it, so once the suite starts the connection ends only
	// when the suite has exited. Until then an interrupt ends it too.
	connCtx, connStop := context.WithCancel(context.WithoutCancel(ctx))
	defer connStop()
	suiteStarted := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			connStop()
		case <-suiteStarted:
		case <-connCtx.Done():
		}
	}()
	source, stopped, err := connect(connCtx, o, kept)
	defer func() {
		connStop()
		stopped()
	}()
	if err != nil {
		return err
	}
	raw, err := kubectlFlatten(ctx, source, o.kubeContext)
	if err != nil {
		return err
	}
	inContainer, err := containerKubeconfig(raw, runtime.GOOS)
	if err != nil {
		return err
	}
	kubeconfig, err := writePrivateTemp(o.out, "container-*.kubeconfig", inContainer)
	if err != nil {
		return err
	}
	defer os.Remove(kubeconfig)

	filter := labelFilter(o.full, o.only, os.Getenv("NVKE_TENANT_VALIDATE") == "true")
	passed := passedEnv(os.Environ())
	platform, err := output(ctx, "docker", "image", "inspect", o.image, "--format", "{{.Os}}/{{.Architecture}}")
	if err != nil {
		return err
	}
	args := dockerArgs(o, platform, kubeconfig, filter, passed, runtime.GOOS)
	if err := writeInputs(o, name, filter, passed); err != nil {
		return err
	}

	fmt.Fprintf(out, "management cluster %s, image %s\nexpecting controller %s, Kubernetes %s\nfilter: %s\n", name, o.image,
		cmp.Or(o.controllerVersion, "from the profile"), cmp.Or(o.kubernetesVersion, "from the profile"), filter)
	logFile, err := os.OpenFile(filepath.Join(o.out, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	run := exec.CommandContext(ctx, "docker", args...)
	run.Stdout = io.MultiWriter(out, logFile)
	run.Stderr = run.Stdout
	// docker run passes the interrupt to the suite, which deletes what it
	// made before it exits; a kill would leave a probe pod on the management cluster. Its
	// own process group keeps a terminal's interrupt from reaching it as well,
	// since Ginkgo skips its cleanup on a second one.
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	run.Cancel = func() error { return run.Process.Signal(os.Interrupt) }
	run.WaitDelay = 2 * time.Minute
	close(suiteStarted)
	runErr := run.Run()

	passedChecks := 0
	if report, err := os.ReadFile(filepath.Join(o.out, "artifacts", validateJUnit)); err == nil {
		summary, n, serr := summarizeJUnit(report)
		if serr != nil {
			fmt.Fprintf(out, "reading %s: %v\n", validateJUnit, serr)
		} else {
			fmt.Fprint(out, summary)
			passedChecks = n
		}
	}
	fmt.Fprintf(out, "reports in %s\n", filepath.Join(o.out, "artifacts"))
	if runErr != nil {
		return fmt.Errorf("validation: %w", runErr)
	}
	// Ginkgo exits 0 when every selected spec skips, or when --only selects
	// none, which would read as a management cluster that passed.
	if !o.dryRun && passedChecks == 0 {
		return errors.New("validation: no check passed, so the run says nothing about the management cluster; see the skips above")
	}
	return nil
}

// runSwitches are the image's ENV that decide whether a run checks anything.
// A validation profile cannot set them either.
var runSwitches = []string{"NVKE_MGMT_CLUSTER_VALIDATE", "NVKE_E2E_REQUIRE_EXPECTED_VERSION", "NKE_E2E_AUTH_DISABLE"}

// runSwitchesSet is the runSwitches env sets, even to nothing: docker passes
// an empty value through, which turns the switch off.
func runSwitchesSet(env []string) []string {
	var set []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(runSwitches, name) {
			set = append(set, name)
		}
	}
	slices.Sort(set)
	return set
}

// waitForDocker waits out a Docker daemon waking from idle, which fails its
// first requests.
func waitForDocker(ctx context.Context) error {
	var err error
	for range 30 {
		if err = exec.CommandContext(ctx, "docker", "info").Run(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("the Docker daemon does not answer: %w", err)
}

// buildValidateImage builds the image from this checkout, for the host's
// architecture, tagged with the checkout's description.
func buildValidateImage(ctx context.Context, out io.Writer) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	describe, err := output(ctx, "git", "-C", root, "describe", "--tags", "--always", "--dirty", "--match", "v*")
	if err != nil {
		return "", err
	}
	if !dockerTag.MatchString(describe) {
		return "", fmt.Errorf("git describe gave %q, which is not a Docker tag; pass --image", describe)
	}
	image := "nvke-validate:" + describe
	fmt.Fprintf(out, "building %s\n", image)
	build := exec.CommandContext(ctx, "make", "-C", filepath.Join(root, validateImageDir), "validate-image",
		"VALIDATE_IMAGE="+image, "VALIDATE_PLATFORM=linux/"+runtime.GOARCH)
	build.Stdout, build.Stderr = out, out
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("building %s: %w", image, err)
	}
	return image, nil
}

// connect reaches the management cluster and returns a kubeconfig the host can use. What
// it starts in the background ends with ctx; the returned func waits for that
// after ctx is done.
func connect(ctx context.Context, o validateOptions, kept keptRecord) (string, func(), error) {
	switch {
	case o.runDir != "":
		port, err := kubeconfigPort(kept.Kubeconfig)
		if err != nil {
			return "", func() {}, err
		}
		return kept.Kubeconfig, func() {}, forwardThroughJumpHost(ctx, kept, port, filepath.Join(o.out, "connect.log"))
	case o.teleportCluster != "":
		port, err := freePort()
		if err != nil {
			return "", func() {}, err
		}
		logPath := filepath.Join(o.out, "connect.log")
		// A reused --out appends to the log, so read only what this proxy writes.
		var offset int64
		if info, err := os.Stat(logPath); err == nil {
			offset = info.Size()
		}
		proxy := exec.CommandContext(ctx, "tsh", "proxy", "kube", o.teleportCluster, "--port", strconv.Itoa(port), "--format=unix")
		// tsh writes its kubeconfig to TELEPORT_KUBECONFIG where that is set,
		// and removes it on exit, so a workstation's own is never the one used.
		tshConfig := filepath.Join(o.out, fmt.Sprintf("tsh-proxy-%d.kubeconfig", port))
		proxy.Env = append(os.Environ(), "TELEPORT_KUBECONFIG="+tshConfig)
		proxy.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Interrupted rather than killed, so tsh removes its kubeconfig; one
		// that outlasts WaitDelay is killed, and stopped removes it instead.
		proxy.Cancel = func() error { return syscall.Kill(-proxy.Process.Pid, syscall.SIGINT) }
		proxy.WaitDelay = 10 * time.Second
		exited, err := startLogged(proxy, logPath)
		if err != nil {
			return "", func() {}, err
		}
		stopped := func() {
			<-exited
			if err := os.Remove(tshConfig); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "removing %s: %v\n", tshConfig, err)
			}
		}
		if err := waitForPort(ctx, port, exited, o.out); err != nil {
			return "", stopped, err
		}
		// tsh prints the kubeconfig on stderr, and removes it when it exits.
		for range 10 {
			log, _ := os.ReadFile(logPath)
			if int64(len(log)) < offset {
				offset = 0
			}
			if path := tshKubeconfig(log[offset:]); path != "" {
				return path, stopped, nil
			}
			time.Sleep(time.Second)
		}
		return "", stopped, fmt.Errorf("tsh proxy kube printed no kubeconfig; see %s", logPath)
	default:
		return o.kubeconfig, func() {}, nil
	}
}

// startLogged starts cmd with its output in logPath, and returns a channel
// closed when it exits.
func startLogged(cmd *exec.Cmd, logPath string) (<-chan struct{}, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		// A process killed by a signal writes nothing of its own.
		fmt.Fprintf(log, "%s exited: %v\n", filepath.Base(cmd.Path), cmd.Wait())
		log.Close()
		close(exited)
	}()
	return exited, nil
}

func waitForPort(ctx context.Context, port int, exited <-chan struct{}, out string) error {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	logPath := filepath.Join(out, "connect.log")
	for range 30 {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			return fmt.Errorf("the connection to the management cluster exited; see %s", logPath)
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("nothing listened on %s; see %s", address, logPath)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// relayCommand is the suite's relay, run on the jump host: nc to the node's
// SSH port, or bash's /dev/tcp where the jump host has no nc. keptSite checks
// that the address is an IP, so it reaches the shell as nothing else.
func relayCommand(address string) string {
	const script = `if command -v nc >/dev/null 2>&1; then exec nc %[1]s 22; fi; ` +
		`exec bash -c 'exec 3<>/dev/tcp/%[1]s/22 && { cat <&3 & exec cat >&3; }'`
	return fmt.Sprintf(script, address)
}

// relayArgs are tsh's arguments to relay to the node through its jump host.
func relayArgs(kept keptRecord) []string {
	jump := kept.JumpHost
	if kept.JumpLogin != "" {
		jump = kept.JumpLogin + "@" + jump
	}
	return []string{"ssh", jump, relayCommand(kept.Address)}
}

// sshHandshakeTimeout bounds the SSH handshake with the node.
var sshHandshakeTimeout = time.Minute

// forwardThroughJumpHost forwards the kept management cluster's API server port to the
// host's loopback, reaching the node as the suite does: an SSH session with
// the run's key, carried by tsh through the jump host. It runs no ssh
// binary. What it starts ends with ctx.
func forwardThroughJumpHost(ctx context.Context, kept keptRecord, port int, logPath string) error {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	pem, err := os.ReadFile(kept.Key)
	if err != nil {
		log.Close()
		return err
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		log.Close()
		return fmt.Errorf("%s: %w", kept.Key, err)
	}

	relay := exec.CommandContext(ctx, "tsh", relayArgs(kept)...)
	relay.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	relay.Stderr = log
	stdin, err := relay.StdinPipe()
	if err != nil {
		log.Close()
		return err
	}
	stdout, err := relay.StdoutPipe()
	if err != nil {
		log.Close()
		return err
	}
	if err := relay.Start(); err != nil {
		log.Close()
		return err
	}
	go func() {
		fmt.Fprintf(log, "tsh exited: %v\n", relay.Wait())
		log.Close()
	}()

	nodeAddress := net.JoinHostPort(kept.Address, "22")
	conn, chans, reqs, err := sshHandshake(&pipeConn{Reader: stdout, WriteCloser: stdin}, nodeAddress, &ssh.ClientConfig{
		User: kept.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			return checkHostKey(kept.HostKey, key)
		},
	}, func() { _ = syscall.Kill(-relay.Process.Pid, syscall.SIGKILL) })
	if err != nil {
		return fmt.Errorf("ssh to %s@%s through its jump host: %w; see %s", kept.User, kept.Address, err, logPath)
	}
	client := ssh.NewClient(conn, chans, reqs)

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		client.Close()
		return fmt.Errorf("forwarding %s: %w", address, err)
	}
	go func() {
		<-ctx.Done()
		listener.Close()
		client.Close()
	}()
	go func() {
		for {
			local, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer local.Close()
				remote, err := client.Dial("tcp", address)
				if err != nil {
					return
				}
				defer remote.Close()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
				go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return nil
}

// sshHandshake runs the SSH handshake over conn, and calls stop to end conn
// once sshHandshakeTimeout passes without one. A relay's pipes have no
// deadlines, and a tsh waiting on an MFA prompt, or a relay to an address
// that never answers, would otherwise hang the run.
func sshHandshake(conn net.Conn, address string, cfg *ssh.ClientConfig, stop func()) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	timer := time.AfterFunc(sshHandshakeTimeout, stop)
	c, chans, reqs, err := ssh.NewClientConn(conn, address, cfg)
	if !timer.Stop() {
		if err == nil {
			c.Close()
		}
		return nil, nil, nil, fmt.Errorf("no handshake in %s", sshHandshakeTimeout)
	}
	return c, chans, reqs, err
}

// checkHostKey holds the node to the host key the run that kept it recorded.
func checkHostKey(recorded string, key ssh.PublicKey) error {
	want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(recorded))
	if err != nil {
		return err
	}
	if !bytes.Equal(key.Marshal(), want.Marshal()) {
		return fmt.Errorf("the node's host key is %s, not the %s its management cluster record holds", ssh.FingerprintSHA256(key), ssh.FingerprintSHA256(want))
	}
	return nil
}

// pipeConn is a relay's stdio as the net.Conn an SSH client runs over.
type pipeConn struct {
	io.Reader
	io.WriteCloser
}

func (*pipeConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (*pipeConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (*pipeConn) SetDeadline(time.Time) error      { return nil }
func (*pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (*pipeConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "tsh relay" }

func kubeconfigPort(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	cluster, err := currentCluster(cfg)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	server, _ := cluster["server"].(string)
	u, err := url.Parse(server)
	if err != nil || u.Port() == "" {
		return 0, fmt.Errorf("%s: server %q names no port", path, server)
	}
	return strconv.Atoi(u.Port())
}

// currentCluster is the cluster entry of the kubeconfig's current context.
func currentCluster(cfg map[string]any) (map[string]any, error) {
	current, _ := cfg["current-context"].(string)
	var clusterName string
	for _, c := range asList(cfg["contexts"]) {
		if c["name"] == current {
			entry, _ := c["context"].(map[string]any)
			clusterName, _ = entry["cluster"].(string)
		}
	}
	for _, c := range asList(cfg["clusters"]) {
		if c["name"] == clusterName {
			cluster, _ := c["cluster"].(map[string]any)
			return cluster, nil
		}
	}
	return nil, fmt.Errorf("no cluster for the current context %q", current)
}

func asList(v any) []map[string]any {
	items, _ := v.([]any)
	var out []map[string]any
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func tshKubeconfig(log []byte) string {
	for _, line := range strings.Split(string(log), "\n") {
		if path, ok := strings.CutPrefix(strings.TrimSpace(line), "export KUBECONFIG="); ok {
			return strings.Trim(path, `"'`)
		}
	}
	return ""
}

// kubectlFlatten is the kubeconfig's current, or named, context alone, with
// its file references inlined, as the container needs it.
func kubectlFlatten(ctx context.Context, path, kubeContext string) ([]byte, error) {
	args := []string{"--kubeconfig", path, "config", "view", "--raw", "--minify", "--flatten"}
	if kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl config view: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}

// containerKubeconfig points a flattened, minified kubeconfig at what the
// container can reach. Docker Desktop reaches the host's loopback as
// host.docker.internal; on Linux the container shares the host's network
// and needs no change. A loopback proxy-url, which tsh's local proxy uses, is
// rewritten; otherwise a loopback server is, with tls-server-name pinned to
// the original host so the certificate still matches.
func containerKubeconfig(raw []byte, goos string) ([]byte, error) {
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	for _, u := range asList(cfg["users"]) {
		if user, _ := u["user"].(map[string]any); user["exec"] != nil {
			return nil, errors.New("the kubeconfig authenticates with an exec plugin, which the image does not have; use --teleport-cluster, or a kubeconfig with certificates or a token")
		} else if user["tokenFile"] != nil {
			return nil, errors.New("the kubeconfig reads its token from a file, which the image does not have; embed the token with token: instead")
		}
	}
	if goos == "linux" {
		return raw, nil
	}
	cluster, err := currentCluster(cfg)
	if err != nil {
		return nil, err
	}
	if proxy, _ := cluster["proxy-url"].(string); proxy != "" {
		if u, err := url.Parse(proxy); err == nil && isLoopback(u.Hostname()) {
			u.Host = net.JoinHostPort("host.docker.internal", u.Port())
			if u.Port() == "" {
				u.Host = "host.docker.internal"
			}
			cluster["proxy-url"] = u.String()
		}
		return yaml.Marshal(cfg)
	}
	server, _ := cluster["server"].(string)
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("server %q: %w", server, err)
	}
	if isLoopback(u.Hostname()) {
		if pinned, _ := cluster["tls-server-name"].(string); pinned == "" {
			cluster["tls-server-name"] = u.Hostname()
		}
		u.Host = net.JoinHostPort("host.docker.internal", u.Port())
		cluster["server"] = u.String()
	}
	return yaml.Marshal(cfg)
}

func isLoopback(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host).IsLoopback()
}

// labelFilter is read-only by default: the specs labeled creates make
// short-lived objects on the management cluster.
func labelFilter(full bool, only string, tenant bool) string {
	filter := "mgmt-cluster-validate && !creates"
	if full {
		filter = "mgmt-cluster-validate"
	}
	if tenant {
		filter = strings.ReplaceAll(filter, "mgmt-cluster-validate", "(mgmt-cluster-validate || tenant-validate)")
	}
	if only != "" {
		filter = "(" + filter + ") && (" + only + ")"
	}
	return filter
}

// passedEnv is the names of the NVKE_* and NKE_E2E_* variables in env, which
// reach the image as they are, less those validate sets itself.
func passedEnv(env []string) []string {
	var names []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if (strings.HasPrefix(name, "NVKE_") || strings.HasPrefix(name, "NKE_E2E_")) && !slices.Contains(imageInputs, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func dockerArgs(o validateOptions, platform, kubeconfig, filter string, passed []string, goos string) []string {
	args := []string{"run", "--rm", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--platform", platform}
	// Docker Desktop reaches the host's loopback as host.docker.internal;
	// Linux reaches it only from the host's network.
	if goos == "linux" {
		args = append(args, "--network", "host")
	}
	for _, name := range passed {
		args = append(args, "-e", name)
	}
	args = append(args,
		"-v", filepath.Join(o.out, "artifacts")+":/artifacts",
		"-v", kubeconfig+":/kube/config:ro",
		"-e", "KUBECONFIG=/kube/config",
	)
	if o.controllerVersion != "" {
		args = append(args, "-e", "NVKE_CONTROLLER_EXPECTED_VERSION="+o.controllerVersion)
	}
	if o.kubernetesVersion != "" {
		args = append(args, "-e", "NVKE_MGMT_CLUSTER_KUBERNETES_VERSION="+o.kubernetesVersion)
	}
	if o.profile != "" {
		args = append(args, "-v", o.profile+":/profile.yaml:ro", "-e", "NVKE_VALIDATE_PROFILE=/profile.yaml")
	}
	args = append(args, o.image, "-ginkgo.label-filter="+filter)
	if o.dryRun {
		args = append(args, "-ginkgo.dry-run")
	}
	return args
}

func writeInputs(o validateOptions, name, filter string, passed []string) error {
	if o.profile != "" {
		data, err := os.ReadFile(o.profile)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(o.out, "profile.yaml"), data, 0o600); err != nil {
			return err
		}
	}
	inputs := fmt.Sprintf("MANAGEMENT_CLUSTER=%s\nRUN_DIR=%s\nTELEPORT_CLUSTER=%s\nKUBECONFIG=%s\nIMAGE=%s\nPROFILE=%s\nCONTROLLER_VERSION=%s\nKUBERNETES_VERSION=%s\nFILTER=%s\nDRY_RUN=%t\nPASSED_ENV=%s\n",
		name, o.runDir, o.teleportCluster, o.kubeconfig, o.image, o.profile, o.controllerVersion, o.kubernetesVersion, filter, o.dryRun, strings.Join(passed, " "))
	return os.WriteFile(filepath.Join(o.out, "inputs.env"), []byte(inputs), 0o600)
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(b)), nil
}

type junitReport struct {
	Suites []struct {
		Cases []junitCase `xml:"testcase"`
	} `xml:"testsuite"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
}

type junitCase struct {
	Name       string        `xml:"name,attr"`
	Failure    *junitMessage `xml:"failure"`
	Error      *junitMessage `xml:"error"`
	Skipped    *junitMessage `xml:"skipped"`
	Properties []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"properties>property"`
}

// notFailing marks the report entries a check records for what it found but
// does not fail on, such as what a scoped check found outside its scope.
const notFailing = "not failing the check"

// summarizeJUnit counts the specs, names the failed and skipped ones and any
// failed suite node, such as an [AfterSuite], and lists what the checks found
// but did not fail on. It returns the summary and how many specs passed.
func summarizeJUnit(report []byte) (string, int, error) {
	var r junitReport
	if err := xml.Unmarshal(report, &r); err != nil {
		return "", 0, err
	}
	var passed int
	var failed, suiteFailed, skipped, outside []string
	for _, suite := range r.Suites {
		for _, c := range suite.Cases {
			m := c.Failure
			if m == nil {
				m = c.Error
			}
			var first string
			if m != nil {
				first, _, _ = strings.Cut(m.Message, "\n")
			}
			if !strings.HasPrefix(c.Name, "[It]") {
				if m != nil {
					suiteFailed = append(suiteFailed, withMessage(c.Name, first))
				}
				continue
			}
			switch {
			case m != nil:
				failed = append(failed, withMessage(c.Name, first))
			case c.Skipped != nil:
				skipped = append(skipped, withMessage(c.Name, c.Skipped.Message))
			default:
				passed++
			}
			for _, p := range c.Properties {
				if strings.Contains(p.Name, notFailing) {
					value := p.Value
					var s string
					if json.Unmarshal([]byte(p.Value), &s) == nil {
						value = s
					}
					outside = append(outside, p.Name+":\n    "+strings.ReplaceAll(value, "\n", "\n    "))
				}
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%d passed, %d failed, %d skipped\n", passed, len(failed), len(skipped))
	for _, f := range suiteFailed {
		fmt.Fprintf(&b, "  FAILED  %s, which is the suite's, not a check's\n", f)
	}
	for _, f := range failed {
		fmt.Fprintf(&b, "  FAILED  %s\n", f)
	}
	for _, s := range skipped {
		fmt.Fprintf(&b, "  skipped %s\n", s)
	}
	for _, o := range outside {
		fmt.Fprintf(&b, "  %s\n", o)
	}
	return b.String(), passed, nil
}

func withMessage(name, message string) string {
	if message == "" {
		return name
	}
	return name + ": " + message
}

// dockerTag is what a Docker tag may hold, so a tag from git describe cannot
// reach the shell in the make recipe it is passed to.
var dockerTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// writePrivateTemp writes data to a new file in dir, readable only by the
// owner, and returns its path. A fixed name could overwrite an input, or keep
// an existing file's looser mode.
func writePrivateTemp(dir, pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
