// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// A simple wrapper around apt tools to manage deb packages on Ubuntus.

package deb

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

type Package struct {
	// Package name
	Name string
	// Package version
	Version string
	// Status is abbreviated package status (as three characters - desired status, package status, error flags).
	// See https://manpages.ubuntu.com/manpages/focal/man1/dpkg-query.1.html
	Status string
}

func List(ctx context.Context) (iter.Seq[Package], error) {
	cmd := exec.CommandContext(
		ctx,
		"dpkg-query",
		// See https://manpages.ubuntu.com/manpages/focal/man1/dpkg-query.1.html
		"--show", // Show information on package
		"--showformat=${Package} ${Version} ${db:Status-Abbrev}\n",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("unable to list installed packages: %w", err)
	}

	return func(yield func(Package) bool) {
		for line := range strings.Lines(string(output)) {
			fields := strings.Fields(line)

			if len(fields) != 3 {
				slog.ErrorContext(ctx, "unexpected fields in dpkg-query output", "line", line)
				continue
			}

			pak := Package{
				Name:    fields[0],
				Version: fields[1],
				Status:  fields[2],
			}

			if !yield(pak) {
				return
			}
		}
	}, nil
}

func aptget(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "apt-get", args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")

	joined := strings.Join(args, " ")
	slog.InfoContext(ctx, "calling apt-get", "args", joined)

	output, err := cmd.CombinedOutput()
	result := string(output)
	slog.DebugContext(ctx, "apt-get done", "output", result)

	if err != nil {
		return fmt.Errorf("failed to run apt %q: %w\nOutput: %s", joined, err, result)
	}

	return nil
}

// Update calls apt-get update
func Update(ctx context.Context) error {
	return aptget(ctx, "update")
}

// Install calls apt-get install <package> or <package=version>
func Install(ctx context.Context, name ...string) error {
	if len(name) == 0 {
		return errors.New("no packages to install")
	}

	args := append([]string{
		"install",
		"--no-install-recommends",
		"--quiet",
		"--assume-yes",
		"--allow-downgrades", // In case we have a newer version installed and want rollback.
	}, name...)

	err := aptget(ctx, args...)
	if err != nil {
		return fmt.Errorf("failed to install %q: %w", name, err)
	}

	return nil
}

func Remove(ctx context.Context, purge bool, name ...string) error {
	if len(name) == 0 {
		return errors.New("no packages to remove")
	}

	args := []string{
		"remove",
		"--quiet",
		"--assume-yes",
		"--auto-remove",
	}

	if purge {
		args = append(args, "--purge")
	}

	args = append(args, name...)

	return aptget(ctx, args...)
}

const (
	aptKeyringsDir     = "/etc/apt/keyrings"
	aptPreferencesDir  = "/etc/apt/preferences.d"
	aptSourcesListDDir = "/etc/apt/sources.list.d"
)

// AptRepoConfig is the input for ConfigureAptRepo.
type AptRepoConfig struct {
	KeyringName         string
	RepoName            string
	SigningKey          string
	RepoPriority        int32
	DebURI              string
	Suite               string
	Components          string
	SourcesListBasename string
}

// ConfigureAptRepo writes /etc/apt/keyrings/<keyringName>.gpg,
// /etc/apt/sources.list.d/<name>.list when deb_uri and suite are set,
// optionally /etc/apt/preferences.d/nvke-<keyringName>.pref, then apt-get update.
func ConfigureAptRepo(ctx context.Context, cfg AptRepoConfig) (string, error) {
	basename, baseErr := aptKeyringBasename(cfg.KeyringName)
	if baseErr != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "validate keyring basename", "error", baseErr, "keyring_name", cfg.KeyringName)
		return "", baseErr
	}

	repoName := strings.TrimSpace(cfg.RepoName)
	if repoName == "" {
		err := errors.New("repo_name is required")
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "validate repo_name", "error", err)
		return "", err
	}
	if strings.ContainsAny(repoName, "\n\r") {
		err := errors.New("repo_name must be a single line")
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "validate repo_name", "error", err, "repo_name", repoName)
		return "", err
	}
	if strings.TrimSpace(cfg.SigningKey) == "" {
		err := errors.New("repo_signing_key is required")
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "validate signing key", "error", err)
		return "", err
	}

	debURI := strings.TrimSpace(cfg.DebURI)
	suite := strings.TrimSpace(cfg.Suite)
	components := strings.TrimSpace(cfg.Components)
	if components == "" {
		components = "main"
	}

	if debURI == "" || suite == "" {
		err := errors.New("deb_uri and suite are required to configure the apt source")
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "validate deb source", "error", err, "deb_uri", debURI, "suite", suite)
		return "", err
	}
	if err := validateAptDebURI(debURI); err != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "validate deb_uri", "error", err, "deb_uri", debURI)
		return "", err
	}
	if err := validateAptSuite(suite); err != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "validate suite", "error", err, "suite", suite)
		return "", err
	}
	if err := validateAptComponents(components); err != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "validate components", "error", err, "components", components)
		return "", err
	}

	listBase, err := aptSourcesListBasenameOrDefault(cfg.SourcesListBasename, basename)
	if err != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "validate sources list basename", "error", err, "sources_list_basename", cfg.SourcesListBasename, "keyring_basename", basename)
		return "", err
	}

	var rawOut strings.Builder
	writeLog := func(format string, args ...any) {
		_, _ = fmt.Fprintf(&rawOut, format, args...)
	}

	if err := os.MkdirAll(aptKeyringsDir, 0o755); err != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "create keyrings dir", "error", err, "path", aptKeyringsDir, "details", rawOut.String())
		return rawOut.String(), fmt.Errorf("create keyrings dir: %w", err)
	}

	keyPath := filepath.Join(aptKeyringsDir, fmt.Sprintf("%s.gpg", basename))
	if strings.Contains(cfg.SigningKey, "BEGIN PGP") {
		writeLog("dearmor signing key to %s\n", keyPath)
		cmd := exec.CommandContext(ctx, "gpg", "--batch", "--yes", "--no-tty", "--dearmor", "-o", keyPath)
		cmd.Stdin = strings.NewReader(cfg.SigningKey)
		out, runErr := cmd.CombinedOutput()
		writeLog("%s", string(out))
		if runErr != nil {
			slog.ErrorContext(ctx, "configure apt repo failed", "operation", "gpg --dearmor", "error", runErr, "path", keyPath, "output", string(out), "details", rawOut.String())
			return rawOut.String(), fmt.Errorf("gpg --dearmor: %w", runErr)
		}
	} else {
		writeLog("write binary keyring to %s\n", keyPath)
		if werr := os.WriteFile(keyPath, []byte(cfg.SigningKey), 0o644); werr != nil {
			slog.ErrorContext(ctx, "configure apt repo failed", "operation", "write keyring", "error", werr, "path", keyPath, "details", rawOut.String())
			return rawOut.String(), fmt.Errorf("write keyring: %w", werr)
		}
	}

	if mkErr := os.MkdirAll(aptSourcesListDDir, 0o755); mkErr != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "create sources.list.d", "error", mkErr, "path", aptSourcesListDDir, "details", rawOut.String())
		return rawOut.String(), fmt.Errorf("create sources.list.d: %w", mkErr)
	}
	sourcesPath := filepath.Join(aptSourcesListDDir, listBase)
	debLine := fmt.Sprintf("deb [signed-by=%s] %s %s %s\n", keyPath, debURI, suite, components)
	listBody := "# Written by NVKE agent ConfigureAptRepo\n" + debLine
	writeLog("write sources list %s\n", sourcesPath)
	if werr := os.WriteFile(sourcesPath, []byte(listBody), 0o644); werr != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "write sources list", "error", werr, "path", sourcesPath, "details", rawOut.String())
		return rawOut.String(), fmt.Errorf("write sources list: %w", werr)
	}

	prefPath := filepath.Join(aptPreferencesDir, fmt.Sprintf("nvke-%s.pref", basename))
	legacyPrefPath := filepath.Join(aptPreferencesDir, fmt.Sprintf("nke-%s.pref", basename))
	if cfg.RepoPriority == 0 {
		writeLog("remove preferences file if present: %s\n", prefPath)
		_ = os.Remove(prefPath)
	} else {
		if mkErr := os.MkdirAll(aptPreferencesDir, 0o755); mkErr != nil {
			slog.ErrorContext(ctx, "configure apt repo failed", "operation", "create preferences dir", "error", mkErr, "path", aptPreferencesDir, "details", rawOut.String())
			return rawOut.String(), fmt.Errorf("create preferences dir: %w", mkErr)
		}
		prefBody := fmt.Sprintf(
			"Explanation: NVKE-managed priority for repository %q\n"+
				"Package: *\n"+
				"Pin: release o=%s\n"+
				"Pin-Priority: %d\n",
			repoName,
			repoName,
			cfg.RepoPriority,
		)
		writeLog("write apt preferences %s\n", prefPath)
		if werr := os.WriteFile(prefPath, []byte(prefBody), 0o644); werr != nil {
			slog.ErrorContext(ctx, "configure apt repo failed", "operation", "write apt preferences", "error", werr, "path", prefPath, "details", rawOut.String())
			return rawOut.String(), fmt.Errorf("write apt preferences: %w", werr)
		}
	}

	if rmErr := os.Remove(legacyPrefPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return rawOut.String(), fmt.Errorf("remove legacy apt preferences: %w", rmErr)
	}

	writeLog("apt-get update\n")
	cmd := exec.CommandContext(ctx, "apt-get", "update")
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, runErr := cmd.CombinedOutput()
	writeLog("%s", string(out))
	if runErr != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "operation", "apt-get update", "error", runErr, "output", string(out), "details", rawOut.String())
		return rawOut.String(), fmt.Errorf("apt-get update: %w", runErr)
	}

	return rawOut.String(), nil
}

// RemoveAptRepo deletes /etc/apt/keyrings/<keyringName>.gpg,
// /etc/apt/preferences.d/nvke-<keyringName>.pref, and
// /etc/apt/sources.list.d/<listBasename> if present, then runs apt-get update.
// sourcesListBasename selects the .list file; if empty, listBasename is <keyringName>.list.
// keyringName must pass the same validation as ConfigureAptRepo. Missing files are ignored.
func RemoveAptRepo(ctx context.Context, keyringName, sourcesListBasename string) (string, error) {
	basename, baseErr := aptKeyringBasename(keyringName)
	if baseErr != nil {
		slog.ErrorContext(ctx, "remove apt repo failed", "operation", "validate keyring basename", "error", baseErr, "keyring_name", keyringName)
		return "", baseErr
	}

	listBase, listErr := aptSourcesListBasenameOrDefault(sourcesListBasename, basename)
	if listErr != nil {
		slog.ErrorContext(ctx, "remove apt repo failed", "operation", "validate sources list basename", "error", listErr, "sources_list_basename", sourcesListBasename, "keyring_basename", basename)
		return "", listErr
	}

	var rawOut strings.Builder
	writeLog := func(format string, args ...any) {
		_, _ = fmt.Fprintf(&rawOut, format, args...)
	}

	keyPath := filepath.Join(aptKeyringsDir, fmt.Sprintf("%s.gpg", basename))
	writeLog("remove keyring if present: %s\n", keyPath)
	if rmErr := os.Remove(keyPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		slog.ErrorContext(ctx, "remove apt repo failed", "operation", "remove keyring", "error", rmErr, "keyPath", keyPath, "details", rawOut.String())
		return rawOut.String(), fmt.Errorf("remove keyring: %w", rmErr)
	}

	prefPath := filepath.Join(aptPreferencesDir, fmt.Sprintf("nvke-%s.pref", basename))
	for _, path := range []string{prefPath, filepath.Join(aptPreferencesDir, fmt.Sprintf("nke-%s.pref", basename))} {
		writeLog("remove apt preferences if present: %s\n", path)
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return rawOut.String(), fmt.Errorf("remove apt preferences: %w", rmErr)
		}
	}

	sourcesPath := filepath.Join(aptSourcesListDDir, listBase)
	writeLog("remove sources list if present: %s\n", sourcesPath)
	if rmErr := os.Remove(sourcesPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		slog.ErrorContext(ctx, "remove apt repo failed", "operation", "remove sources list", "error", rmErr, "sourcesPath", sourcesPath, "keyPath", keyPath, "prefPath", prefPath, "details", rawOut.String())
		return rawOut.String(), fmt.Errorf("remove sources list: %w", rmErr)
	}

	writeLog("apt-get update\n")
	cmd := exec.CommandContext(ctx, "apt-get", "update")
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, runErr := cmd.CombinedOutput()
	writeLog("%s", string(out))
	if runErr != nil {
		slog.ErrorContext(ctx, "remove apt repo failed", "operation", "apt-get update", "error", runErr, "output", string(out), "keyPath", keyPath, "prefPath", prefPath, "sourcesPath", sourcesPath, "details", rawOut.String())
		return rawOut.String(), fmt.Errorf("apt-get update: %w", runErr)
	}

	return rawOut.String(), nil
}

func aptKeyringBasename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("keyring_name is required")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return "", errors.New("keyring_name must be a single path segment (no directories)")
	}
	if name == "." || name == ".." {
		return "", errors.New("invalid keyring_name")
	}
	if strings.EqualFold(filepath.Ext(name), ".gpg") {
		return "", errors.New("keyring_name must not include a .gpg suffix; the agent appends .gpg")
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("keyring_name contains invalid character %q", r)
	}
	if len(name) > 128 {
		return "", errors.New("keyring_name too long")
	}
	return name, nil
}

func validateAptDebURI(raw string) error {
	if strings.ContainsAny(raw, "\n\r") {
		return errors.New("deb_uri must be a single line")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("deb_uri: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("deb_uri scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("deb_uri must include a host")
	}
	return nil
}

func validateAptSuite(s string) error {
	if s == "" {
		return errors.New("suite is empty")
	}
	if strings.ContainsAny(s, "\n\r \t") {
		return errors.New("suite must be a single token without whitespace")
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' {
			continue
		}
		return fmt.Errorf("suite contains invalid character %q", r)
	}
	return nil
}

func validateAptComponents(s string) error {
	if strings.ContainsAny(s, "\n\r\t") {
		return errors.New("components must not contain newlines or tabs")
	}
	for _, part := range strings.Fields(s) {
		for _, r := range part {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
				continue
			}
			return fmt.Errorf("components segment %q contains invalid character %q", part, r)
		}
	}
	return nil
}

func aptSourcesListBasenameOrDefault(explicit, keyringBasename string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit == "" {
		return keyringBasename + ".list", nil
	}
	return aptSourcesListBasenameExplicit(explicit)
}

func aptSourcesListBasenameExplicit(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("sources_list_basename is empty")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return "", errors.New("sources_list_basename must be a single path segment")
	}
	if name == ".list" || name == ".." {
		return "", errors.New("invalid sources_list_basename")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".list") {
		return "", errors.New("sources_list_basename must end with .list")
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("sources_list_basename contains invalid character %q", r)
	}
	if len(name) > 128 {
		return "", errors.New("sources_list_basename too long")
	}
	return name, nil
}
