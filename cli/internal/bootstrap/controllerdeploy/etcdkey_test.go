// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestEtcdEncryptionKey(t *testing.T) {
	id := uuid.New()
	active := base64.StdEncoding.EncodeToString([]byte("key-bytes"))
	runner := &fakeRunner{outputs: []fakeOutput{{
		output: `{"apiVersion":"v1","kind":"Secret","data":{"active":"` + active + `","active-since":"MQ=="}}`,
	}}}

	data, err := New(runner, slog.Default()).EtcdEncryptionKey(context.Background(), "/kube.conf", id)
	if err != nil {
		t.Fatalf("EtcdEncryptionKey: %v", err)
	}
	if string(data["active"]) != "key-bytes" {
		t.Errorf("active = %q, want decoded key bytes", data["active"])
	}
	if string(data["active-since"]) != "1" {
		t.Errorf("active-since = %q, want %q", data["active-since"], "1")
	}

	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(runner.calls))
	}
	args := strings.Join(runner.calls[0].args, " ")
	for _, want := range []string{"--kubeconfig /kube.conf", "-n nvke-controller", "get secret etcd-encryption-key-" + id.String(), "-o json"} {
		if !strings.Contains(args, want) {
			t.Errorf("kubectl args %q do not contain %q", args, want)
		}
	}
}

// TestEtcdEncryptionKeyErrorsCarryNoOutput: this read's output is the key, so
// unlike the package's other kubectl reads, a failure must not quote it.
func TestEtcdEncryptionKeyErrorsCarryNoOutput(t *testing.T) {
	const leaked = "OUTPUT-THAT-MUST-NOT-APPEAR"
	for name, out := range map[string]fakeOutput{
		"command failed":  {output: leaked, err: errors.New("exit status 1")},
		"not json":        {output: leaked},
		"no active field": {output: `{"data":{"pending":"` + base64.StdEncoding.EncodeToString([]byte(leaked)) + `"}}`},
	} {
		runner := &fakeRunner{outputs: []fakeOutput{out}}
		_, err := New(runner, slog.Default()).EtcdEncryptionKey(context.Background(), "/kube.conf", uuid.New())
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("%s: error %q carries the command output", name, err)
		}
	}
}
