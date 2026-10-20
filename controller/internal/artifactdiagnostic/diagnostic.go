// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package artifactdiagnostic owns producer-neutral, unqualified supplied-artifact checks.
package artifactdiagnostic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	nativePlatform       = "linux/amd64"
	chartPackageKind     = "chart-package"
	nativeOS             = "linux"
	cleanupFailed        = "failed"
	complete             = "complete"
	controllerConfigName = "controller-config"
	postgresName         = "postgres"
	temporalName         = "temporal"
	nativeArch           = "amd64"
	controllerImageKind  = "controller-image"
	kindTool             = "kind"
)

// Options names one supplied artifact. Supporting images are never subjects.
type Options struct {
	Kind, Subject, SHA256, Version, Output, Cluster string
	NodeImage, PostgresImage, TemporalImage         string
	Timeout                                         time.Duration
}

// Report separates assertion completion, process failure, and resource cleanup.
type Report struct {
	APIVersion      string   `json:"apiVersion"`
	Capture         string   `json:"capture"`
	ChartName       string   `json:"chartName,omitempty"`
	ChartVersion    string   `json:"chartVersion,omitempty"`
	ChartAppVersion string   `json:"chartAppVersion,omitempty"`
	Prerequisites   []string `json:"prerequisites"`
	Kind            string   `json:"kind"`
	SubjectSHA256   string   `json:"subjectSha256"`
	Platform        string   `json:"platform"`
	Completed       bool     `json:"completed"`
	Phase           string   `json:"phase"`
	Failure         string   `json:"failure,omitempty"`
	Cleanup         string   `json:"cleanup"`
	CleanupFailure  string   `json:"cleanupFailure,omitempty"`
	Checks          []string `json:"checks"`
	IndexDigest     string   `json:"indexDigest,omitempty"`
	ManifestDigest  string   `json:"manifestDigest,omitempty"`
	ConfigDigest    string   `json:"configDigest,omitempty"`
	RuntimeImage    string   `json:"runtimeImage,omitempty"`
	ContainerID     string   `json:"containerId,omitempty"`
	NodeName        string   `json:"nodeName,omitempty"`
	NodePlatform    string   `json:"nodePlatform,omitempty"`
	PodUID          string   `json:"podUid,omitempty"`
	Version         string   `json:"version,omitempty"`
	Dependencies    []string `json:"dependencies,omitempty"`
}

type runner struct {
	options Options
	report  Report
	log     *os.File
}

// Admit rejects static unsupported inputs before creating output or resources.
func Admit(ctx context.Context, o Options) error {
	if ctx == nil {
		return errors.New("context required")
	}
	if o.Kind != controllerImageKind && o.Kind != chartPackageKind {
		return fmt.Errorf("unsupported kind %q", o.Kind)
	}
	if runtime.GOOS != nativeOS || runtime.GOARCH != nativeArch {
		return errors.New("requires native Linux AMD64")
	}
	if o.Timeout <= 0 || o.Timeout > time.Hour || !filepath.IsAbs(o.Output) || !validDigest(o.SHA256) {
		return errors.New("positive bounded timeout, new output directory and subject SHA256 required")
	}
	info, err := os.Lstat(o.Subject)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("subject must be a regular non-symlink file")
	}
	tools := []string{"go", "bash", "helm", "yq", "python3"}
	if o.Kind == controllerImageKind {
		tools = []string{"go", "docker", kindTool, "kubectl"}
		if o.Version == "" || !strings.HasPrefix(o.Cluster, "iv2-") || len(o.Cluster) != 36 || strings.Trim(o.Cluster[4:], "0123456789abcdef") != "" {
			return errors.New("expected version and private iv2 cluster name required")
		}
		for _, image := range []string{o.NodeImage, o.PostgresImage, o.TemporalImage} {
			if !pinnedImage(image) {
				return errors.New("supporting images require repository@sha256 digests")
			}
		}
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool %s: %w", tool, err)
		}
	}
	if o.Kind == chartPackageKind {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(bounded, "python3", "-c", "import yaml")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
		configureProcess(cmd)
		err := cmd.Run()
		stopProcess(cmd)
		if err != nil {
			return fmt.Errorf("python PyYAML prerequisite: %w", err)
		}
	}
	return nil
}

// Run retains original logs and a terminal report, including after preparation failure.
func Run(ctx context.Context, o Options) (result error) {
	if err := Admit(ctx, o); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if o.Kind == controllerImageKind {
		if _, _, _, err := inspectImage(bounded, o.Subject); err != nil {
			return err
		}
	} else {
		if _, err := extractChart(bounded, o.Subject, ""); err != nil {
			return err
		}
	}
	if err := os.Mkdir(o.Output, 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(o.Output, "execution.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	r := runner{o, Report{APIVersion: "nvke.dev/artifact-diagnostic/v1", Kind: o.Kind, SubjectSHA256: o.SHA256, Platform: nativePlatform, Phase: "inspect", Capture: complete, Prerequisites: []string{"native-linux-amd64", "go", "bash", "helm", "yq", "python3"}, Cleanup: "not-started", Checks: []string{}}, log}
	defer func() {
		verify, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer stop()
		if err := checkHash(verify, o.Subject, o.SHA256); err != nil {
			result = errors.Join(result, err)
		}
		r.report.Completed = result == nil
		if result != nil {
			r.report.Failure = result.Error()
		} else {
			r.report.Phase = complete
		}
		raw, err := json.MarshalIndent(r.report, "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(o.Output, "report.json.partial"), raw, 0o600)
		}
		if err == nil {
			err = os.Rename(filepath.Join(o.Output, "report.json.partial"), filepath.Join(o.Output, "report.json"))
		}
		result = errors.Join(result, err)
	}()
	if err := checkHash(bounded, o.Subject, o.SHA256); err != nil {
		return err
	}
	if o.Kind == chartPackageKind {
		return r.chart(bounded)
	}
	r.report.Prerequisites = []string{"native-linux-amd64", "go", "docker", kindTool, "kubectl"}
	return r.controller(bounded)
}

func validDigest(s string) bool {
	raw, err := hex.DecodeString(strings.TrimPrefix(s, "sha256:"))
	return strings.HasPrefix(s, "sha256:") && err == nil && len(raw) == 32
}

func pinnedImage(s string) bool {
	repo, digest, ok := strings.Cut(s, "@")
	return ok && repo != "" && !strings.ContainsAny(repo, " \n\r\t") && validDigest(digest)
}

func checkHash(ctx context.Context, path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, contextReader(func(p []byte) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return f.Read(p)
	})); err != nil {
		return err
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != want {
		return errors.New("supplied artifact bytes changed")
	}
	return nil
}

func (r *runner) command(ctx context.Context, input io.Reader, name string, args ...string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(bounded, name, args...)
	cmd.Stdin = input
	// Inherit tool locations, but never ambient suite selectors or Make/Go overrides.
	for _, key := range []string{"PATH", "HOME", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "GOTOOLCHAIN=auto", "GOFLAGS=-mod=readonly", "KIND_EXPERIMENTAL_PROVIDER=docker", "DOCKER_CONTEXT=default", "KIND_EXPERIMENTAL_DOCKER_NETWORK="+r.options.Cluster, "HELM_CACHE_HOME="+filepath.Join(r.options.Output, "helm-cache"), "HELM_CONFIG_HOME="+filepath.Join(r.options.Output, "helm-config"), "HELM_DATA_HOME="+filepath.Join(r.options.Output, "helm-data"))
	configureProcess(cmd)
	cmd.WaitDelay = 5 * time.Second
	var output bytes.Buffer
	sink := &boundedOutput{destination: io.MultiWriter(r.log, &output), remaining: 8 << 20}
	cmd.Stdout = sink
	errorsSink := &boundedOutput{destination: r.log, remaining: 8 << 20}
	cmd.Stderr = errorsSink
	err := cmd.Run()
	stopProcess(cmd)
	out := output.Bytes()
	var writeErr error
	if sink.truncated || errorsSink.truncated {
		r.report.Capture = "incomplete"
		writeErr = errors.New("command diagnostics exceeded 8 MiB")
	}
	if err != nil {
		return out, fmt.Errorf("%s failed: %w", name, err)
	}
	return out, writeErr
}

// boundedOutput keeps original diagnostics bounded without blocking process teardown.
type boundedOutput struct {
	destination io.Writer
	remaining   int
	truncated   bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
		w.truncated = true
	}
	written, err := w.destination.Write(p)
	w.remaining -= written
	if err != nil {
		return written, err
	}
	return n, nil
}

type contextReader func([]byte) (int, error)

func (r contextReader) Read(p []byte) (int, error) { return r(p) }
