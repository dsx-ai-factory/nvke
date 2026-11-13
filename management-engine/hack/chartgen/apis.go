// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func generateAPIs(binary, stage, chart string) error {
	data, err := os.ReadFile(filepath.Join(chart, "values.yaml"))
	if err != nil {
		return err
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return err
	}
	for _, component := range []struct{ surface, key string }{{"tenant", "tenantAPI"}, {"operator", "operatorAPI"}} {
		defaults, err := generateAPI(binary, stage, chart, component.surface, component.key)
		if err != nil {
			return fmt.Errorf("generate %s API: %w", component.surface, err)
		}
		values[component.key] = defaults
	}
	data, err = yaml.Marshal(values)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(chart, "values.yaml"), data, 0o644)
}

func generateAPI(binary, stage, chart, surface, key string) (map[string]any, error) {
	data, err := os.ReadFile("config/dev/" + surface + "-api.yaml")
	if err != nil {
		return nil, err
	}
	var source appsv1.Deployment
	if err := yaml.Unmarshal(data, &source); err != nil {
		return nil, err
	}
	if len(source.Spec.Template.Spec.Containers) != 1 || source.Spec.Template.Spec.Containers[0].Name != "api" {
		return nil, fmt.Errorf("expected one canonical api container")
	}
	container := &source.Spec.Template.Spec.Containers[0]
	if len(container.Ports) != 1 || container.Ports[0].Name != "http" || container.Ports[0].ContainerPort != 8080 {
		return nil, fmt.Errorf("canonical API must expose the named http port on 8080")
	}
	args, err := filterNamedArgs(container.Args, "namespace", "kubeconfig", "context", "listen")
	if err != nil {
		return nil, err
	}
	probes, err := yaml.Marshal(map[string]any{
		"livenessProbe":  container.LivenessProbe,
		"readinessProbe": container.ReadinessProbe,
	})
	if err != nil {
		return nil, err
	}
	probeDefaults := map[string]any{}
	if err := yaml.Unmarshal(probes, &probeDefaults); err != nil {
		return nil, err
	}
	// The pinned plugin parameterizes one controller-manager. Normalize only its temporary input.
	source.Name = "management-engine-controller-manager"
	container.Name = "manager"
	container.Args = append([]string{"--listen=:8080"}, args...)
	input, err := yaml.Marshal(source)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(stage, surface)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := copyFile("PROJECT", filepath.Join(dir, "PROJECT")); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "input.yaml"), input, 0o644); err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, "edit", "--plugins=helm/v2-alpha", "--manifests=./input.yaml", "--output-dir=generated")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("kubebuilder: %w\n%s", err, output)
	}
	generated := filepath.Join(dir, "generated/chart")
	data, err = os.ReadFile(filepath.Join(generated, "values.yaml"))
	if err != nil {
		return nil, err
	}
	var raw struct {
		Manager map[string]any `json:"manager"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	defaults := raw.Manager
	if defaults == nil {
		return nil, fmt.Errorf("plugin omitted manager values")
	}
	defaults["enabled"] = false
	if defaults["pod"] == nil {
		defaults["pod"] = map[string]any{"labels": map[string]any{}, "annotations": map[string]any{}}
	}
	defaults["args"] = args
	defaults["extraVolumes"] = []any{}
	defaults["extraVolumeMounts"] = []any{}
	defaults["serviceAccount"] = map[string]any{"create": true, "name": ""}
	defaults["stateAccess"] = map[string]any{
		"mode": "inCluster", "context": "",
		"kubeconfigSecret": map[string]any{"name": "", "key": "kubeconfig"},
	}
	defaults["service"] = map[string]any{"port": 8080}
	merge(defaults, probeDefaults)
	if surface == "tenant" {
		defaults["namespace"] = ""
	}
	data, err = os.ReadFile(filepath.Join(generated, "templates/manager/manager.yaml"))
	if err != nil {
		return nil, err
	}
	rendered, err := customizeAPI(string(data), surface, key)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(chart, "templates/apis/"+surface+".yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return defaults, os.WriteFile(path, []byte(rendered), 0o644)
}

func customizeAPI(template, surface, key string) (string, error) {
	oldAccount := `      serviceAccountName: {{ include "management-engine.resourceName" ` +
		`(dict "suffix" "` + surface + `-api" "context" $) }}`
	newAccount := `      serviceAccountName: {{ include "management-engine.apiServiceAccount" ` +
		`(dict "root" $ "api" .Values.manager "surface" "` + surface + `") }}`
	replacements := [][2]string{
		{`"suffix" "controller-manager"`, `"suffix" "` + surface + `-api"`},
		{"        name: manager", "        name: api"},
		{"      automountServiceAccountToken: true",
			`      automountServiceAccountToken: {{ eq .Values.manager.stateAccess.mode "inCluster" }}`},
		{"        {{- range .Values.manager.args }}\n        - {{ . }}\n        {{- end }}",
			`{{ include "management-engine.apiArgs" (dict "api" .Values.manager "surface" "` + surface + `") | indent 8 }}`},
		{"    matchLabels:\n", "    matchLabels:\n      app.kubernetes.io/instance: {{ .Release.Name }}\n" +
			"      app.kubernetes.io/component: " + surface + "-api\n"},
		{oldAccount, newAccount},
	}
	for _, pair := range replacements {
		if strings.Count(template, pair[0]) != 1 {
			return "", fmt.Errorf("API plugin template changed at %q", pair[0])
		}
		template = strings.Replace(template, pair[0], pair[1], 1)
	}
	label := `{{ include "management-engine.name" . }}-` + surface + `-api`
	if strings.Count(template, label) != 2 {
		return "", fmt.Errorf("API plugin selector labels changed")
	}
	template = strings.ReplaceAll(template, label,
		`{{ include "management-engine.name" . | quote }}`)
	podLabel := `        app.kubernetes.io/name: {{ include "management-engine.name" . | quote }}`
	if strings.Count(template, podLabel) != 1 {
		return "", fmt.Errorf("API plugin pod labels changed")
	}
	template = strings.Replace(template, podLabel,
		"        app.kubernetes.io/component: "+surface+"-api\n"+podLabel, 1)
	template = strings.Replace(template, `omit . "app.kubernetes.io/name"`,
		`omit . "app.kubernetes.io/component" "app.kubernetes.io/name"`, 1)
	for _, field := range []string{"livenessProbe", "readinessProbe"} {
		var err error
		template, err = replaceAPIProbe(template, field)
		if err != nil {
			return "", err
		}
	}
	// Preserve canonical volumes and expose additions even when the source has none.
	for _, field := range []struct {
		name, helper, before, extras string
		indent                       int
	}{
		{"volumeMounts", "apiMount", "        name: api", "extraVolumeMounts", 8},
		{"volumes", "apiVolume", "      serviceAccountName:", "extraVolumes", 6},
	} {
		marker := strings.Repeat(" ", field.indent) + field.name + ":\n"
		items := fmt.Sprintf(`{{ include "management-engine.%s" .Values.manager | indent %d }}`+"\n",
			field.helper, field.indent)
		switch strings.Count(template, marker) {
		case 1:
			template = strings.Replace(template, marker, marker+items, 1)
		case 0:
			block := fmt.Sprintf(`{{ if or (eq .Values.manager.stateAccess.mode "remote") .Values.manager.%s }}`+"\n",
				field.extras)
			block += marker + items + fmt.Sprintf(
				`{{ with .Values.manager.%s }}{{ toYaml . | indent %d }}{{ end }}`+"\n{{ end }}\n",
				field.extras, field.indent)
			if strings.Count(template, field.before) != 1 {
				return "", fmt.Errorf("API plugin template changed at %q", field.before)
			}
			template = strings.Replace(template, field.before, block+field.before, 1)
		default:
			return "", fmt.Errorf("API plugin emitted multiple %s fields", field.name)
		}
	}
	template = strings.ReplaceAll(template, "toYaml .Values.manager.extraVolumeMounts | nindent 10",
		"toYaml .Values.manager.extraVolumeMounts | nindent 8")
	template = strings.ReplaceAll(template, "toYaml .Values.manager.extraVolumes | nindent 8",
		"toYaml .Values.manager.extraVolumes | nindent 6")
	template = strings.ReplaceAll(template, "{{ .Release.Namespace }}", "{{ .Release.Namespace | quote }}")
	template = strings.ReplaceAll(template, "{{ .Release.Name }}", "{{ .Release.Name | quote }}")
	return strings.ReplaceAll(template, ".Values.manager", ".Values."+key), nil
}

func replaceAPIProbe(template, field string) (string, error) {
	marker := "        " + field + ":\n"
	if strings.Count(template, marker) != 1 {
		return "", fmt.Errorf("API plugin omitted %s", field)
	}
	start := strings.Index(template, marker)
	end := start + len(marker)
	for _, line := range strings.SplitAfter(template[end:], "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if strings.TrimSpace(line) != "" && len(line)-len(trimmed) <= 8 {
			break
		}
		end += len(line)
	}
	replacement := marker + "{{ toYaml .Values.manager." + field + " | indent 10 }}\n"
	return template[:start] + replacement + template[end:], nil
}
