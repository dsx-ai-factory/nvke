// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentupgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

const operationFileName = "operation.json"

var (
	requestKeyPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)_(apt|bundle)$`)

	// ErrOperationActive means another non-terminal node-agent operation owns the node.
	ErrOperationActive = errors.New("another node-agent upgrade operation is active")
)

// Store persists updater state in one directory per target version and method.
type Store struct {
	Root string
}

// NewStore returns a durable updater operation store rooted at root.
func NewStore(root string) *Store {
	return &Store{Root: root}
}

// ValidateRequestKey rejects values that cannot safely be used as systemd
// instance names and operation-directory names.
func ValidateRequestKey(key string) error {
	if !requestKeyPattern.MatchString(key) {
		return fmt.Errorf("request pair must match %s", requestKeyPattern.String())
	}
	return nil
}

// OperationDir returns the validated directory for an operation.
func (s *Store) OperationDir(key string) (string, error) {
	if err := ValidateRequestKey(key); err != nil {
		return "", err
	}
	if s == nil || s.Root == "" {
		return "", errors.New("node-agent upgrade store root is empty")
	}
	return filepath.Join(s.Root, key), nil
}

// Create persists op, returning an identical existing request idempotently.
func (s *Store) Create(op Operation) (Operation, bool, error) {
	dir, err := s.OperationDir(op.Key())
	if err != nil {
		return Operation{}, false, err
	}
	if existing, found, loadErr := s.Existing(op); loadErr != nil {
		return Operation{}, false, loadErr
	} else if found {
		return existing, false, nil
	}

	active, err := s.Active()
	if err != nil {
		return Operation{}, false, err
	}
	if active != nil {
		return Operation{}, false, fmt.Errorf("%w: %s is %s", ErrOperationActive, active.Key(), active.Phase)
	}

	// A completed request can be retried with a new artifact. Retain only files
	// captured for this installation, so restoration cannot use an older retry.
	if err := os.RemoveAll(dir); err != nil {
		return Operation{}, false, fmt.Errorf("clearing completed request files: %w", err)
	}
	now := time.Now().UTC()
	op.Phase = PhasePending
	op.Step = "queued"
	op.ErrorMessage = ""
	op.CreatedAt = now
	op.UpdatedAt = now
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Operation{}, false, fmt.Errorf("creating operation directory: %w", err)
	}
	if err := s.write(op); err != nil {
		return Operation{}, false, err
	}
	return op, true, nil
}

// Existing returns an idempotently matching operation before the caller does
// artifact resolution or other external work.
func (s *Store) Existing(request Operation) (Operation, bool, error) {
	existing, err := s.Load(request.Key())
	if errors.Is(err, os.ErrNotExist) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	if existing.Terminal() {
		return Operation{}, false, nil
	}
	return existing, true, nil
}

// Load reads a persisted operation.
func (s *Store) Load(key string) (Operation, error) {
	dir, err := s.OperationDir(key)
	if err != nil {
		return Operation{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, operationFileName))
	if err != nil {
		return Operation{}, err
	}
	var op Operation
	if err := json.Unmarshal(data, &op); err != nil {
		return Operation{}, fmt.Errorf("decoding operation %s: %w", key, err)
	}
	if op.Key() != key {
		return Operation{}, fmt.Errorf("operation file for %s contains request pair %q", key, op.Key())
	}
	return op, nil
}

// Active returns the first non-terminal operation, if any.
func (s *Store) Active() (*Operation, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing node-agent upgrade operations: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || ValidateRequestKey(entry.Name()) != nil {
			continue
		}
		op, loadErr := s.Load(entry.Name())
		if errors.Is(loadErr, os.ErrNotExist) {
			continue
		}
		if loadErr != nil {
			return nil, loadErr
		}
		if !op.Terminal() {
			return &op, nil
		}
	}
	return nil, nil
}

// SetStatus atomically updates the operation phase, step, and error message.
func (s *Store) SetStatus(key string, phase Phase, step, errorMessage string) (Operation, error) {
	op, err := s.Load(key)
	if err != nil {
		return Operation{}, err
	}
	op.Phase = phase
	op.Step = step
	op.ErrorMessage = errorMessage
	op.UpdatedAt = time.Now().UTC()
	if err := s.write(op); err != nil {
		return Operation{}, err
	}
	return op, nil
}

// SetPreviousPackage records the exact Debian package retained for restoration.
func (s *Store) SetPreviousPackage(key, name, version string) (Operation, error) {
	op, err := s.Load(key)
	if err != nil {
		return Operation{}, err
	}
	op.PreviousPackageName = name
	op.PreviousPackageVersion = version
	op.UpdatedAt = time.Now().UTC()
	if err := s.write(op); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func (s *Store) write(op Operation) error {
	dir, err := s.OperationDir(op.Key())
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding operation %s: %w", op.Key(), err)
	}
	tmp, err := os.CreateTemp(dir, ".operation-*.json")
	if err != nil {
		return fmt.Errorf("creating temporary operation file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting operation file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing operation file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing operation file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing operation file: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, operationFileName)); err != nil {
		return fmt.Errorf("publishing operation file: %w", err)
	}
	return nil
}

// Current returns the newest durable operation, including completed attempts.
func (s *Store) Current() (*Operation, error) {
	active, err := s.Active()
	if err != nil || active != nil {
		return active, err
	}
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing upgrade status: %w", err)
	}
	var current *Operation
	for _, entry := range entries {
		if !entry.IsDir() || ValidateRequestKey(entry.Name()) != nil {
			continue
		}
		op, err := s.Load(entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if current == nil || op.CreatedAt.After(current.CreatedAt) {
			current = &op
		}
	}
	return current, nil
}

// Lock serializes admission with the bootstrap updater in another process.
func (s *Store) Lock() (func(), error) {
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(s.Root, "upgrade.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

// Prune bounds completed attempts and retains assets for active and recent requests.
// Callers must hold Lock while pruning and admitting a new attempt.
func (s *Store) Prune() error {
	operations, err := s.operations()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	latest := map[string]Operation{}
	for _, op := range operations {
		if !op.Terminal() {
			keep[op.Key()] = true
			continue
		}
		category := "failure"
		if op.Phase == PhaseSucceeded {
			category = "success"
		}
		if previous, ok := latest[category]; !ok || op.CreatedAt.After(previous.CreatedAt) {
			latest[category] = op
		}
	}
	for _, op := range latest {
		keep[op.Key()] = true
	}
	for _, op := range operations {
		if keep[op.Key()] {
			continue
		}
		dir, err := s.OperationDir(op.Key())
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("pruning completed upgrade: %w", err)
		}
	}
	return nil
}

func (s *Store) operations() ([]Operation, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing upgrade attempts: %w", err)
	}
	var operations []Operation
	for _, entry := range entries {
		if !entry.IsDir() || ValidateRequestKey(entry.Name()) != nil {
			continue
		}
		op, err := s.Load(entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	return operations, nil
}
