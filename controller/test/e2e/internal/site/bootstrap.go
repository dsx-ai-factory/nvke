// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra"
)

const (
	defaultReadyTimeout = 10 * time.Minute
	defaultPollInterval = 10 * time.Second
)

// Deployer installs the NKE stack onto a freshly bootstrapped cluster, over
// the same session bootstrap used. It is an interface because what "the
// stack" is, and how it is installed, is decided outside this suite.
type Deployer interface {
	Deploy(ctx context.Context, s infra.Session, t DeployTarget) error
}

// DeployTarget is the cluster a Deployer installs onto, in node paths.
type DeployTarget struct {
	Node        infra.Node
	SiteName    string
	TrustDomain string

	// Kubeconfig is the cluster's admin kubeconfig on the node.
	Kubeconfig string

	// Assets is where the candidate's asset paths were unpacked.
	Assets string
}

// Prober reports what CWE on a site controller says about itself, through
// the given kubeconfig. It returns the /api/version body's version once
// /healthz answers.
type Prober func(ctx context.Context, kubeconfig string) (string, error)

// Options are the parts of Bootstrap a test replaces.
type Options struct {
	// Probe defaults to asking CWE through a port-forward.
	Probe Prober

	// OutputDir is where the forwarded kubeconfig is written. It defaults to
	// a new temporary directory.
	OutputDir string

	ReadyTimeout time.Duration
	PollInterval time.Duration
}

// Result is a bootstrapped, deployed site controller.
type Result struct {
	// Kubeconfig is a runner path to an admin kubeconfig that reaches the
	// cluster through the session's forward. It is valid until Teardown.
	Kubeconfig string

	// Version is what CWE reported.
	Version string
}

// Bootstrap builds a site controller on n: it stages the candidate onto the
// node, bootstraps Kubernetes with nkectl, deploys the NKE stack with the
// candidate's Deployer, and checks CWE answers with the expected version.
//
// A failure leaves the node as it is, for the caller to collect from before
// tearing the session down.
func Bootstrap(ctx context.Context, n infra.Node, s infra.Session, c Candidate, opts Options) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{}, err
	}
	if n.Name == "" || n.IP == "" {
		return Result{}, errors.New("site: node has no name or IP")
	}
	opts = opts.withDefaults()

	b := bootstrap{node: n, session: s, candidate: c}
	for _, step := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"stage nkectl", b.stageNkectl},
		{"stage assets", b.stageAssets},
		{"import images", b.importImages},
		{"generate site spec", b.generateSiteSpec},
		{"bootstrap site", b.bootstrapSite},
		{"deploy", b.deploy},
	} {
		if err := step.run(ctx); err != nil {
			return Result{}, fmt.Errorf("site %s: %s: %w", c.SiteName, step.name, err)
		}
	}

	kubeconfig, err := b.forwardKubeconfig(ctx, opts.OutputDir)
	if err != nil {
		return Result{}, fmt.Errorf("site %s: forward kubeconfig: %w", c.SiteName, err)
	}
	version, err := waitForCWE(ctx, opts, kubeconfig)
	if err != nil {
		return Result{}, fmt.Errorf("site %s: %w", c.SiteName, err)
	}
	if c.ControllerVersion != "" && version != c.ControllerVersion {
		return Result{}, fmt.Errorf("site %s: CWE reports version %q, want %q", c.SiteName, version, c.ControllerVersion)
	}
	return Result{Kubeconfig: kubeconfig, Version: version}, nil
}

func (o Options) withDefaults() Options {
	if o.Probe == nil {
		o.Probe = probeCWE
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = defaultReadyTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	return o
}

// TrustDomain is the SPIRE trust domain a site is bootstrapped with.
func TrustDomain(siteName string) string { return siteName + ".nke.local" }

type bootstrap struct {
	node      infra.Node
	session   infra.Session
	candidate Candidate
}

const (
	nkectlPath     = "/usr/local/bin/nkectl"
	assetsArchive  = workDir + "/assets.tar.gz"
	assetsDir      = workDir + "/assets"
	imagesDir      = workDir + "/images"
	hostsCSV       = workDir + "/hosts.csv"
	siteSpecPath   = workDir + "/sitespec.yaml"
	kubeconfigPath = workDir + "/site.kubeconfig"
	diagnosticsDir = workDir + "/diagnostics"

	// nodeRecordDir is what the node says about itself, as opposed to what
	// nkectl says about the cluster. captureNodeRecord fills it.
	nodeRecordDir = workDir + "/node"
)

const (
	// agentStateFile is the node agent's state on disk, mirroring StateFile
	// in agent/internal/agent. Duplicated rather than imported because that
	// package is internal to the agent module.
	//
	// It is the only place the whole of a failed kubeadm run survives: the
	// agent keeps the output in kubeadmErrorMessage, but
	// KubeadmStatusResponse has no field for it, so what reaches CWE is one
	// line picked out of it. Run 36908360256 reported "error execution phase
	// preflight: preflight checks failed" and nothing more, when the
	// [ERROR ...] lines naming the check that failed were sitting here.
	agentStateFile = "/var/lib/nke/state.json"

	// maskJoinCredentials blanks kubeadm's bootstrap token and certificate
	// key in the captured files.
	//
	// A site whose own kubeadm init succeeded has both in the agent's
	// journal -- the failure that prompted the collection can be anywhere
	// after it -- and this archive is uploaded as a CI artifact, so it
	// outlives the cluster they are scoped to. redactArchive cannot do this:
	// it substitutes the values the suite placed on the node, and these are
	// kubeadm's own.
	//
	// The third expression is the bootstrap token's documented shape, which
	// catches it in the agent's state as well, where it is a JSON field
	// rather than a command-line flag.
	maskJoinCredentials = "find " + nodeRecordDir + " -type f -exec sed -i -E" +
		" -e \"s/--token[= ][^[:space:]]+/--token " + redactedMarker + "/g\"" +
		" -e \"s/--certificate-key[= ][^[:space:]]+/--certificate-key " + redactedMarker + "/g\"" +
		" -e \"s/[a-z0-9]{6}\\.[a-z0-9]{16}/" + redactedMarker + "/g\"" +
		" {} +"
)

// nodeRecordItem is one file of the node record: the command to run, and the
// name to write its output under.
type nodeRecordItem struct {
	name string
	cmd  string
}

// nodeRecord is what to read off the node and where to put it, in the order
// a reader wants them: the agent first, since it is the process that runs
// kubeadm, then the two services a preflight check can fail on.
func nodeRecord() []nodeRecordItem {
	return []nodeRecordItem{
		// No -n: CWE polls the agent for status every few seconds, so on a
		// 30-minute timeout a tail is 30 minutes of polls and the kubeadm
		// output is above it.
		{"nke-agent.journal", "journalctl -b -u nke-agent --no-pager -o cat"},
		{"kubelet.journal", "journalctl -b -u kubelet --no-pager -o cat"},
		{"containerd.journal", "journalctl -b -u containerd --no-pager -o cat"},
		{"state.json", "cat " + agentStateFile},
	}
}

const (
	// diagnosticsArchive is where CollectDiagnostics stages its tarball on
	// the node. Beside the directory rather than inside it, so the archive
	// does not try to contain itself.
	diagnosticsArchive = workDir + "/diagnostics.tar.gz"

	// noDiagnosticsMarker is echoed by the remote shell instead of relying
	// on an exit status: `test -d X && tar` exits non-zero both when the
	// directory is missing and when tar fails, which are the two cases
	// worth telling apart.
	noDiagnosticsMarker = "nke-e2e-no-diagnostics"
)

// ErrNoDiagnostics is returned when the node yielded nothing at all: nkectl
// wrote no diagnostics directory and the node record is missing too, which
// takes a session that cannot run anything. nkectl's absence on its own is
// not an error, and used to be: run 36045936723 came up and tripped on a
// version-string comparison, and reporting "could not collect the node's
// diagnostics" there sent the reader looking for a second fault that did not
// exist. Distinguished from a real failure to archive or fetch, which means
// diagnostics do exist and are being lost.
var ErrNoDiagnostics = errors.New("the node has no diagnostics")

// CollectDiagnostics copies what the node knows about its own failure off it
// and into dest: the cluster diagnostics nkectl writes when it fails, and
// the node's own journals and agent state.
//
// It has to be called before the node is released, which on a failed run is
// minutes later, and nothing else preserves them: run 36016817952 spent 48
// minutes reaching a 30-minute "Provisioning" timeout and kept no evidence
// from the cluster side at all. A tarball rather than a file at a time
// because the directory's contents are nkectl's to decide.
//
// secrets are the values the run placed on the node, removed from the archive
// before it is written. The archive is uploaded as a CI artifact and a
// journal is a plausible place for a credential to surface, so a caller that
// put one on the machine is expected to name it here.
func CollectDiagnostics(ctx context.Context, s infra.Session, dest string, secrets ...string) error {
	// Before the archive, because the point is to be inside it. Not fatal:
	// nkectl's diagnostics are worth having on their own, and each item
	// records its own error in the file it was meant to fill.
	_ = captureNodeRecord(ctx, s)

	// Whichever of the two exist, named rather than globbed so tar does not
	// take the archive it is writing as a member.
	cmd := fmt.Sprintf("cd %s && members= && for d in %s %s; do if test -d $d; then members=\"$members $d\"; fi; done && "+
		"if test -z \"$members\"; then echo %s; else tar -czf %s $members; fi",
		shellQuote(workDir), path.Base(diagnosticsDir), path.Base(nodeRecordDir),
		shellQuote(noDiagnosticsMarker), shellQuote(diagnosticsArchive))
	out, err := s.Run(ctx, asRoot(cmd))
	if err != nil {
		return fmt.Errorf("archive the diagnostics on the node: %w\n%s", err, tail(out))
	}
	if strings.Contains(out, noDiagnosticsMarker) {
		return ErrNoDiagnostics
	}
	// Fetch, not Run: it reads into its own buffer, so the bytes arrive
	// unmixed with anything the shell says.
	data, err := s.Fetch(ctx, diagnosticsArchive)
	if err != nil {
		return err
	}
	redacted, err := redactArchive(data, secrets)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, redacted, 0o600)
}

// captureNodeRecord writes the node's own account of itself into
// nodeRecordDir, for CollectDiagnostics to archive alongside nkectl's.
//
// Every item redirects its own stderr into its own file, so an absent unit
// or an unreadable path documents itself in the archive instead of failing
// the capture and taking the rest with it.
func captureNodeRecord(ctx context.Context, s infra.Session) error {
	record := nodeRecord()
	script := make([]string, 0, len(record)+2)
	script = append(script, "mkdir -p "+shellQuote(nodeRecordDir))
	for _, item := range record {
		script = append(script, item.cmd+" > "+shellQuote(path.Join(nodeRecordDir, item.name))+" 2>&1")
	}
	script = append(script, maskJoinCredentials)
	if _, err := s.Run(ctx, asRoot(strings.Join(script, "; "))); err != nil {
		return fmt.Errorf("capture the node's record: %w", err)
	}
	return nil
}

func (b bootstrap) stageNkectl(ctx context.Context) error {
	data, err := os.ReadFile(b.candidate.Nkectl)
	if err != nil {
		return err
	}
	if err := b.session.Put(ctx, nkectlPath, data, 0o755); err != nil {
		return err
	}
	return b.run(ctx, shellQuote(nkectlPath)+" version")
}

func (b bootstrap) stageAssets(ctx context.Context) error {
	archive, err := archiveAssets(b.candidate.Assets, b.candidate.AssetPaths)
	if err != nil {
		return err
	}
	if err := b.session.Put(ctx, assetsArchive, archive, 0o600); err != nil {
		return err
	}
	return b.run(ctx, asRoot(fmt.Sprintf("rm -rf %[1]s && mkdir -p %[1]s && tar -C %[1]s -xzf %[2]s && rm -f %[2]s",
		shellQuote(assetsDir), shellQuote(assetsArchive))))
}

// importImages imports each archive into both containerd namespaces: k8s.io
// for the cluster, default for the bootstrap kind cluster nerdctl runs.
func (b bootstrap) importImages(ctx context.Context) error {
	for _, image := range b.candidate.Images {
		data, err := os.ReadFile(image)
		if err != nil {
			return err
		}
		remote := path.Join(imagesDir, filepath.Base(image))
		if err := b.session.Put(ctx, remote, data, 0o600); err != nil {
			return err
		}
		cmd := fmt.Sprintf("ctr -n k8s.io images import %[1]s && ctr -n default images import %[1]s && rm -f %[1]s",
			shellQuote(remote))
		if err := b.run(ctx, asRoot(cmd)); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(image), err)
		}
	}
	return nil
}

func (b bootstrap) generateSiteSpec(ctx context.Context) error {
	csv := fmt.Sprintf("name,ip,tpm_ek_thumbprint\n%s,%s,%s\n", b.node.Name, b.node.IP, b.node.TPMEkThumbprint)
	if err := b.session.Put(ctx, hostsCSV, []byte(csv), 0o600); err != nil {
		return err
	}
	args := []string{nkectlPath, "generate", "sitespec", b.candidate.SiteName, "--nodes", hostsCSV}
	for _, set := range []string{
		"cluster.k8sVersion=" + b.candidate.KubernetesVersion,
		"cluster.cniName=" + b.candidate.CNI,
		"spire.trustDomain=" + TrustDomain(b.candidate.SiteName),
	} {
		args = append(args, "--set", set)
	}
	return b.run(ctx, asRoot(quoteAll(args)+" > "+shellQuote(siteSpecPath)))
}

// bootstrapSite runs nkectl as root in a login shell. Under a plain sudo the
// bootstrap's nerdctl inherits the invoking user's environment and looks for
// a rootless containerd.
func (b bootstrap) bootstrapSite(ctx context.Context) error {
	args := []string{
		nkectlPath, "bootstrap", "site",
		"-f", siteSpecPath,
		"--kubeconfig-out", kubeconfigPath,
		"--diagnostics-dir", diagnosticsDir,
	}
	if b.candidate.DisableMTLS {
		args = append(args, "--disable-mtls")
	}
	cmd := "cd " + shellQuote(workDir) + " && " + b.env() + quoteAll(args)
	return b.run(ctx, asRoot(cmd))
}

func (b bootstrap) env() string {
	var sb strings.Builder
	for _, k := range sortedKeys(b.candidate.BootstrapEnv) {
		v := strings.ReplaceAll(b.candidate.BootstrapEnv[k], assetsToken, assetsDir)
		sb.WriteString(k + "=" + shellQuote(v) + " ")
	}
	return sb.String()
}

func (b bootstrap) deploy(ctx context.Context) error {
	return b.candidate.Deploy.Deploy(ctx, b.session, DeployTarget{
		Node:        b.node,
		SiteName:    b.candidate.SiteName,
		TrustDomain: TrustDomain(b.candidate.SiteName),
		Kubeconfig:  kubeconfigPath,
		Assets:      assetsDir,
	})
}

func (b bootstrap) run(ctx context.Context, cmd string) error {
	out, err := b.session.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, tail(out))
	}
	return nil
}

// asRoot runs a shell command as root in a login shell, so root's PATH and
// HOME apply rather than the SSH user's.
func asRoot(cmd string) string {
	return "sudo -n -i sh -c " + shellQuote("mkdir -p "+workDir+" && "+cmd)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func quoteAll(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tail keeps the end of a command's output, where the failure is.
func tail(out string) string {
	const limit = 4096
	if len(out) <= limit {
		return out
	}
	return "..." + out[len(out)-limit:]
}
