// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	encryptionLoadedConfigMetric = "apiserver_encryption_config_controller_last_config_info"
)

// apiserverEncryptionState is private confirmation data. Config and apiserver
// fingerprints must never be added to logs, RPC responses or workflow history.
type apiserverEncryptionState struct {
	configHash     string
	apiserverID    string
	processStarted float64
}

func encryptionConfigContentHash(raw []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}

func (n *NodeAgentServer) encryptionConfigHash() (string, error) {
	raw, err := os.ReadFile(filepath.Join(n.RootPath, "/etc/kubernetes/kube-api-encryption/encryption-config.yaml"))
	if err != nil || len(raw) == 0 {
		return "", status.Error(codes.FailedPrecondition, "cannot read encryption config for reload confirmation")
	}
	return encryptionConfigContentHash(raw), nil
}

func validEncryptionMetricHash(hash string) bool {
	if !strings.HasPrefix(hash, "sha256:") || len(hash) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(hash, "sha256:"))
	return err == nil
}

// parseApiserverEncryptionState uses the Prometheus parser, retaining no raw
// samples in errors. A local process exports exactly one currently loaded config.
func parseApiserverEncryptionState(raw []byte) (apiserverEncryptionState, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(raw))
	if err != nil {
		return apiserverEncryptionState{}, errors.New("invalid apiserver metrics exposition")
	}
	invalid := errors.New("missing or invalid apiserver encryption identity metrics")
	configs := families[encryptionLoadedConfigMetric].GetMetric()
	processes := families["process_start_time_seconds"].GetMetric()
	if len(configs) != 1 || len(processes) != 1 {
		return apiserverEncryptionState{}, invalid
	}
	configValue, ok := encryptionGaugeValue(configs[0])
	if !ok || configValue != 1 {
		return apiserverEncryptionState{}, invalid
	}
	labels, ok := encryptionMetricLabels(configs[0])
	if !ok || len(labels) != 2 || !validEncryptionMetricHash(labels["hash"]) {
		return apiserverEncryptionState{}, invalid
	}
	id, exists := labels["apiserver_id_hash"]
	// Kubernetes deliberately emits an empty identity if APIServerID is empty.
	if !exists || (id != "" && !validEncryptionMetricHash(id)) {
		return apiserverEncryptionState{}, invalid
	}
	started, ok := encryptionGaugeValue(processes[0])
	if !ok || started <= 0 || len(processes[0].GetLabel()) != 0 {
		return apiserverEncryptionState{}, invalid
	}
	return apiserverEncryptionState{configHash: labels["hash"], apiserverID: id, processStarted: started}, nil
}

func encryptionMetricValue(metric *dto.Metric) (float64, bool) {
	var value float64
	switch {
	case metric.GetGauge() != nil:
		value = metric.GetGauge().GetValue()
	case metric.GetCounter() != nil:
		value = metric.GetCounter().GetValue()
	case metric.GetUntyped() != nil:
		value = metric.GetUntyped().GetValue()
	default:
		return 0, false
	}
	return value, !math.IsNaN(value) && !math.IsInf(value, 0)
}

func encryptionGaugeValue(metric *dto.Metric) (float64, bool) {
	if metric.GetCounter() != nil {
		return 0, false
	}
	return encryptionMetricValue(metric)
}

func encryptionMetricLabels(metric *dto.Metric) (map[string]string, bool) {
	labels := make(map[string]string, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		if _, exists := labels[label.GetName()]; exists {
			return nil, false
		}
		labels[label.GetName()] = label.GetValue()
	}
	return labels, true
}

func (n *NodeAgentServer) getApiserverEncryptionState(ctx context.Context) (apiserverEncryptionState, error) {
	if n.clientRegistry == nil {
		return apiserverEncryptionState{}, errors.New("k8s client registry is not configured")
	}
	client, err := n.clientRegistry.LocalClient()
	if err != nil || client == nil {
		return apiserverEncryptionState{}, errors.New("local apiserver metrics client is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, apiserverMetricsProbeTimeout)
	defer cancel()
	raw, err := client.RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		// An upstream error may contain a response body with private fingerprints.
		return apiserverEncryptionState{}, errors.New("local apiserver metrics are unavailable")
	}
	return parseApiserverEncryptionState(raw)
}

func (n *NodeAgentServer) confirmEncryptionConfig(ctx context.Context, walPath, opID, targetHash string, observed apiserverEncryptionState) (bool, error) {
	n.encryptionMu.Lock()
	defer n.encryptionMu.Unlock()
	saved, exists, err := readEncRotationWAL(walPath)
	if err != nil {
		return false, err
	}
	if exists && saved.OpID != opID {
		return false, status.Error(codes.FailedPrecondition, "encryption operation ownership changed during confirmation")
	}
	onDisk, err := n.encryptionConfigHash()
	if err != nil {
		return false, err
	}
	if onDisk != targetHash {
		return false, status.Error(codes.FailedPrecondition, "encryption config changed during confirmation")
	}
	confirmed, err := n.getApiserverEncryptionState(ctx)
	if err != nil {
		return false, err
	}
	if confirmed.configHash != targetHash || confirmed.processStarted != observed.processStarted || confirmed.apiserverID != observed.apiserverID {
		return false, nil
	}
	onDisk, err = n.encryptionConfigHash()
	if err != nil {
		return false, err
	}
	if onDisk != targetHash {
		return false, status.Error(codes.FailedPrecondition, "encryption config changed during confirmation")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Re-read even while holding the agent mutex: independent filesystem writers
	// do not share it. Never remove a checkpoint installed by another operation.
	saved, exists, err = readEncRotationWAL(walPath)
	if err != nil {
		return false, err
	}
	if exists && saved.OpID != opID {
		return false, status.Error(codes.FailedPrecondition, "encryption operation ownership changed during confirmation")
	}
	if !exists {
		return true, nil
	}
	if err := os.Remove(walPath); err != nil {
		return false, status.Errorf(codes.Internal, "remove rotation WAL: %v", err)
	}
	return true, nil
}

func (n *NodeAgentServer) waitForEncryptionConfig(ctx context.Context, walPath, opID, targetHash string) error {
	var metricDownSince time.Time
	for {
		if ctx.Err() != nil {
			return encryptionConfirmationContextError(ctx)
		}
		observed, err := n.getApiserverEncryptionState(ctx)
		if err == nil && observed.configHash == targetHash {
			var confirmed bool
			confirmed, err = n.confirmEncryptionConfig(ctx, walPath, opID, targetHash, observed)
			if confirmed {
				return nil
			}
		}
		// Only metrics failures count as an outage; a failed WAL removal is local.
		if code := status.Code(err); code == codes.FailedPrecondition || code == codes.Internal {
			return err
		}
		if err != nil {
			if metricDownSince.IsZero() {
				metricDownSince = time.Now()
			} else if time.Since(metricDownSince) >= encryptionMetricsFailLimit {
				return status.Error(codes.Unavailable, "cannot confirm loaded encryption config; retry after local apiserver metrics recover")
			}
		} else {
			metricDownSince = time.Time{}
		}
		timer := time.NewTimer(n.reloadPoll())
		select {
		case <-ctx.Done():
			timer.Stop()
			return encryptionConfirmationContextError(ctx)
		case <-timer.C:
		}
	}
}

func encryptionConfirmationContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return status.Error(codes.Canceled, "encryption config confirmation canceled; retry is safe")
	}
	return status.Error(codes.DeadlineExceeded, "timed out waiting for apiserver to load encryption config; retry is safe")
}
