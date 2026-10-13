// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentupgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	agentPackageName         = "nvke-agent"
	legacyAgentPackageName   = "nke-agent"
	legacyAgentVersionPrefix = "NKE Agent Version:"
	agentServiceName         = "nvke-agent.service"
	installCommand           = "install"
	aptAssumeYesFlag         = "-y"
	aptAllowDowngradesFlag   = "--allow-downgrades"
	aptReinstallFlag         = "--reinstall"
)

// CommandRunner is the updater's test seam for apt, dpkg, tar, and systemd.
type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, dir string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("running %s: %w", filepath.Base(name), err)
	}
	return output, nil
}

// Updater executes one durable operation outside the nvke-agent systemd cgroup.
type Updater struct {
	Store              *Store
	Runner             CommandRunner
	AgentBinary        string
	UpdaterBinary      string
	SystemdUnitDir     string
	AgentService       string
	MigrationScript    string
	VerifyAttempts     int
	VerifyPollInterval time.Duration
}

// NewUpdater returns an updater configured for a production node filesystem.
func NewUpdater(store *Store) *Updater {
	return &Updater{
		Store:              store,
		Runner:             execRunner{},
		AgentBinary:        "/usr/local/bin/nvke-agent",
		UpdaterBinary:      "/usr/local/bin/nvke-agent-updater",
		SystemdUnitDir:     "/etc/systemd/system",
		AgentService:       agentServiceName,
		MigrationScript:    "/usr/local/lib/nvke-agent/migrate-agent-paths.sh",
		VerifyAttempts:     30,
		VerifyPollInterval: time.Second,
	}
}

// Run executes or resumes the operation with id.
func (u *Updater) Run(ctx context.Context, id string) error {
	op, err := u.Store.Load(id)
	if err != nil {
		return fmt.Errorf("loading node-agent upgrade operation: %w", err)
	}
	if op.Terminal() {
		return nil
	}
	if _, err := u.Store.SetStatus(id, PhaseRunning, "starting", ""); err != nil {
		return fmt.Errorf("marking node-agent upgrade running: %w", err)
	}

	if op.Action == ActionRollback {
		return u.runRollback(ctx, op)
	}

	mutated, runErr := u.runUpgrade(ctx, op)
	if runErr == nil {
		_, err = u.Store.SetStatus(id, PhaseSucceeded, "verified", "")
		return err
	}

	if !mutated {
		_, statusErr := u.Store.SetStatus(id, PhaseFailedRestored, "failed before replacement", runErr.Error())
		return errors.Join(runErr, statusErr)
	}

	_, _ = u.Store.SetStatus(id, PhaseRunning, "restoring previous version", runErr.Error())
	restoreErr := u.restoreUpgrade(ctx, op)
	if restoreErr == nil {
		_, statusErr := u.Store.SetStatus(id, PhaseFailedRestored, "previous version restored", runErr.Error())
		return errors.Join(runErr, statusErr)
	}
	message := fmt.Sprintf("upgrade failed: %v; restore failed: %v", runErr, restoreErr)
	_, statusErr := u.Store.SetStatus(id, PhaseFailedNotRestored, "restore failed", message)
	return errors.Join(runErr, restoreErr, statusErr)
}

func (u *Updater) runUpgrade(ctx context.Context, op Operation) (bool, error) {
	switch op.Method {
	case MethodAPT:
		return u.runAPTUpgrade(ctx, op)
	case MethodBundle:
		return u.runBundleUpgrade(ctx, op)
	default:
		return false, fmt.Errorf("unsupported node-agent upgrade method %q", op.Method)
	}
}

func (u *Updater) runAPTUpgrade(ctx context.Context, op Operation) (bool, error) {
	if _, err := u.Store.SetStatus(op.ID, PhaseRunning, "refreshing apt metadata", ""); err != nil {
		return false, err
	}
	if _, err := u.Runner.Run(ctx, "apt-get", []string{"update"}, ""); err != nil {
		return false, err
	}

	previousPackageName, previousPackageVersion := op.PreviousPackageName, op.PreviousPackageVersion
	if previousPackageVersion == "" {
		var err error
		previousPackageName, previousPackageVersion, err = u.installedPackage(ctx)
		if err != nil {
			return false, err
		}
		if _, err := u.Store.SetPreviousPackage(op.ID, previousPackageName, previousPackageVersion); err != nil {
			return false, err
		}
	} else if previousPackageName == "" {
		previousPackageName = legacyAgentPackageName
	}

	assetsDir, err := u.assetsDir(op.ID)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Join(assetsDir, "partial"), 0o700); err != nil {
		return false, fmt.Errorf("creating APT staging directory: %w", err)
	}
	targetPackageVersion := strings.TrimPrefix(op.TargetVersion, "v")
	if _, err := u.Store.SetStatus(op.ID, PhaseRunning, "downloading exact apt packages", ""); err != nil {
		return false, err
	}
	targetPackageName, err := u.availablePackage(ctx, targetPackageVersion)
	if err != nil {
		return false, err
	}
	packages := uniqueStrings(targetPackageName+"="+targetPackageVersion, previousPackageName+"="+previousPackageVersion)
	for _, pkg := range packages {
		args := []string{
			aptAssumeYesFlag, "--download-only", aptReinstallFlag, aptAllowDowngradesFlag,
			"-o", "Dir::Cache::archives=" + assetsDir,
			installCommand, pkg,
		}
		if _, err := u.Runner.Run(ctx, "apt-get", args, ""); err != nil {
			return false, fmt.Errorf("staging %s: %w", pkg, err)
		}
	}

	targetDeb, err := u.findDebianPackage(ctx, assetsDir, targetPackageName, targetPackageVersion)
	if err != nil {
		return false, err
	}
	if _, err := u.findDebianPackage(ctx, assetsDir, previousPackageName, previousPackageVersion); err != nil {
		return false, fmt.Errorf("retaining previous package for rollback: %w", err)
	}

	if _, err := u.Store.SetStatus(op.ID, PhaseRunning, "installing exact apt package", ""); err != nil {
		return false, err
	}
	installArgs := []string{aptAssumeYesFlag, aptReinstallFlag, aptAllowDowngradesFlag, installCommand, targetDeb}
	if _, err := u.Runner.Run(ctx, "apt-get", installArgs, ""); err != nil {
		return true, err
	}
	return true, u.verifyAPTVersion(ctx, targetPackageName, op.TargetVersion, targetPackageVersion)
}

func (u *Updater) runBundleUpgrade(ctx context.Context, op Operation) (bool, error) {
	if op.ArtifactURL == "" {
		return false, errors.New("bundle operation is missing artifact URL")
	}
	assetsDir, err := u.assetsDir(op.ID)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(assetsDir, 0o700); err != nil {
		return false, fmt.Errorf("creating bundle staging directory: %w", err)
	}
	if err := u.snapshotInstalledFiles(op.ID); err != nil {
		return false, err
	}

	archive := filepath.Join(assetsDir, "target.tgz")
	extractDir := filepath.Join(assetsDir, "target")
	if _, err := u.Store.SetStatus(op.ID, PhaseRunning, "downloading exact bundle", ""); err != nil {
		return false, err
	}
	if _, err := u.Runner.Run(ctx, "curl", []string{"--fail", "--location", "--output", archive, op.ArtifactURL}, ""); err != nil {
		return false, err
	}
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return false, fmt.Errorf("creating bundle extract directory: %w", err)
	}
	if _, err := u.Runner.Run(ctx, "tar", []string{"-xzf", archive, "-C", extractDir}, ""); err != nil {
		return false, err
	}
	if _, err := u.Store.SetStatus(op.ID, PhaseRunning, "installing exact bundle", ""); err != nil {
		return false, err
	}
	if err := u.installBundleFiles(ctx, extractDir); err != nil {
		return true, err
	}
	return true, u.verifyVersion(ctx, op.TargetVersion)
}

func (u *Updater) runRollback(ctx context.Context, op Operation) error {
	original, err := u.Store.Load(op.RestoreFromOperationID)
	if err != nil {
		return u.failRollback(op.ID, fmt.Errorf("loading upgrade operation to restore: %w", err))
	}
	if original.Phase != PhaseSucceeded {
		return u.failRollback(op.ID, fmt.Errorf("upgrade operation %s is %s, not Succeeded", original.ID, original.Phase))
	}
	if _, err := u.Store.SetStatus(op.ID, PhaseRunning, "restoring retained artifact", ""); err != nil {
		return err
	}
	if err := u.restoreUpgrade(ctx, original); err != nil {
		return u.failRollback(op.ID, err)
	}
	if _, err := u.Store.SetStatus(op.ID, PhaseSucceeded, "previous version restored", ""); err != nil {
		return err
	}
	return nil
}

func (u *Updater) restoreUpgrade(ctx context.Context, op Operation) error {
	switch op.Method {
	case MethodAPT:
		return u.restoreAPT(ctx, op)
	case MethodBundle:
		if err := u.restoreSnapshot(ctx, op.ID); err != nil {
			return err
		}
		return u.verifyVersion(ctx, op.PreviousVersion)
	default:
		return fmt.Errorf("unsupported restore method %q", op.Method)
	}
}

func (u *Updater) restoreAPT(ctx context.Context, op Operation) error {
	stored, err := u.Store.Load(op.ID)
	if err != nil {
		return err
	}
	if stored.PreviousPackageVersion == "" {
		return errors.New("previous Debian package version was not retained")
	}
	assetsDir, err := u.assetsDir(op.ID)
	if err != nil {
		return err
	}
	previousPackageName := stored.PreviousPackageName
	if previousPackageName == "" {
		// Operations written by the old updater did not record the package name.
		previousPackageName = legacyAgentPackageName
	}
	previousDeb, err := u.findDebianPackage(ctx, assetsDir, previousPackageName, stored.PreviousPackageVersion)
	if err != nil {
		return err
	}
	args := []string{aptAssumeYesFlag, aptReinstallFlag, aptAllowDowngradesFlag, installCommand, previousDeb}
	if _, err := u.Runner.Run(ctx, "apt-get", args, ""); err != nil {
		return err
	}
	return u.verifyAPTVersion(ctx, previousPackageName, op.PreviousVersion, stored.PreviousPackageVersion)
}

func (u *Updater) installedPackage(ctx context.Context) (string, string, error) {
	for _, name := range []string{agentPackageName, legacyAgentPackageName} {
		output, err := u.Runner.Run(ctx, "dpkg-query", []string{"--show", "--showformat=${db:Status-Status} ${Version}", name}, "")
		fields := strings.Fields(string(output))
		if err == nil && len(fields) == 2 && fields[0] == "installed" {
			return name, fields[1], nil
		}
	}
	return "", "", errors.New("APT self-upgrade requires nvke-agent or nke-agent to be installed as a Debian package")
}

func (u *Updater) availablePackage(ctx context.Context, version string) (string, error) {
	for _, name := range []string{agentPackageName, legacyAgentPackageName} {
		output, err := u.Runner.Run(ctx, "apt-cache", []string{"show", name + "=" + version}, "")
		if err == nil && strings.Contains(string(output), "Package: "+name+"\n") {
			return name, nil
		}
	}
	return "", fmt.Errorf("neither nvke-agent nor nke-agent is available at version %s", version)
}

func (u *Updater) usePackageName(name string) {
	u.AgentBinary = filepath.Join(filepath.Dir(u.AgentBinary), name)
	u.UpdaterBinary = filepath.Join(filepath.Dir(u.UpdaterBinary), name+"-updater")
	u.AgentService = name + ".service"
}

func (u *Updater) verifyAPTVersion(ctx context.Context, name, agentVersion, packageVersion string) error {
	installedName, actual, err := u.installedPackage(ctx)
	if err != nil {
		return err
	}
	if installedName != name || actual != packageVersion {
		return fmt.Errorf("installed %s=%s, expected %s=%s", installedName, actual, name, packageVersion)
	}
	u.usePackageName(name)
	return u.verifyVersion(ctx, agentVersion)
}

func (u *Updater) findDebianPackage(ctx context.Context, dir, name, version string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.deb"))
	if err != nil {
		return "", fmt.Errorf("listing staged Debian packages: %w", err)
	}
	sort.Strings(matches)
	for _, candidate := range matches {
		pkgOutput, pkgErr := u.Runner.Run(ctx, "dpkg-deb", []string{"--field", candidate, "Package"}, "")
		if pkgErr != nil || strings.TrimSpace(string(pkgOutput)) != name {
			continue
		}
		versionOutput, versionErr := u.Runner.Run(ctx, "dpkg-deb", []string{"--field", candidate, "Version"}, "")
		if versionErr == nil && strings.TrimSpace(string(versionOutput)) == version {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("staged %s=%s package not found", name, version)
}

func (u *Updater) snapshotInstalledFiles(operationID string) error {
	_, binaryErr := os.Stat(u.AgentBinary)
	_, unitErr := os.Stat(filepath.Join(u.SystemdUnitDir, u.AgentService))
	if errors.Is(binaryErr, os.ErrNotExist) || errors.Is(unitErr, os.ErrNotExist) {
		u.usePackageName(legacyAgentPackageName)
	}
	assetsDir, err := u.assetsDir(operationID)
	if err != nil {
		return err
	}
	backupDir := filepath.Join(assetsDir, "previous")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return fmt.Errorf("creating bundle rollback directory: %w", err)
	}
	marker := filepath.Join(backupDir, ".snapshot-complete")
	if _, err := os.Stat(marker); err == nil {
		// A resumed rename must retain the original layout, not mix the old
		// and new names or replace the previous binaries with the target.
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Retained snapshots written by the old updater have no completion marker.
	legacySnapshotComplete := true
	legacyFiles := []string{
		legacyAgentPackageName,
		legacyAgentPackageName + "-updater",
		legacyAgentPackageName + ".service",
		legacyAgentPackageName + "-updater@.service",
	}
	for _, name := range legacyFiles {
		if _, err := os.Stat(filepath.Join(backupDir, name)); err != nil {
			legacySnapshotComplete = false
			break
		}
	}
	if legacySnapshotComplete {
		return os.WriteFile(marker, nil, 0o600)
	}
	files := []string{
		u.AgentBinary,
		u.UpdaterBinary,
		filepath.Join(u.SystemdUnitDir, u.AgentService),
		filepath.Join(u.SystemdUnitDir, filepath.Base(u.UpdaterBinary)+"@.service"),
	}
	if filepath.Base(u.AgentBinary) == agentPackageName {
		files = append(files, u.MigrationScript)
	}
	for _, source := range files {
		if err := copyRegularFile(source, filepath.Join(backupDir, filepath.Base(source))); err != nil {
			return fmt.Errorf("retaining %s: %w", source, err)
		}
	}
	return os.WriteFile(marker, nil, 0o600)
}

func (u *Updater) installBundleFiles(ctx context.Context, extractDir string) error {
	agent, err := findFile(extractDir, agentPackageName, legacyAgentPackageName)
	if err != nil {
		return err
	}
	packageName := filepath.Base(agent)
	updater, err := findFile(extractDir, packageName+"-updater")
	if err != nil {
		return err
	}
	agentUnit, err := findFile(extractDir, packageName+".service")
	if err != nil {
		return err
	}
	updaterUnit, err := findFile(extractDir, packageName+"-updater@.service")
	if err != nil {
		return err
	}
	migrationScript := ""
	if packageName == agentPackageName {
		migrationScript, err = findFile(extractDir, "migrate-agent-paths.sh")
		if err != nil {
			return err
		}
	}
	// Reload first so restoration also works after a partially installed rename.
	if _, err := u.Runner.Run(ctx, "systemctl", []string{"daemon-reload"}, ""); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(u.SystemdUnitDir, u.AgentService)); !errors.Is(err, os.ErrNotExist) {
		if _, err := u.Runner.Run(ctx, "systemctl", []string{"stop", u.AgentService}, ""); err != nil {
			return err
		}
		if _, err := u.Runner.Run(ctx, "systemctl", []string{"disable", u.AgentService}, ""); err != nil {
			return err
		}
	}
	if u.AgentService != packageName+".service" {
		// A regular legacy unit would prevent systemd from creating the new
		// unit's Alias. The snapshot already retained these files for rollback.
		for _, name := range []string{u.AgentService, filepath.Base(u.UpdaterBinary) + "@.service"} {
			if err := os.Remove(filepath.Join(u.SystemdUnitDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("removing replaced unit %s: %w", name, err)
			}
		}
	}
	u.usePackageName(packageName)
	// Old retained bundles predate the migration; aliases survive their restore.
	if migrationScript != "" {
		if _, err := u.Runner.Run(ctx, "bash", []string{migrationScript}, ""); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(u.MigrationScript), 0o755); err != nil {
			return err
		}
		if _, err := u.Runner.Run(ctx, installCommand, []string{"-m", "0755", migrationScript, u.MigrationScript}, ""); err != nil {
			return err
		}
	}
	for _, file := range []struct {
		source string
		target string
		mode   string
	}{
		{source: updater, target: u.UpdaterBinary, mode: "0755"},
		{source: agent, target: u.AgentBinary, mode: "0755"},
		{source: updaterUnit, target: filepath.Join(u.SystemdUnitDir, filepath.Base(u.UpdaterBinary)+"@.service"), mode: "0644"},
		{source: agentUnit, target: filepath.Join(u.SystemdUnitDir, u.AgentService), mode: "0644"},
	} {
		if _, err := u.Runner.Run(ctx, installCommand, []string{"-m", file.mode, file.source, file.target}, ""); err != nil {
			return err
		}
	}
	if packageName == agentPackageName {
		legacyBinary := filepath.Join(filepath.Dir(u.AgentBinary), legacyAgentPackageName)
		if err := os.Remove(legacyBinary); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Symlink(u.AgentBinary, legacyBinary); err != nil {
			return err
		}
	}
	if _, err := u.Runner.Run(ctx, "systemctl", []string{"daemon-reload"}, ""); err != nil {
		return err
	}
	if _, err := u.Runner.Run(ctx, "systemctl", []string{"enable", "--force", u.AgentService}, ""); err != nil {
		return err
	}
	_, err = u.Runner.Run(ctx, "systemctl", []string{"restart", u.AgentService}, "")
	return err
}

func (u *Updater) restoreSnapshot(ctx context.Context, operationID string) error {
	assetsDir, err := u.assetsDir(operationID)
	if err != nil {
		return err
	}
	backupDir := filepath.Join(assetsDir, "previous")
	return u.installBundleFiles(ctx, backupDir)
}

func (u *Updater) verifyVersion(ctx context.Context, expected string) error {
	attempts := u.VerifyAttempts
	if attempts <= 0 {
		attempts = 1
	}
	interval := u.VerifyPollInterval
	for attempt := range attempts {
		output, err := u.Runner.Run(ctx, u.AgentBinary, []string{"--version"}, "")
		if err == nil && parseAgentVersion(string(output)) == expected {
			if _, activeErr := u.Runner.Run(ctx, "systemctl", []string{"is-active", "--quiet", u.AgentService}, ""); activeErr == nil {
				return nil
			}
		}
		if attempt+1 < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
	return fmt.Errorf("nvke-agent did not become active at version %s", expected)
}

func (u *Updater) assetsDir(operationID string) (string, error) {
	dir, err := u.Store.OperationDir(operationID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "assets"), nil
}

func (u *Updater) failRollback(operationID string, operationErr error) error {
	_, statusErr := u.Store.SetStatus(operationID, PhaseFailedNotRestored, "rollback failed", operationErr.Error())
	return errors.Join(operationErr, statusErr)
}

func parseAgentVersion(output string) string {
	output = strings.TrimSpace(output)
	for _, prefix := range []string{"NVKE Agent Version:", legacyAgentVersionPrefix} {
		if version, ok := strings.CutPrefix(output, prefix); ok {
			return strings.TrimSpace(version)
		}
	}
	return ""
}

func uniqueStrings(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func findFile(root string, names ...string) (string, error) {
	var matches []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && slices.Contains(names, entry.Name()) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("searching bundle for %v: %w", names, err)
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("bundle must contain exactly one of %v, found %d", names, len(matches))
	}
	return matches[0], nil
}

func copyRegularFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}
