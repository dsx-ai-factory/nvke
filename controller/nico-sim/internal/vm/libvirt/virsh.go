// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/vm"
)

// New returns a vm.Runtime backed by libvirt/virsh on Linux.
func New(uri string) vm.Runtime {
	if strings.TrimSpace(uri) == "" {
		uri = "qemu:///session"
	}
	return &virsh{uri: uri}
}

type virsh struct {
	uri string
}

const (
	startAttempts    = 6
	startRetryDelay  = 10 * time.Second
	defineAttempts   = 6
	defineRetryDelay = 30 * time.Second
	stopTimeout      = 60 * time.Second
)

func (r *virsh) EnsureBridge(ctx context.Context, bridgeName, bridgeCIDR string) error {
	// Check if bridge already exists and is configured. With qemu:///session the
	// bridge is pre-configured by an admin (via setup-linux.sh). We just
	// verify it's ready; no privileged operations needed.
	linkOut, linkErr := r.runIP(ctx, "link", "show", bridgeName)
	if linkErr == nil {
		bridgeAddr := gatewayCIDR(bridgeCIDR)
		addrOut, _ := r.runIP(ctx, "-4", "addr", "show", "dev", bridgeName)
		if strings.Contains(addrOut, bridgeAddr) && strings.Contains(strings.ToUpper(linkOut), ",UP") {
			return nil // bridge exists, has correct IP, and is UP
		}
	}

	// Bridge doesn't exist or isn't fully configured — try to set it up.
	// This requires root and is the fallback for qemu:///system.
	if linkErr != nil {
		if _, createErr := r.runIP(ctx, "link", "add", "name", bridgeName, "type", "bridge"); createErr != nil {
			return fmt.Errorf("failed to create bridge %s: %w (run setup-linux.sh first)", bridgeName, createErr)
		}
	}

	bridgeAddr := gatewayCIDR(bridgeCIDR)
	out, err := r.runIP(ctx, "-4", "addr", "show", "dev", bridgeName)
	if err != nil {
		return fmt.Errorf("failed to inspect bridge %s addresses: %w", bridgeName, err)
	}
	if !strings.Contains(out, bridgeAddr) {
		if _, err := r.runIP(ctx, "addr", "add", bridgeAddr, "dev", bridgeName); err != nil &&
			!strings.Contains(err.Error(), "File exists") {
			return fmt.Errorf("failed to assign bridge address %s: %w", bridgeAddr, err)
		}
	}

	if _, err := r.runIP(ctx, "link", "set", bridgeName, "up"); err != nil {
		return fmt.Errorf("failed to bring bridge %s up: %w", bridgeName, err)
	}
	return nil
}

func (r *virsh) CreateAndStart(ctx context.Context, spec vm.InstanceSpec) (string, error) {
	workDir := filepath.Dir(spec.DiskPath)

	// Pre-initialise swtpm state so we can capture the EK cert as a
	// readable file. The UUID is pinned in the domain XML so libvirt
	// finds and reuses the state we created.
	domainUUID, tpmEnabled, err := r.setupTPM(ctx, workDir)
	if err != nil {
		return "", fmt.Errorf("swtpm setup failed: %w", err)
	}

	xmlPath := filepath.Join(workDir, "domain.xml")
	xmlContent := renderDomainXML(spec, spec.Arch, domainUUID, tpmEnabled)
	if err := os.WriteFile(xmlPath, []byte(xmlContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to write domain xml: %w", err)
	}

	// On the arm64 CI runners a define can fail for a while with "Unable to find 'efi' firmware"; a later one works.
	if err := retry(ctx, defineAttempts, defineRetryDelay, func() error {
		_, defineErr := r.runVirsh(ctx, "define", xmlPath)
		return defineErr
	}); err != nil {
		return "", fmt.Errorf("virsh define failed: %w", err)
	}
	if _, err := r.runVirsh(ctx, "autostart", spec.DomainName); err != nil {
		return "", fmt.Errorf("virsh autostart failed: %w", err)
	}
	// Domains that carry a TPM race each other inside libvirt: each start
	// creates the shared swtpm run directory, and the loser gets
	// "failed to create directory ...: File exists" and never boots. The
	// engine marks such an instance Error and stops reconciling it, so one
	// lost race costs the whole VM. A later attempt finds the directory there.
	// A neighbour pod's qemu install also leaves /dev/kvm unusable for a few seconds.
	if err := retry(ctx, startAttempts, startRetryDelay, func() error {
		_, startErr := r.runVirsh(ctx, "start", spec.DomainName)
		return startErr
	}); err != nil {
		return "", fmt.Errorf("virsh start failed: %w", err)
	}
	return spec.DomainName, nil
}

// retry calls fn until it succeeds, up to attempts times, waiting delay
// between tries. It reports the last error.
func retry(ctx context.Context, attempts int, delay time.Duration, fn func() error) error {
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("gave up after %d attempts: %w", attempts, err)
}

func (r *virsh) Reboot(ctx context.Context, machineID string) error {
	if _, err := r.runVirsh(ctx, "reboot", machineID); err != nil {
		// Fallback path for guests that ignore ACPI reboot.
		if _, destroyErr := r.runVirsh(ctx, "destroy", machineID); destroyErr != nil {
			return fmt.Errorf("virsh reboot failed (%w) and destroy fallback failed (%w)", err, destroyErr)
		}
		if _, startErr := r.runVirsh(ctx, "start", machineID); startErr != nil {
			return fmt.Errorf("failed to start VM after destroy fallback: %w", startErr)
		}
	}
	return nil
}

// Stop asks the guest to shut down via ACPI and forces it off if it has not
// stopped after a minute. The domain definition, disk and TPM state stay.
func (r *virsh) Stop(ctx context.Context, machineID string) error {
	if _, err := r.runVirsh(ctx, "shutdown", machineID); err != nil {
		return err
	}
	for deadline := time.Now().Add(stopTimeout); time.Now().Before(deadline); {
		if state, _ := r.GetState(ctx, machineID); state == vm.MachineStateShutOff {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	_, err := r.runVirsh(ctx, "destroy", machineID)
	return err
}

func (r *virsh) Start(ctx context.Context, machineID string) error {
	_, err := r.runVirsh(ctx, "start", machineID)
	return err
}

func (r *virsh) Delete(ctx context.Context, machineID string) error {
	_, _ = r.runVirsh(ctx, "destroy", machineID)
	if _, err := r.runVirsh(ctx, "undefine", machineID, "--nvram", "--tpm"); err != nil {
		// Fallback for older libvirt without --tpm support.
		_, _ = r.runVirsh(ctx, "undefine", machineID, "--nvram")
	}
	return nil
}

func (r *virsh) GetState(ctx context.Context, machineID string) (vm.MachineState, error) {
	out, err := r.runVirsh(ctx, "domstate", machineID)
	if err != nil {
		return vm.MachineStateNotFound, nil
	}
	raw := strings.ToLower(strings.TrimSpace(out))
	switch {
	case strings.Contains(raw, "running"), strings.Contains(raw, "idle"):
		return vm.MachineStateRunning, nil
	case strings.Contains(raw, "paused"), strings.Contains(raw, "pmsuspended"):
		return vm.MachineStatePaused, nil
	case strings.Contains(raw, "shut off"), strings.Contains(raw, "in shutdown"):
		return vm.MachineStateShutOff, nil
	case strings.Contains(raw, "crashed"):
		return vm.MachineStateCrashed, nil
	default:
		return vm.MachineStateNotFound, nil
	}
}

func (r *virsh) IsRunning(ctx context.Context, machineID string) (bool, error) {
	s, err := r.GetState(ctx, machineID)
	return s == vm.MachineStateRunning, err
}

func (r *virsh) SupportsTCPProbe() bool {
	return true
}

func (r *virsh) ListMachines(ctx context.Context) ([]string, error) {
	out, err := r.runVirsh(ctx, "list", "--all", "--name")
	if err != nil {
		return nil, fmt.Errorf("virsh list failed: %w", err)
	}
	var machines []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name != "" && strings.HasPrefix(name, "nico-vm-") {
			machines = append(machines, name)
		}
	}
	return machines, nil
}

// LookupDHCPLease queries the libvirt network's DHCP leases and returns the
// IPv4 address currently assigned to the given MAC. Used in libvirt-network
// mode, where libvirt's own dnsmasq (not nico-vms) hands out addresses.
// virsh renders the lease table as:
//
//	Expiry Time   MAC address   Protocol   IP address           Hostname   Client ID
//	...           00:52:...     ipv4       192.168.122.45/24    host       ...
func (r *virsh) LookupDHCPLease(ctx context.Context, network, mac string) (string, bool, error) {
	out, err := r.runVirsh(ctx, "net-dhcp-leases", network)
	if err != nil {
		return "", false, fmt.Errorf("virsh net-dhcp-leases %s failed: %w", network, err)
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		hasMAC := false
		for _, f := range fields {
			if strings.EqualFold(f, mac) {
				hasMAC = true
				break
			}
		}
		if !hasMAC {
			continue
		}
		// The IP column is rendered as "192.168.122.45/24" — find it by
		// parsing rather than relying on a fixed column index.
		for _, f := range fields {
			if ip, _, perr := net.ParseCIDR(f); perr == nil && ip.To4() != nil {
				return ip.String(), true, nil
			}
		}
	}
	return "", false, nil
}

func (r *virsh) runVirsh(ctx context.Context, args ...string) (string, error) {
	finalArgs := make([]string, 0, len(args)+2)
	if r.uri != "" {
		finalArgs = append(finalArgs, "-c", r.uri)
	}
	finalArgs = append(finalArgs, args...)

	cmd := exec.CommandContext(ctx, "virsh", finalArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("virsh %v failed: %w: %s", finalArgs, err, string(output))
	}
	return string(output), nil
}

func (r *virsh) runIP(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "ip", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("ip %v failed: %w: %s", args, err, string(output))
	}
	return string(output), nil
}

func gatewayCIDR(cidr string) string {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return cidr
	}
	v4 := ip.To4()
	if v4 == nil {
		return cidr
	}
	mask := ipNet.Mask
	if len(mask) != 4 {
		return cidr
	}
	network := binary.BigEndian.Uint32(v4) & binary.BigEndian.Uint32(mask)
	gateway := network + 1
	prefixLen, _ := ipNet.Mask.Size()
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, gateway)
	return fmt.Sprintf("%s/%d", net.IP(b).String(), prefixLen)
}

func detectDomainType() string {
	if _, err := os.Stat("/dev/kvm"); err == nil {
		return "kvm"
	}
	return "qemu"
}

func renderDomainXML(spec vm.InstanceSpec, arch, domainUUID string, tpmEnabled bool) string {
	return GenerateDomainXML(DomainConfig{
		Name:        spec.DomainName,
		UUID:        domainUUID,
		VCPUs:       spec.VCPUs,
		MemoryMiB:   spec.MemoryMiB,
		OverlayPath: spec.DiskPath,
		SeedISOPath: spec.SeedISOPath,
		BridgeName:  spec.BridgeName,
		NetworkName: spec.NetworkName,
		NICMAC:      spec.NICMAC,
		Arch:        arch,
		UEFI:        spec.UEFI,
		TPM:         tpmEnabled,
		DomainType:  detectDomainType(),
	})
}

// setupTPM pre-initialises a swtpm state directory and captures the EK
// certificate as a readable file. Returns the domain UUID to pin in the
// XML so libvirt finds our pre-created state, and whether TPM was enabled.
// If swtpm_setup is not installed, returns ("", false, nil) — TPM is skipped.
func (r *virsh) setupTPM(ctx context.Context, workDir string) (domainUUID string, tpmEnabled bool, err error) {
	if _, lookErr := exec.LookPath("swtpm_setup"); lookErr != nil {
		return "", false, nil
	}

	domainUUID = uuid.New().String()
	stateDir := r.swtpmStatePath(domainUUID)
	certDir := filepath.Join(workDir, "tpm-certs")

	if mkErr := os.MkdirAll(stateDir, 0o755); mkErr != nil {
		return "", false, fmt.Errorf("failed to create swtpm state dir %s: %w", stateDir, mkErr)
	}
	if mkErr := os.MkdirAll(certDir, 0o755); mkErr != nil {
		_ = os.RemoveAll(filepath.Dir(stateDir))
		return "", false, fmt.Errorf("failed to create cert output dir: %w", mkErr)
	}

	// swtpm_localca stores CA state (signing key, serial) in a directory
	// defined by /etc/swtpm-localca.conf (default: /var/lib/swtpm-localca/).
	// For non-root users (qemu:///session), that path is not writable.
	// Generate a custom config pointing to a user-writable directory.
	setupConf, confErr := r.ensureSwtpmConfig(workDir)
	if confErr != nil {
		_ = os.RemoveAll(filepath.Dir(stateDir))
		_ = os.RemoveAll(certDir)
		return "", false, fmt.Errorf("failed to create swtpm config: %w", confErr)
	}

	cmd := exec.CommandContext(ctx, "swtpm_setup",
		"--tpm2",
		"--tpmstate", stateDir,
		"--config", setupConf,
		"--create-ek-cert",
		"--create-platform-cert",
		"--write-ek-cert-files", certDir,
	)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		_ = os.RemoveAll(filepath.Dir(stateDir))
		_ = os.RemoveAll(certDir)
		return "", false, fmt.Errorf("swtpm_setup failed: %w: %s", runErr, string(output))
	}

	// Read the EK certificate (DER format) written by swtpm_setup.
	var certData []byte
	for _, name := range []string{"ek-rsa2048.crt", "ek-rsa3072.crt", "ek-ecc384.crt"} {
		if data, readErr := os.ReadFile(filepath.Join(certDir, name)); readErr == nil {
			certData = data
			break
		}
	}
	if certData != nil {
		encoded := base64.StdEncoding.EncodeToString(certData)
		_ = os.WriteFile(filepath.Join(workDir, "tpm-ek.crt"), []byte(encoded), 0o644)
	}

	_ = os.RemoveAll(certDir)

	// stateDir is a guess: libvirt's swtpm location depends on which driver
	// serves the domain, which the URI does not always say. Mirror the finished
	// state to the other candidate so both hold this same EK. Running
	// swtpm_setup twice would not do — that yields two different EKs, and the
	// cert would name the wrong one half the time.
	if mirrorErr := r.mirrorSwtpmState(stateDir, domainUUID); mirrorErr != nil {
		// Not fatal: the seeded path may still be the one libvirt uses, and
		// TPMEKCert refuses the cert if it is not.
		fmt.Fprintf(os.Stderr, "nico-vms: could not mirror swtpm state for %s: %v\n", domainUUID, mirrorErr)
	}
	return domainUUID, true, nil
}

// mirrorSwtpmState copies finished swtpm state to the candidate directory
// libvirt would use if the URI guess in swtpmStatePath is wrong.
func (r *virsh) mirrorSwtpmState(stateDir, domainUUID string) error {
	dst := systemSwtpmStatePath(domainUUID)
	if dst == stateDir {
		dst = sessionSwtpmStatePath(domainUUID)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(stateDir, entry.Name()))
		if readErr != nil {
			return readErr
		}
		if writeErr := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o644); writeErr != nil {
			return writeErr
		}
	}
	// libvirt's system driver runs swtpm as the swtpm user, which must be able
	// to write this state back. Only root can hand it over.
	if os.Geteuid() == 0 {
		_ = exec.Command("chown", "-R", "swtpm:swtpm", dst).Run()
	}
	return nil
}

// ensureSwtpmConfig creates user-writable swtpm configuration files so that
// swtpm_localca can store its CA state without needing root access.
// Returns the path to the swtpm_setup config file.
func (r *virsh) ensureSwtpmConfig(workDir string) (string, error) {
	localcaDir := r.swtpmLocalCADir()
	if err := os.MkdirAll(localcaDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir localca dir: %w", err)
	}

	localcaConf := filepath.Join(workDir, "swtpm-localca.conf")
	localcaContent := fmt.Sprintf("statedir = %s\nsigningkey = %s\nissuercert = %s\ncertserial = %s\n",
		localcaDir,
		filepath.Join(localcaDir, "signkey.pem"),
		filepath.Join(localcaDir, "issuercert.pem"),
		filepath.Join(localcaDir, "certserial"),
	)
	if err := os.WriteFile(localcaConf, []byte(localcaContent), 0o644); err != nil {
		return "", err
	}

	setupConf := filepath.Join(workDir, "swtpm_setup.conf")
	setupContent := fmt.Sprintf("create_certs_tool=/usr/bin/swtpm_localca\ncreate_certs_tool_config=%s\ncreate_certs_tool_options=/etc/swtpm-localca.options\n",
		localcaConf,
	)
	if err := os.WriteFile(setupConf, []byte(setupContent), 0o644); err != nil {
		return "", err
	}
	return setupConf, nil
}

// swtpmLocalCADir returns a user-writable directory for swtpm_localca state.
func (r *virsh) swtpmLocalCADir() string {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "swtpm-localca")
}

// swtpmStatePath returns the directory where libvirt expects to find swtpm
// state for the given domain UUID, derived from the connection URI.
func (r *virsh) swtpmStatePath(domainUUID string) string {
	if !strings.Contains(r.uri, "system") {
		return sessionSwtpmStatePath(domainUUID) // session mode (default)
	}
	return systemSwtpmStatePath(domainUUID)
}

func sessionSwtpmStatePath(domainUUID string) string {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "libvirt", "qemu", "swtpm", domainUUID, "tpm2")
}

// systemSwtpmRoot is where libvirt's system driver keeps swtpm state.
// A variable so tests can point it somewhere writable.
var systemSwtpmRoot = "/var/lib/libvirt/swtpm"

func systemSwtpmStatePath(domainUUID string) string {
	return filepath.Join(systemSwtpmRoot, domainUUID, "tpm2")
}

// TPMEKCert reads the base64-encoded EK certificate from the working
// directory associated with the given machine. The cert file is written
// during CreateAndStart by setupTPM.
func (r *virsh) TPMEKCert(ctx context.Context, machineID string) (string, error) {
	dumpxml, err := r.runVirsh(ctx, "dumpxml", machineID)
	if err != nil {
		return "", fmt.Errorf("failed to dump domain xml for %s: %w", machineID, err)
	}

	if !strings.Contains(dumpxml, "<tpm") {
		return "", nil
	}

	diskPath := extractDiskPathFromXML(dumpxml)
	if diskPath == "" {
		return "", fmt.Errorf("could not determine disk path from domain xml for %s", machineID)
	}

	certPath := filepath.Join(filepath.Dir(diskPath), "tpm-ek.crt")
	certData, readErr := os.ReadFile(certPath)
	if readErr != nil {
		return "", fmt.Errorf("TPM device present but failed to read EK cert at %s: %w", certPath, readErr)
	}

	// setupTPM seeds swtpm state and reads the EK from it, guessing libvirt's
	// location for that state from the URI. A wrong guess means libvirt made its
	// own state with a different EK, so this cert names a key the guest lacks.
	// Refuse it: it feeds the TPM attestor allowlist, where the miss surfaces
	// later as an x509 error naming nothing that led to it.
	if used, uuid, mismatch := r.swtpmStateMismatch(dumpxml); mismatch {
		return "", fmt.Errorf(
			"stale EK cert for %s: libvirt used swtpm state in %s, cert came from %s (libvirt URI %q)",
			machineID, used, r.swtpmStatePath(uuid), r.uri,
		)
	}
	return strings.TrimSpace(string(certData)), nil
}

// swtpmStateMismatch reports whether libvirt runs this domain against swtpm
// state other than the directory setupTPM seeded. An absent directory is not
// evidence either way, so an unreadable layout returns false rather than
// failing a working VM.
func (r *virsh) swtpmStateMismatch(dumpxml string) (usedPath, domainUUID string, mismatch bool) {
	domainUUID = extractUUIDFromXML(dumpxml)
	if domainUUID == "" {
		return "", "", false
	}

	seeded := r.swtpmStatePath(domainUUID)
	// The other candidate: what libvirt uses when the URI does not describe the
	// driver actually serving the domain.
	other := systemSwtpmStatePath(domainUUID)
	if seeded == other {
		other = sessionSwtpmStatePath(domainUUID)
	}

	if dirHasEntries(other) && !dirHasEntries(seeded) {
		return other, domainUUID, true
	}
	return "", domainUUID, false
}

// dirHasEntries reports whether dir exists and holds at least one entry.
func dirHasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// extractUUIDFromXML pulls <uuid>...</uuid> out of virsh dumpxml output.
func extractUUIDFromXML(dumpxml string) string {
	const openTag, closeTag = "<uuid>", "</uuid>"
	start := strings.Index(dumpxml, openTag)
	if start < 0 {
		return ""
	}
	rest := dumpxml[start+len(openTag):]
	end := strings.Index(rest, closeTag)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// extractDiskPathFromXML pulls the first <source file='...'> path from
// virsh dumpxml output. This is the overlay disk path whose parent
// directory is the instance working directory.
func extractDiskPathFromXML(dumpxml string) string {
	const prefix = "<source file='"
	idx := strings.Index(dumpxml, prefix)
	if idx < 0 {
		return ""
	}
	rest := dumpxml[idx+len(prefix):]
	end := strings.Index(rest, "'")
	if end < 0 {
		return ""
	}
	return rest[:end]
}
