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
	"time"
)

const operationFileName = "operation.json"

var (
	operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

	// ErrOperationConflict means an operation ID was reused for a different request.
	ErrOperationConflict = errors.New("node-agent upgrade operation conflicts with existing request")
	// ErrOperationActive means another non-terminal node-agent operation owns the node.
	ErrOperationActive = errors.New("another node-agent upgrade operation is active")
)

// Store persists updater operations in one directory per caller-provided ID.
type Store struct {
	Root string
}

// NewStore returns a durable updater operation store rooted at root.
func NewStore(root string) *Store {
	return &Store{Root: root}
}

// ValidateOperationID rejects values that cannot safely be used as systemd
// instance names and operation-directory names.
func ValidateOperationID(id string) error {
	if !operationIDPattern.MatchString(id) {
		return fmt.Errorf("operation_id must match %s", operationIDPattern.String())
	}
	return nil
}

// OperationDir returns the validated directory for an operation.
func (s *Store) OperationDir(id string) (string, error) {
	if err := ValidateOperationID(id); err != nil {
		return "", err
	}
	if s == nil || s.Root == "" {
		return "", errors.New("node-agent upgrade store root is empty")
	}
	return filepath.Join(s.Root, id), nil
}

// Create persists op, returning an identical existing request idempotently.
func (s *Store) Create(op Operation) (Operation, bool, error) {
	dir, err := s.OperationDir(op.ID)
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
		return Operation{}, false, fmt.Errorf("%w: %s is %s", ErrOperationActive, active.ID, active.Phase)
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
	existing, err := s.Load(request.ID)
	if errors.Is(err, os.ErrNotExist) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	if !sameRequest(existing, request) {
		return Operation{}, false, ErrOperationConflict
	}
	return existing, true, nil
}

// Load reads a persisted operation.
func (s *Store) Load(id string) (Operation, error) {
	dir, err := s.OperationDir(id)
	if err != nil {
		return Operation{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, operationFileName))
	if err != nil {
		return Operation{}, err
	}
	var op Operation
	if err := json.Unmarshal(data, &op); err != nil {
		return Operation{}, fmt.Errorf("decoding operation %s: %w", id, err)
	}
	if op.ID != id {
		return Operation{}, fmt.Errorf("operation file for %s contains id %q", id, op.ID)
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
		if !entry.IsDir() || ValidateOperationID(entry.Name()) != nil {
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
func (s *Store) SetStatus(id string, phase Phase, step, errorMessage string) (Operation, error) {
	op, err := s.Load(id)
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

// SetPreviousPackageVersion records the exact Debian version retained for rollback.
func (s *Store) SetPreviousPackageVersion(id, version string) (Operation, error) {
	op, err := s.Load(id)
	if err != nil {
		return Operation{}, err
	}
	op.PreviousPackageVersion = version
	op.UpdatedAt = time.Now().UTC()
	if err := s.write(op); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func (s *Store) write(op Operation) error {
	dir, err := s.OperationDir(op.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding operation %s: %w", op.ID, err)
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

func sameRequest(existing, requested Operation) bool {
	if existing.ID != requested.ID || existing.Action != requested.Action {
		return false
	}
	switch existing.Action {
	case ActionUpgrade:
		return existing.TargetVersion == requested.TargetVersion && existing.Method == requested.Method
	case ActionRollback:
		return existing.RestoreFromOperationID == requested.RestoreFromOperationID
	default:
		return false
	}
}
