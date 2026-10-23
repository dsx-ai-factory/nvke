// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
)

// These tests run what Stack renders through the deploy path's own scripts and
// charts, as exported from DeployPathRef. The unit tests pin what is rendered;
// these catch a value the charts do not read, which on a live site fails
// silently or an hour in.
//
// They need bash, python3 and helm, and a clone DeployPathRef resolves in.
// The SPIRE chart is fetched from its repository and is skipped offline.

// deployPath exports the pinned deploy path, or skips.
func deployPath(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("renders charts with helm")
	}
	for _, tool := range []string{"git", "bash", "python3", "helm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	top, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not in a git clone: %v", err)
	}
	repo := strings.TrimSpace(string(top))
	if exec.CommandContext(context.Background(), "git", "-C", repo, "cat-file", "-e", DeployPathRef+"^{commit}").Run() != nil {
		t.Skipf("%s does not resolve in this clone", DeployPathRef)
	}
	dir, err := ExportRef(context.Background(), repo, DeployPathRef, CurrentLayout().Export)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// databasePassword is what renderValues gives Stack, and wantDBSecret is how a
// chart's Secret carries it once rendered.
const databasePassword = "db-secret"

var wantDBSecret = base64.StdEncoding.EncodeToString([]byte(databasePassword))

// skipOffline skips when helm could not reach a chart repository or was rate
// limited by it, since these tests are about what the charts read and not
// about the network.
func skipOffline(t *testing.T, chart string, err error) {
	t.Helper()
	for _, offline := range []string{"dial tcp", "no such host", "cannot be reached", "toomanyrequests"} {
		if strings.Contains(err.Error(), offline) {
			t.Skipf("%s chart unreachable: %v", chart, err)
		}
	}
}

// renderValues writes what Stack renders for sbomTarget into a directory.
func renderValues(t *testing.T, assets string) string {
	t.Helper()
	overlay, err := os.ReadFile(filepath.Join(assets, "deploy", "nico", "spire", "overlay.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	d := testSBOM().withDefaults()
	d.DatabasePassword = databasePassword
	files, err := d.render(sbomTarget, overlay)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runTool(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), errors.New(err.Error() + ": " + stderr.String())
	}
	return stdout.String(), nil
}

// helmInstalls maps each release to the argv nvke-deploy.sh would run for
// it, as the bash-quoted line --dry-run prints.
func helmInstalls(t *testing.T, dryRun string) map[string]string {
	t.Helper()
	release := regexp.MustCompile(`^helm upgrade --install (\S+) `)
	installs := map[string]string{}
	for _, line := range strings.Split(dryRun, "\n") {
		if m := release.FindStringSubmatch(line); m != nil {
			installs[m[1]] = line
		}
	}
	return installs
}

// helmTemplate replays an install as helm template, so the chart renders from
// exactly the files, in exactly the order, the install would use.
func helmTemplate(t *testing.T, install string) (string, error) {
	t.Helper()
	cmd := strings.Replace(strings.TrimSpace(install), "helm upgrade --install ", "helm template ", 1)
	home := t.TempDir()
	for _, v := range []string{"HELM_CACHE_HOME", "HELM_CONFIG_HOME", "HELM_DATA_HOME"} {
		t.Setenv(v, filepath.Join(home, v))
	}
	return runTool(t, "bash", "-c", cmd)
}

func documents(t *testing.T, manifest string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for _, raw := range strings.Split(manifest, "\n---") {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("parse rendered manifest: %v", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

func find(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, doc := range docs {
		if doc["kind"] == kind && dig(t, doc, "metadata", "name") == name {
			return doc
		}
	}
	t.Fatalf("no %s %s rendered", kind, name)
	return nil
}

func TestSBOMAgainstDeployPath(t *testing.T) {
	assets := deployPath(t)
	// This renders the deploy path in the checkout, so it asserts the name
	// that deploy path installs, not whatever a lane is configured for. The
	// two are the same thing read from the same file, which is the point of
	// controllerNameFromDeployPath.
	name, err := controllerNameFromDeployPath(assets, CurrentLayout().DeployPath)
	if err != nil {
		t.Fatalf("reading the deploy path's controller name: %v", err)
	}
	t.Setenv(controller.EnvNamespace, name)
	t.Setenv(controller.EnvService, name)
	values := renderValues(t, assets)
	dryRun, err := runTool(t, "bash", filepath.Join(assets, "deploy", "nvke-deploy.sh"), "install",
		"--deploy-path", filepath.Join(assets, "deploy", "nico"),
		"--config", filepath.Join(values, "bootstrap-config.json"),
		"--values-for", "controller="+filepath.Join(values, "controller.yaml"),
		"--values-for", "spire="+filepath.Join(values, "spire.yaml"),
		"--values-for", "postgresql="+filepath.Join(values, "postgresql.yaml"),
		"--values-for", "temporal="+filepath.Join(values, "temporal.yaml"),
		"--dry-run")
	if err != nil {
		t.Fatalf("nvke-deploy.sh --dry-run: %v\n%s", err, dryRun)
	}
	installs := helmInstalls(t, dryRun)
	// A chart installed by path has to be among the exported assets, or the
	// install fails on the site; --dry-run only prints the command.
	for release, install := range installs {
		if fields := strings.Fields(install); len(fields) > 4 && filepath.IsAbs(fields[4]) {
			if _, err := os.Stat(filepath.Join(fields[4], "Chart.yaml")); err != nil {
				t.Errorf("release %s installs %s, which the layout does not export: %v", release, fields[4], err)
			}
		}
	}
	for release, file := range map[string]string{
		"nvke-controller": "controller.yaml",
		"spire":           "spire.yaml",
		"postgres":        "postgresql.yaml",
		"temporal":        "temporal.yaml",
	} {
		install, ok := installs[release]
		if !ok {
			t.Errorf("no helm install for release %s in:\n%s", release, dryRun)
			continue
		}
		if !strings.HasSuffix(strings.TrimSpace(install), "--values "+filepath.Join(values, file)) {
			t.Errorf("release %s does not take %s last, so the deploy path's values would win:\n%s", release, file, install)
		}
	}

	t.Run("controller chart", func(t *testing.T) {
		install, ok := installs["nvke-controller"]
		if !ok {
			t.Skip("no controller install")
		}
		manifest, err := helmTemplate(t, install)
		if err != nil {
			t.Fatal(err)
		}
		docs := documents(t, manifest)

		svc := find(t, docs, "Service", controllerName())
		if ns := dig(t, svc, "metadata", "namespace"); ns != controllerName() {
			t.Errorf("controller Service namespace = %v, want %s: the health probe would miss it", ns, controllerName())
		}

		deployment := find(t, docs, "Deployment", controllerName())
		containers, _ := dig(t, deployment, "spec", "template", "spec", "containers").([]any)
		if len(containers) == 0 {
			t.Fatal("controller Deployment has no containers")
		}
		container, _ := containers[0].(map[string]any)
		if got := container["image"]; got != "nvcr.io/nvke/cwe:v1.2.3" {
			t.Errorf("controller image = %v", got)
		}
		env := map[string]any{}
		for _, e := range container["env"].([]any) {
			entry, _ := e.(map[string]any)
			if name, ok := entry["name"].(string); ok {
				env[name] = entry["value"]
			}
		}
		for name, want := range map[string]string{
			"NODE_AGENT_PORT":          nodeAgentPort,
			"SPIRE_SERVER_API_ADDRESS": spireServerAPIAddress,
		} {
			if env[name] != want {
				t.Errorf("controller env %s = %v, want %s", name, env[name], want)
			}
		}

		var settings map[string]any
		raw, _ := dig(t, find(t, docs, "ConfigMap", "nvke-controller-config"), "data", "config.yaml").(string)
		if err := yaml.Unmarshal([]byte(raw), &settings); err != nil {
			t.Fatalf("parse nke-controller-config: %v", err)
		}
		for key, want := range map[string]string{
			// The greenfield profile, not the chart's legacy defaults.
			"temporal.namespace":             controllerName(),
			"temporal.taskQueue":             controllerName() + "-tasks",
			"fieldManager.name":              controllerName(),
			"fieldManager.rebootMachineName": controllerName() + "-reboot-machine",
			"managedByLabelValue":            controllerName(),
			"teleport.managedByLabelValue":   controllerName(),
			"teleport.tokenNamePrefix":       controllerName() + "-",
		} {
			if got := dig(t, settings, strings.Split(key, ".")...); got != want {
				t.Errorf("nke-controller-config %s = %v, want %s", key, got, want)
			}
		}
		if got, _ := env["DB_CONNECTION_STRING"].(string); !strings.Contains(got, "/"+controllerName()+"?") {
			t.Errorf("DB_CONNECTION_STRING = %v, want database %s", got, controllerName())
		}

		for _, want := range []string{
			"nvcr.io/nvke/profiles:run-42",
			"https://10.0.0.5:30443/.well-known/openid-configuration",
			"e2e-site.nke.local",
			"clusters/finalizers",
			"dgxc-tenant-credentials",
		} {
			if !strings.Contains(manifest, want) {
				t.Errorf("controller chart does not render %s", want)
			}
		}
	})

	t.Run("spire chart", func(t *testing.T) {
		install, ok := installs["spire"]
		if !ok {
			t.Skip("no spire install")
		}
		manifest, err := helmTemplate(t, install)
		if err != nil {
			skipOffline(t, "SPIRE", err)
			t.Fatal(err)
		}
		for _, want := range []string{
			"nke.site-controller.internal",
			"10.0.0.5",
			"https://10.0.0.5:30443/keys",
			mustChecksum(t, "arm64"),
			"spiffe://e2e-site.nke.local" + spireAdminPath(),
		} {
			if !strings.Contains(manifest, want) {
				t.Errorf("SPIRE chart does not render %s", want)
			}
		}
	})

	// The database password reaches three releases under two different keys,
	// and a key no chart reads is silent at install time: the Bitnami chart
	// generates a superuser password of its own, and the mismatch surfaces
	// later as the Temporal schema job crash-looping on "password
	// authentication failed". These assert the value arrives in the charts,
	// which is what the unit tests on render cannot see.
	t.Run("postgres chart", func(t *testing.T) {
		install, ok := installs["postgres"]
		if !ok {
			t.Skip("no postgres install")
		}
		manifest, err := helmTemplate(t, install)
		if err != nil {
			skipOffline(t, "PostgreSQL", err)
			t.Fatal(err)
		}
		secret := find(t, documents(t, manifest), "Secret", "postgres-postgresql")
		if got := dig(t, secret, "data", "postgres-password"); got != wantDBSecret {
			t.Errorf("postgres-password = %v, want %s: the chart generated one of its own, so nothing "+
				"connecting as postgres holds what the other releases were given", got, wantDBSecret)
		}
		if want := `CREATE DATABASE "` + controllerName() + `"`; !strings.Contains(manifest, want) {
			t.Errorf("postgres chart does not render %s: nothing creates the controller's database", want)
		}
	})

	t.Run("temporal chart", func(t *testing.T) {
		install, ok := installs["temporal"]
		if !ok {
			t.Skip("no temporal install")
		}
		manifest, err := helmTemplate(t, install)
		if err != nil {
			skipOffline(t, "Temporal", err)
			t.Fatal(err)
		}
		docs := documents(t, manifest)
		// One assertion per store rather than a search of the manifest:
		// setting the password on only one of the two is the specific mistake
		// the deploy path warns about.
		for _, name := range []string{"temporal-default-store", "temporal-visibility-store"} {
			if got := dig(t, find(t, docs, "Secret", name), "data", "password"); got != wantDBSecret {
				t.Errorf("%s password = %v, want %s", name, got, wantDBSecret)
			}
		}
	})

	t.Run("credential templates", func(t *testing.T) {
		out := t.TempDir()
		if _, err := runTool(t, "python3", filepath.Join(assets, "deploy", "lib", "render-template.py"),
			filepath.Join(values, "bootstrap-config.json"),
			filepath.Join(assets, "deploy", "nico", "nvke-base-config"), out); err != nil {
			t.Fatal(err)
		}
		rendered := map[string]string{}
		entries, err := os.ReadDir(out)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(out, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "{{") {
				t.Errorf("%s has an unrendered field:\n%s", e.Name(), data)
			}
			rendered[e.Name()] = string(data)
		}
		for file, wants := range map[string][]string{
			"20-nico-credentials.yaml":        {"https://forge.example", "org", "tok", "forge"},
			"35-dgxc-tenant-credentials.yaml": {`user: "admin"`, `password: "p"`},
		} {
			for _, want := range wants {
				if !strings.Contains(rendered[file], want) {
					t.Errorf("%s lacks %s:\n%s", file, want, rendered[file])
				}
			}
		}
	})
}
