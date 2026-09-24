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
)

// diagnosticsArchive is where CollectDiagnostics stages its tarball on the
// node. Beside the directory rather than inside it, so the archive does not
// try to contain itself.
const diagnosticsArchive = workDir + "/diagnostics.tar.gz"

// ErrNoDiagnostics is returned when the node has no diagnostics directory,
// which is the ordinary case: nkectl writes one when it fails, so its absence
// usually means the deploy worked and the run failed somewhere after it. Run
// 36045936723 did exactly that -- the stack came up and the suite tripped on
// a version-string comparison -- and reporting "could not collect the node's
// diagnostics" as an error there sent the reader looking for a second fault
// that did not exist. Distinguished from a real failure to archive or fetch,
// which means diagnostics do exist and are being lost.
var ErrNoDiagnostics = errors.New("the node has no diagnostics directory")

// noDiagnosticsMarker is echoed by the remote shell instead of relying on an
// exit status: `test -d X && tar` exits non-zero both when the directory is
// missing and when tar fails, which are the two cases worth telling apart.
const noDiagnosticsMarker = "nke-e2e-no-diagnostics"

// CollectDiagnostics copies the cluster diagnostics nkectl writes on failure
// off the node and into dest.
//
// It has to be called before the node is released, which on a failed run is
// minutes later, and nothing else preserves them: run 36016817952 spent 48
// minutes reaching a 30-minute "Provisioning" timeout and kept no evidence
// from the cluster side at all. A tarball rather than a file at a time
// because the directory's contents are nkectl's to decide.
func CollectDiagnostics(ctx context.Context, s infra.Session, dest string) error {
	cmd := fmt.Sprintf("if test -d %s; then tar -czf %s -C %s diagnostics; else echo %s; fi",
		shellQuote(diagnosticsDir), shellQuote(diagnosticsArchive), shellQuote(workDir),
		shellQuote(noDiagnosticsMarker))
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
	return os.WriteFile(dest, data, 0o644)
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
