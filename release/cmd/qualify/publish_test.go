// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

const publishTestResult = `{
  "apiVersion": "nvke.nvidia.com/v1alpha1",
  "kind": "QualificationResult",
  "spec": {"subject": "nvke", "type": "site-controller", "version": "v1.0.0"},
  "status": {
    "combinations": [{
      "id": "cli-v1.0.0",
      "streams": [{"name": "amd64-1.34", "result": "PASS"}]
    }]
  }
}`

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// publishTestResultValue is publishTestResult's one combination/one stream,
// as a Go value.
func publishTestResultValue() qualification.Result {
	return qualification.Result{
		APIVersion: "nvke.nvidia.com/v1alpha1",
		Kind:       qualification.KindQualificationResult,
		Spec:       qualification.ResultSpec{Subject: "nvke", Type: "site-controller", Version: "v1.0.0"},
		Status: qualification.ResultStatus{
			Combinations: []qualification.ResultCombination{{
				ID:      "cli-v1.0.0",
				Streams: []qualification.ResultStream{{Name: "amd64-1.34", Result: "PASS"}},
			}},
		},
	}
}

func writeResult(t *testing.T, result qualification.Result) string {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, "qualify-result.json", string(b))
}

func TestLoadResultAccepts(t *testing.T) {
	path := writeFile(t, "qualify-result.json", publishTestResult)

	result, err := loadResult(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Spec.Subject != "nvke" {
		t.Errorf("subject = %q, want nvke", result.Spec.Subject)
	}
}

func TestLoadResultRejectsMissingFile(t *testing.T) {
	_, err := loadResult(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("got nil, want an error for a missing file")
	}
}

func TestLoadResultRejectsMalformedJSON(t *testing.T) {
	path := writeFile(t, "qualify-result.json", "not json")

	_, err := loadResult(path)
	if err == nil {
		t.Fatal("got nil, want an error for malformed JSON")
	}
}

func TestLoadResultRejectsWrongKind(t *testing.T) {
	path := writeFile(t, "qualify-result.json", strings.Replace(publishTestResult, "QualificationResult", "QualificationPlan", 1))

	_, err := loadResult(path)
	if err == nil || !strings.Contains(err.Error(), "kind is") {
		t.Errorf("err = %v, want it to name the wrong kind", err)
	}
}

func runPublishCmd(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := publishCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return buf.String(), err
}

// publishArgs returns a full, otherwise-valid argument list for publishCmd,
// so each rejection test only has to override the one flag/file it means to
// break -- all of these stop before buildDestination ever runs, so none of
// them touch a real GCS bucket.
func publishArgs(t *testing.T, resultPath, junitPath, combination, stream string) []string {
	t.Helper()
	return []string{
		"--result", resultPath,
		"--junit", junitPath,
		"--combination", combination,
		"--stream", stream,
	}
}

func TestPublishCmdRejectsTooManyCombinations(t *testing.T) {
	result := publishTestResultValue()
	result.Status.Combinations = append(result.Status.Combinations, qualification.ResultCombination{
		ID:      "other",
		Streams: []qualification.ResultStream{{Name: "x", Result: "PASS"}},
	})
	resultPath := writeResult(t, result)
	junitPath := writeFile(t, "qualify-e2e.junit.xml", "<testsuites/>")

	_, err := runPublishCmd(t, publishArgs(t, resultPath, junitPath, "cli-v1.0.0", "amd64-1.34")...)
	if err == nil || !strings.Contains(err.Error(), "want exactly 1") {
		t.Errorf("err = %v, want it to say want exactly 1 combination", err)
	}
}

func TestPublishCmdRejectsTooManyStreams(t *testing.T) {
	result := publishTestResultValue()
	result.Status.Combinations[0].Streams = append(result.Status.Combinations[0].Streams,
		qualification.ResultStream{Name: "arm64-1.34", Result: "PASS"})
	resultPath := writeResult(t, result)
	junitPath := writeFile(t, "qualify-e2e.junit.xml", "<testsuites/>")

	_, err := runPublishCmd(t, publishArgs(t, resultPath, junitPath, "cli-v1.0.0", "amd64-1.34")...)
	if err == nil || !strings.Contains(err.Error(), "want exactly 1") {
		t.Errorf("err = %v, want it to say want exactly 1 stream", err)
	}
}

func TestPublishCmdRejectsCombinationMismatch(t *testing.T) {
	resultPath := writeFile(t, "qualify-result.json", publishTestResult)
	junitPath := writeFile(t, "qualify-e2e.junit.xml", "<testsuites/>")

	_, err := runPublishCmd(t, publishArgs(t, resultPath, junitPath, "some-other-combination", "amd64-1.34")...)
	if err == nil || !strings.Contains(err.Error(), "does not match the result record's combination") {
		t.Errorf("err = %v, want it to name the combination mismatch", err)
	}
}

func TestPublishCmdRejectsStreamMismatch(t *testing.T) {
	resultPath := writeFile(t, "qualify-result.json", publishTestResult)
	junitPath := writeFile(t, "qualify-e2e.junit.xml", "<testsuites/>")

	_, err := runPublishCmd(t, publishArgs(t, resultPath, junitPath, "cli-v1.0.0", "some-other-stream")...)
	if err == nil || !strings.Contains(err.Error(), "does not match the result record's stream") {
		t.Errorf("err = %v, want it to name the stream mismatch", err)
	}
}

func TestPublishCmdRejectsMissingJUnit(t *testing.T) {
	resultPath := writeFile(t, "qualify-result.json", publishTestResult)

	_, err := runPublishCmd(t, publishArgs(t, resultPath, filepath.Join(t.TempDir(), "does-not-exist.xml"), "cli-v1.0.0", "amd64-1.34")...)
	if err == nil || !strings.Contains(err.Error(), "JUnit report") {
		t.Errorf("err = %v, want it to name the JUnit report", err)
	}
}

func TestBuildDestinationRejectsUnknownEnvironment(t *testing.T) {
	_, _, err := buildDestination(nil, "testgrid", "dev") //nolint:staticcheck // ctx unused before the error path
	if err == nil || !strings.Contains(err.Error(), "is not staging or prod") {
		t.Errorf("err = %v, want it to name the bad environment", err)
	}
}

func TestBuildDestinationRejectsUnknownDestination(t *testing.T) {
	_, _, err := buildDestination(nil, "not-a-real-destination", "staging") //nolint:staticcheck // ctx unused before the error path
	if err == nil || !strings.Contains(err.Error(), "unknown --destination") {
		t.Errorf("err = %v, want it to name the bad destination", err)
	}
}
