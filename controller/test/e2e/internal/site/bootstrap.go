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
	"strings"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra"
)

// Deployer installs the NKE stack onto a freshly bootstrapped cluster, over
// the same session bootstrap used. It is an interface because what "the
// stack" is, and how it is installed, is decided outside this suite.
type Deployer interface {
	Deploy(ctx context.Context, s infra.Session, t DeployTarget) error
}

// DeployTarget is the cluster a Deployer installs onto.
type DeployTarget struct {
	Node        infra.Node
	SiteName    string
	TrustDomain string

	// Cluster is what to install onto. The deploy runs its commands on the
	// node, so it reaches the API server through the node's own kubeconfig.
	Cluster Cluster
}

// Cluster is what Bootstrap left behind: Kubernetes, with nothing installed on
// it yet, reachable from the runner and from the node it runs on.
type Cluster struct {
	// Kubeconfig is a runner path to an admin kubeconfig that reaches the
	// cluster through the session's forward. It is valid until Teardown.
	//
	// Written into a new temporary directory, which is the caller's to remove:
	// it holds an admin credential, and this function does not know how long
	// the caller needs it.
	Kubeconfig string

	// NodeKubeconfig is the same cluster's admin kubeconfig, as a path on the
	// node.
	NodeKubeconfig string
}

// Bootstrap builds Kubernetes on n: it stages the site onto the node, runs
// `nkectl bootstrap site`, and forwards the resulting admin kubeconfig back to
// the runner.
//
// It stops there. Installing the NKE stack is a Deployer's job and checking
// the controller is ProbeController's, and a caller that wants neither simply does not call
// them. That is what lets a run stop after Kubernetes without a do-nothing
// deployer to express it.
//
// A failure leaves the node as it is, for the caller to collect from before
// tearing the session down.
func Bootstrap(ctx context.Context, n infra.Node, s infra.Session, nkectl string, spec Spec) (Cluster, error) {
	if err := spec.validate(); err != nil {
		return Cluster{}, err
	}
	if _, err := os.Stat(nkectl); err != nil {
		return Cluster{}, fmt.Errorf("site: nkectl: %w", err)
	}
	if n.Name == "" || n.IP == "" {
		return Cluster{}, errors.New("site: node has no name or IP")
	}

	b := bootstrap{node: n, session: s, localNkectl: nkectl, spec: spec}
	for _, step := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"stage nkectl", b.stageNkectl},
		{"import images", b.importImages},
		{"generate site spec", b.generateSiteSpec},
		{"bootstrap site", b.bootstrapSite},
	} {
		if err := step.run(ctx); err != nil {
			return Cluster{}, fmt.Errorf("site %s: %s: %w", spec.Name, step.name, err)
		}
	}

	kubeconfig, err := b.forwardKubeconfig(ctx)
	if err != nil {
		return Cluster{}, fmt.Errorf("site %s: forward kubeconfig: %w", spec.Name, err)
	}
	return Cluster{
		Kubeconfig:     kubeconfig,
		NodeKubeconfig: kubeconfigPath,
	}, nil
}

// TrustDomain is the SPIRE trust domain a site is bootstrapped with.
func TrustDomain(siteName string) string { return siteName + ".nke.local" }

type bootstrap struct {
	node    infra.Node
	session infra.Session
	// localNkectl is the binary on the runner, staged to nkectlPath on the
	// node. The only path in this type that is not the node's.
	localNkectl string
	spec        Spec
}

// Paths on the node, not on the runner. workDir is the run's scratch
// directory; nkectlPath is the only one outside it, because that is where the
// binary is installed rather than where this run keeps its working files.
const (
	nkectlPath     = "/usr/local/bin/nkectl"
	imagesDir      = workDir + "/images"
	hostsCSV       = workDir + "/hosts.csv"
	siteSpecPath   = workDir + "/sitespec.yaml"
	kubeconfigPath = workDir + "/site.kubeconfig"
	diagnosticsDir = workDir + "/diagnostics"
)

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

// ErrNoDiagnostics is returned when the node has no diagnostics directory,
// which is the ordinary case: nkectl writes one when it fails, so its absence
// usually means the deploy worked and the run failed somewhere after it. Run
// 36045936723 did exactly that -- the stack came up and the suite tripped on
// a version-string comparison -- and reporting "could not collect the node's
// diagnostics" as an error there sent the reader looking for a second fault
// that did not exist. Distinguished from a real failure to archive or fetch,
// which means diagnostics do exist and are being lost.
var ErrNoDiagnostics = errors.New("the node has no diagnostics directory")

// CollectDiagnostics copies the cluster diagnostics nkectl writes on failure
// off the node and into dest.
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
	redacted, err := redactArchive(data, secrets)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, redacted, 0o600)
}

func (b bootstrap) stageNkectl(ctx context.Context) error {
	data, err := os.ReadFile(b.localNkectl)
	if err != nil {
		return err
	}
	if err := b.session.Put(ctx, nkectlPath, data, 0o755); err != nil {
		return err
	}
	return b.run(ctx, shellQuote(nkectlPath)+" version")
}

// importImages imports each archive into both containerd namespaces: k8s.io
// for the cluster, default for the bootstrap kind cluster nerdctl runs.
func (b bootstrap) importImages(ctx context.Context) error {
	for _, image := range b.spec.ImageArchives {
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
	args := []string{nkectlPath, "generate", "sitespec", b.spec.Name, "--nodes", hostsCSV}
	for _, set := range []string{
		"cluster.k8sVersion=" + b.spec.KubernetesVersion,
		"cluster.cniName=" + b.spec.CNI,
		"spire.trustDomain=" + TrustDomain(b.spec.Name),
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
	if b.spec.DisableMTLS {
		args = append(args, "--disable-mtls")
	}
	cmd := "cd " + shellQuote(workDir) + " && " + b.env() + quoteAll(args)
	return b.run(ctx, asRoot(cmd))
}

// env is the image references nkectl reads from the environment. The mapping
// from a reference to the variable that carries it lives here, so a caller
// states what it is qualifying and not how nkectl is told.
//
// The controller image goes under both its names: the staged nkectl may be a
// published release from before NKECTL_CONTROLLER_IMAGE, which reads only
// NKECTL_CWE_IMAGE. A current nkectl prefers the new name and, with it set,
// does not warn about the old one.
func (b bootstrap) env() string {
	var sb strings.Builder
	for _, v := range []struct{ key, value string }{
		{"NKECTL_CONTROLLER_IMAGE", b.spec.ControllerImage},
		{"NKECTL_CWE_IMAGE", b.spec.ControllerImage},
		{"NKECTL_CAPI_PROVISIONED_IMAGE", b.spec.CAPIProvisionedImage},
	} {
		if v.value != "" {
			sb.WriteString(v.key + "=" + shellQuote(v.value) + " ")
		}
	}
	return sb.String()
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
