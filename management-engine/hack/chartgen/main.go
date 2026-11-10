// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// chartgen builds the chart from Kubebuilder output and bounded packaging extensions.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

func main() {
	binary := flag.String("kubebuilder", "bin/kubebuilder", "Pinned Kubebuilder executable")
	check := flag.Bool("check", false, "Report chart drift without updating the chart")
	flag.Parse()
	if err := generate(*binary, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(binary string, check bool) (err error) {
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "cluster-service-chart-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	for _, pair := range [][2]string{
		{"PROJECT", "PROJECT"}, {"dist/install.yaml", "input.yaml"},
		{"chart/Chart.yaml", "generated/chart/Chart.yaml"},
		{"chart/Chart.lock", "generated/chart/Chart.lock"},
	} {
		if err := copyFile(pair[0], filepath.Join(stage, pair[1])); err != nil {
			return err
		}
	}
	cmd := exec.Command(binary, "edit", "--plugins=helm/v2-alpha", "--manifests=./input.yaml", "--output-dir=generated")
	cmd.Dir = stage
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kubebuilder: %w\n%s", err, output)
	}
	chart := filepath.Join(stage, "generated/chart")
	if err := customize(chart); err != nil {
		return err
	}
	for _, pair := range [][2]string{
		{"hack/chart/templates", "templates"},
		{"config/crd/bases", "files/crds"},
		{"config/dev/tenant-api-rbac.yaml", "files/api/tenant-api-rbac.yaml"},
		{"config/dev/operator-api-rbac.yaml", "files/api/operator-api-rbac.yaml"},
		{"config/rbac/role.yaml", "files/roles/role.yaml"},
		{"config/rbac/leader_election_role.yaml", "files/roles/leader_election_role.yaml"},
		{"config/rbac/metrics_auth_role.yaml", "files/roles/metrics_auth_role.yaml"},
		{"hack/chart/values.schema.json", "values.schema.json"},
		{"../LICENSE", "LICENSE"}, {"../NOTICE", "NOTICE"},
	} {
		if err := copyPath(pair[0], filepath.Join(chart, pair[1])); err != nil {
			return err
		}
	}
	header, err := os.ReadFile("hack/boilerplate.yaml.txt")
	if err != nil {
		return err
	}
	if err := filepath.WalkDir(chart, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".tpl" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(data, []byte("SPDX-License-Identifier:")) {
			return os.WriteFile(path, append(append([]byte{}, header...), append([]byte("\n"), data...)...), 0o644)
		}
		return nil
	}); err != nil {
		return err
	}
	return synchronize(chart, "chart", check)
}

// customize adds chart topology and reference hooks without replacing workload manifests.
func customize(chart string) error {
	path := filepath.Join(chart, "templates/manager/manager.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := string(data)
	replacements := [][2]string{
		{"automountServiceAccountToken: false",
			"automountServiceAccountToken: {{ eq .Values.manager.stateAccess.mode \"inCluster\" }}"},
		{"        {{- range .Values.manager.args }}\n        - {{ . }}\n        {{- end }}",
			"{{ include \"management-engine.managerArgs\" . }}"},
		{"        - mountPath: /etc/management-api\n" +
			"          name: management-api\n          readOnly: true",
			"{{ include \"management-engine.managedMounts\" . }}"},
		{"      - name: management-api\n        secret:\n" +
			"          defaultMode: 292\n          secretName: management-state-client",
			"{{ include \"management-engine.managedVolumes\" . }}"},
		{"toYaml .Values.manager.extraVolumeMounts | nindent 10",
			"toYaml .Values.manager.extraVolumeMounts | nindent 8"},
		{"toYaml .Values.manager.extraVolumes | nindent 8", "toYaml .Values.manager.extraVolumes | nindent 6"},
	}
	for _, replacement := range replacements {
		if strings.Count(s, replacement[0]) != 1 {
			return fmt.Errorf("kubebuilder manager template changed at %q; review the chart extension", replacement[0])
		}
		s = strings.Replace(s, replacement[0], replacement[1], 1)
	}
	start, end := strings.Index(s, "        env:\n"), strings.Index(s, "        image:")
	if start < 0 || end <= start {
		return fmt.Errorf("kubebuilder manager environment boundary changed")
	}
	s = s[:start] + "        env:\n{{ include \"management-engine.managerEnv\" . }}\n" + s[end:]
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		return err
	}
	// The plugin owns every runtime template; the outer gate applies to new resources too.
	if err := filepath.WalkDir(filepath.Join(chart, "templates"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".yaml" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		data = append([]byte("{{- if and .Values.controller.enabled .Values.manager.enabled }}\n"), data...)
		data = append(data, []byte("\n{{- end }}\n")...)
		return os.WriteFile(path, data, 0o644)
	}); err != nil {
		return err
	}
	helpers := filepath.Join(chart, "templates/_helpers.tpl")
	data, err = os.ReadFile(helpers)
	if err != nil {
		return err
	}
	old := "not (.Values.serviceAccount.enabled | default true)"
	if strings.Count(string(data), old) != 1 {
		return fmt.Errorf("kubebuilder ServiceAccount helper changed")
	}
	data = []byte(strings.Replace(string(data), old, "eq .Values.serviceAccount.enabled false", 1))
	if err := os.WriteFile(helpers, data, 0o644); err != nil {
		return err
	}
	return customizeValues(filepath.Join(chart, "values.yaml"))
}

func customizeValues(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return err
	}
	manager := values["manager"].(map[string]any)
	args := []any{}
	for _, raw := range manager["args"].([]any) {
		arg := raw.(string)
		if !strings.HasPrefix(arg, "--kubeconfig=") && !strings.HasPrefix(arg, "--leader-election-namespace=") {
			args = append(args, arg)
		}
	}
	manager["args"] = args
	env := []any{}
	for _, raw := range manager["env"].([]any) {
		if raw.(map[string]any)["name"] != "STATE_NAMESPACE" {
			env = append(env, raw)
		}
	}
	manager["env"] = env
	// The plugin embeds source volumes in the template as well as these defaults.
	// Extra volumes are site additions, not a second rendering of source volumes.
	manager["extraVolumes"] = []any{}
	manager["extraVolumeMounts"] = []any{}
	extra, err := os.ReadFile("hack/chart/values.json")
	if err != nil {
		return err
	}
	patch := map[string]any{}
	if err := json.Unmarshal(extra, &patch); err != nil {
		return err
	}
	merge(values, patch)
	data, err = yaml.Marshal(values)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func merge(dst, src map[string]any) {
	for key, value := range src {
		left, lok := dst[key].(map[string]any)
		right, rok := value.(map[string]any)
		if lok && rok {
			merge(left, right)
		} else {
			dst[key] = value
		}
	}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

func synchronize(src, dst string, check bool) error {
	expected := map[string]bool{}
	drift := []string{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		expected[rel] = true
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		old, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if !bytes.Equal(old, data) {
			drift = append(drift, rel)
			if !check {
				return copyFile(path, filepath.Join(dst, rel))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Only remove obsolete tracked outputs; preserve untracked work and dependency archives.
	tracked, err := exec.Command("git", "ls-files", "-z", "--", dst).Output()
	if err != nil {
		return err
	}
	for path := range strings.SplitSeq(string(tracked), "\x00") {
		if path == "" {
			continue
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		if !expected[rel] {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				continue
			}
			drift = append(drift, rel)
			if !check {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	}
	if err := filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == "charts" {
				return filepath.SkipDir
			}
			return nil
		}
		if !expected[rel] {
			return fmt.Errorf("unexpected chart file %s; preserve it and review its source", rel)
		}
		return nil
	}); err != nil {
		return err
	}
	if check && len(drift) != 0 {
		slices.Sort(drift)
		return fmt.Errorf("chart generation drift; run make chart:\n%s", strings.Join(drift, "\n"))
	}
	return nil
}
