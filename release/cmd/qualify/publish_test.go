// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

const publishTestResult = `{
  "apiVersion": "nvke.nvidia.com/v1alpha1",
  "kind": "QualificationResult",
  "spec": {"subject": "nvke", "version": "v1.0.0"},
  "status": {
    "combination": {
      "id": "cli-v1.0.0",
      "stream": {"name": "amd64-1.34", "result": "passed"}
    }
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

func TestLoadResultAccepts(t *testing.T) {
	path := writeFile(t, "qualify-result.json", publishTestResult)

	result, err := qualification.LoadResult(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Spec.Subject != "nvke" {
		t.Errorf("subject = %q, want nvke", result.Spec.Subject)
	}
}

func TestLoadResultRejectsMissingFile(t *testing.T) {
	_, err := qualification.LoadResult(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("got nil, want an error for a missing file")
	}
}

func TestLoadResultRejectsMalformedJSON(t *testing.T) {
	path := writeFile(t, "qualify-result.json", "not json")

	_, err := qualification.LoadResult(path)
	if err == nil {
		t.Fatal("got nil, want an error for malformed JSON")
	}
}

func TestLoadResultRejectsWrongKind(t *testing.T) {
	path := writeFile(t, "qualify-result.json", strings.Replace(publishTestResult, "QualificationResult", "QualificationPlan", 1))

	_, err := qualification.LoadResult(path)
	if err == nil || !strings.Contains(err.Error(), "kind is") {
		t.Errorf("err = %v, want it to name the wrong kind", err)
	}
}

func TestLoadResultRejectsWrongAPIVersion(t *testing.T) {
	path := writeFile(t, "qualify-result.json", strings.Replace(publishTestResult, "nvke.nvidia.com/v1alpha1", "nvke.nvidia.com/v2", 1))

	_, err := qualification.LoadResult(path)
	if err == nil || !strings.Contains(err.Error(), "apiVersion is") {
		t.Errorf("err = %v, want it to name the wrong apiVersion", err)
	}
}

func TestLoadResultRejectsUnknownField(t *testing.T) {
	path := writeFile(t, "qualify-result.json", strings.Replace(publishTestResult, `"subject": "nvke"`, `"subject": "nvke", "subjekt": "nvke"`, 1))

	_, err := qualification.LoadResult(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("err = %v, want it to name the unknown field", err)
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
func publishArgs(t *testing.T, resultPath, junitPath, stream string) []string {
	t.Helper()
	return []string{
		"--result", resultPath,
		"--junit", junitPath,
		"--stream", stream,
	}
}

func TestPublishCmdRejectsStreamMismatch(t *testing.T) {
	resultPath := writeFile(t, "qualify-result.json", publishTestResult)
	junitPath := writeFile(t, "qualify-e2e.junit.xml", "<testsuites/>")

	_, err := runPublishCmd(t, publishArgs(t, resultPath, junitPath, "some-other-stream")...)
	if err == nil || !strings.Contains(err.Error(), "does not match the result record's stream") {
		t.Errorf("err = %v, want it to name the stream mismatch", err)
	}
}

func TestResolveBucketRejectsUnset(t *testing.T) {
	t.Setenv(testGridBucketEnvVar, "")
	if _, err := resolveBucket(); err == nil {
		t.Fatal("resolveBucket() returned nil, want an error when TESTGRID_BUCKET is unset")
	}
}

func TestResolveBucketReadsEnvVar(t *testing.T) {
	t.Setenv(testGridBucketEnvVar, "some-other-bucket")
	got, err := resolveBucket()
	if err != nil {
		t.Fatal(err)
	}
	if got != "some-other-bucket" {
		t.Errorf("resolveBucket() = %q, want the env var's value", got)
	}
}

// An errored result (Ginkgo never ran) has no suite to tab it under, so
// publish must skip it before ever reaching buildDestination -- which would
// otherwise try to open a real GCS client.
func TestPublishCmdSkipsErroredResult(t *testing.T) {
	resultPath := writeFile(t, "qualify-result.json", strings.Replace(publishTestResult, `"result": "passed"`, `"result": "errored"`, 1))
	junitPath := filepath.Join(t.TempDir(), "does-not-exist.xml")

	stdout, err := runPublishCmd(t, publishArgs(t, resultPath, junitPath, "amd64-1.34")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "skipping publish") {
		t.Errorf("stdout = %q, want it to say the publish was skipped", stdout)
	}
}

func TestBuildDestinationRejectsUnknownDestination(t *testing.T) {
	_, _, err := buildDestination(nil, "not-a-real-destination", "test-bucket") //nolint:staticcheck // ctx unused before the error path
	if err == nil || !strings.Contains(err.Error(), "unknown --destination") {
		t.Errorf("err = %v, want it to name the bad destination", err)
	}
}
