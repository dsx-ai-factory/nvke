// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifactdiagnostic

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// extractChart admits only a bounded single-root package with ordinary files.
func extractChart(ctx context.Context, archive, dest string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > 64<<20 {
		return "", errors.New("chart package exceeds 64 MiB")
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: 256<<20 + 1}
	tr := tar.NewReader(limited)
	seen := map[string]bool{}
	root := ""
	metadata := false
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if root == "" {
			root = strings.Split(name, "/")[0]
		}
		if count >= 10000 || limited.N <= 0 || root == "" || name != path.Clean(name) || !filepath.IsLocal(name) || strings.Contains(name, "\\") || seen[name] || (name != root && !strings.HasPrefix(name, root+"/")) {
			return "", errors.New("unsafe or duplicate chart entry")
		}
		seen[name] = true
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return "", errors.New("chart links and special files unsupported")
		}
		for key := range h.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse.") {
				return "", errors.New("sparse chart entry unsupported")
			}
		}
		if dest != "" {
			target := filepath.Join(dest, filepath.FromSlash(name))
			if h.Typeflag == tar.TypeDir {
				if err := os.MkdirAll(target, 0o700); err != nil {
					return "", err
				}
				continue
			}
			if h.Size < 0 || h.Size > 256<<20 {
				return "", errors.New("chart entry exceeds bound")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return "", err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return "", err
			}
			_, copyErr := io.CopyN(out, tr, h.Size)
			closeErr := out.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return "", err
			}
		} else if _, err := io.CopyN(io.Discard, tr, h.Size); err != nil {
			return "", err
		}

		if name == root+"/Chart.yaml" {
			metadata = true
		}
	}
	// Consume gzip checksum and reject hidden trailing payloads.
	padding, err := io.ReadAll(limited)
	if err != nil || limited.N <= 0 {
		return "", errors.New("invalid or oversized chart stream")
	}
	if strings.Trim(string(padding), "\x00") != "" || !metadata {
		return "", errors.New("chart metadata missing or trailing payload")
	}
	return filepath.Join(dest, root), nil
}

func (r *runner) chart(ctx context.Context) (result error) {
	scratch, err := os.MkdirTemp(r.options.Output, "chart-")
	if err != nil {
		return err
	}
	r.report.Cleanup = "pending"
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			r.report.Cleanup = cleanupFailed
			r.report.CleanupFailure = err.Error()
			result = errors.Join(result, err)
		} else {
			r.report.Cleanup = complete
		}
	}()
	chart, err := extractChart(ctx, r.options.Subject, scratch)
	if err != nil {
		return err
	}
	metadata, err := r.command(ctx, nil, "helm", "show", "chart", chart)
	if err != nil {
		return err
	}
	var identity struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(metadata, &identity); err != nil {
		return err
	}
	if identity.Name == "" || identity.Version == "" {
		return errors.New("missing chart identity")
	}
	r.report.ChartName = identity.Name
	r.report.ChartVersion = identity.Version
	r.report.ChartAppVersion = identity.AppVersion
	r.report.Checks = append(r.report.Checks, "package")
	r.report.Phase = "assertions"
	if _, err := r.command(ctx, nil, "helm", "lint", "--strict", chart); err != nil {
		return err
	}
	rendered, err := r.command(ctx, nil, "helm", "template", "iv2", chart)
	if err != nil {
		return err
	}
	if len(rendered) == 0 {
		return errors.New("empty chart render")
	}
	if err := os.WriteFile(filepath.Join(r.options.Output, "rendered.yaml"), rendered, 0o600); err != nil {
		return err
	}
	r.report.Checks = append(r.report.Checks, "render")
	// This inventory belongs upstream. No caller selector can filter required groups.
	for _, script := range []string{controllerConfigName, "auth", "provider", "spire-attestation", "node-agent-auth", "node-agent-external-mtls"} {
		// env arguments are explicit; the command owner strips ambient overrides.
		if _, err := r.command(ctx, nil, "env", "CHART="+chart, "RENDERS_DIR="+filepath.Join(r.options.Output, "renders"), "GO_TEST_FLAGS=", "bash", "hack/chart-"+script+"-assertions.sh"); err != nil {
			return err
		}
	}
	for _, script := range []string{"identity", "rbac"} {
		if _, err := r.command(ctx, nil, "python3", "hack/chart-"+script+"-assertions.py", "--chart", chart); err != nil {
			return fmt.Errorf("%s contracts: %w", script, err)
		}
	}
	r.report.Checks = append(r.report.Checks, "configuration-contracts")
	return nil
}
