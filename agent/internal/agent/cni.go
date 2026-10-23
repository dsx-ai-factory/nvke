// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"
	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
	yaml "gopkg.in/yaml.v2"
)

var CILIUM_VERSION semver.Version = *semver.MustParse("1.15.5")

// errCNIApplyRequiresControlPlane is returned when charts deploy is requested on a node
// whose Role is not controlplane (see ControlPlaneRole in agent.go).
var errCNIApplyRequiresControlPlane = errors.New("InstallCNI may only be invoked on control-plane nodes")

var errCNIBadRequestImagesRole = errors.New("InstallCNI no longer installs CNI image packages; use InstallPackages (or kubelet/registry pull)")

// errCNIChartsDebNotInstalled is returned when the charts/manifest deb is missing — install via
// InstallPackages (or apt) before invoking InstallCNI. The agent does not validate container images
// in containerd/CRI so clusters can rely entirely on kubelet/registry pull from a tenant registry.
var errCNIChartsDebNotInstalled = errors.New("CNI charts Debian package must be installed before InstallCNI")

// chartsDebPresenceCheck verifies the charts package is registered in dpkg (swappable in tests).
var chartsDebPresenceCheck = chartsDebPresenceDefault

func chartsDebPresenceDefault(ctx context.Context, binPkg string) error {
	if _, err := dpkgInstalledVersion(ctx, binPkg); err != nil {
		slog.WarnContext(ctx, "CNI charts package not installed",
			"package", binPkg,
			"detail", err,
		)
		return fmt.Errorf("%w (%q)", errCNIChartsDebNotInstalled, binPkg)
	}
	return nil
}

// errOvnRequiresHostOpenVSwitch is returned when ovn-kubernetes cannot use host Open vSwitch (ovs-vsctl).
var errOvnRequiresHostOpenVSwitch = errors.New(
	"openvswitch is required on the host for ovn-kubernetes (install a package such as openvswitch-switch and ensure ovs-vsctl can reach ovs-vswitchd)",
)

const (
	ovnKubernetesHelmChartDir = "/etc/dgx/ovn-kubernetes"
	// defaultOvnCniChartStream is the OVN Kubernetes chart stream baked into Debian package names
	// when InstallCNIRequest.version is empty. Sync with OVN_STREAMS in os-image/packages/cni-ovn/Makefile.
	defaultOvnCniChartStream = "2.4.2"
	// defaultCalicoCniStream is the Calico patch stream in Debian package names when version is empty.
	// Sync with CALICO_STREAM in cni-packages debs/calico/BUILD.
	defaultCalicoCniStream = "3.30.1"

	// CNI plugin names, as they arrive on InstallCNIPluginRequest.
	// `tigera` and `calico` name the same plugin; callers send either.
	cniPluginOVN    = "ovn"
	cniPluginCalico = "calico"
	cniPluginTigera = "tigera"

	// OVN Helm overrides for container nodes (CAPD and NICo). A dummy br-ex with
	// no uplink preserves Docker's management eth0. The kernel datapath sends
	// Geneve through the route on that interface. --allow-no-uplink needs local
	// gateway mode. The dsx-ai-factory chart hard-codes OVN_GATEWAY_MODE=shared
	// and has no global.gatewayMode, but appends global.gatewayOptions after
	// --gateway-mode, so the mode is overridden there:
	// https://github.com/dsx-ai-factory/ovn-kubernetes/blob/v2.4.2/charts/ovn-kubernetes/values.yaml
	ovnDummyGatewayBridgeHelmValue = "global.dummyGatewayBridge"
	ovnDummyGatewayBridgeEnabled   = "true"
	ovnGatewayOptsHelmValue        = "global.gatewayOptions"
	ovnDummyGatewayOpts            = "--gateway-mode=local --gateway-interface=br-ex --allow-no-uplink"
	// values-nke.yaml turns pod hostPort on. ovnkube-node refuses it outside
	// shared gateway mode, so container nodes turn it back off.
	ovnPodHostPortHelmValue = "global.enablePodHostPort"
	ovnPodHostPortDisabled  = "false"
)

// checkHostOpenVSwitchReady verifies ovs-vsctl is installed (ovs-vsctl --version).
func checkHostOpenVSwitchReady(runner nvkeutils.CommandRunner) error {
	out, err := runner.Run("ovs-vsctl", []string{"--version"}, &nvkeutils.CommandOptions{Env: os.Environ()})
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("%w: ovs-vsctl --version failed: %v (%s)", errOvnRequiresHostOpenVSwitch, err, msg)
		}
		return fmt.Errorf("%w: ovs-vsctl --version failed: %w", errOvnRequiresHostOpenVSwitch, err)
	}
	return nil
}

func (s *NodeAgentServer) applyTigeraManifestOverrides(path, operatorManifest, resourceManifest string) error {
	if operatorManifest != "" {
		operatorManifestPath := filepath.Join(path, "manifests", "tigera-operator.yaml")
		operatorManifestBackupPath := operatorManifestPath + ".bak"
		s.Runner.Run("mv", []string{operatorManifestPath, operatorManifestBackupPath}, &nvkeutils.CommandOptions{}) //nolint:all
		if err := os.WriteFile(operatorManifestPath, []byte(operatorManifest), 0o644); err != nil {
			return fmt.Errorf("failed to write operator manifest file: %w", err)
		}
	}

	if resourceManifest != "" {
		resourceManifestPath := filepath.Join(path, "manifests", "custom-resources.yaml")
		resourceManifestBackupPath := resourceManifestPath + ".bak"
		_, err := s.Runner.Run("mv", []string{resourceManifestPath, resourceManifestBackupPath}, &nvkeutils.CommandOptions{})
		if err != nil {
			return fmt.Errorf("failed to backup default manifest files: %w", err)
		}
		if err := os.WriteFile(resourceManifestPath, []byte(resourceManifest), 0o644); err != nil {
			return fmt.Errorf("failed to write custom resource manifest file: %w", err)
		}
	}

	return nil
}

func (s *NodeAgentServer) installCalico(version *semver.Version, operatorManifest, resourceManifest string) error {
	// TODO:  It would be nice to simply drop into "InstallBundle" here but CNI's are a little unique in that we usually provide
	// custom manifests.  For now those custom manifests aren't int he bundle so we're doing a two step process:
	// 1. Download/Extract the bundle
	// 2. Overwrite the manifests
	//
	// We can update the bundle to our "formula" and include everything in it then just call "installBundle"
	slog.Info("fetching calico bundle by index", "version", version.String(), "architecture", s.NodeStatus.Architecture)
	calicoBundle, err := nvkeutils.FetchBundleFromIndex(context.Background(), s.BundleServerURL, s.BundleIndexFile, cniPluginCalico, version, s.NodeStatus.Architecture)
	if err != nil {
		slog.Error("failed to fetch calico bundle", "error", err)
		return err
	}
	slog.Info("retrieved calico bundle by index", "Bundle", calicoBundle)

	requestID := generateRequestID()

	bundleStatus := &Bundle{
		RequestID:   requestID,
		Name:        calicoBundle.Name,
		Version:     version,
		InstallTime: time.Now().Format(time.RFC3339),
		Status:      InstallingStatus,
		Source:      calicoBundle.URL,
	}

	calicoBundle.InstallFile = "calico-install.py"
	calicoBundle.InstallCmd = []string{nvkeutils.ResolvePython(), "calico-install.py"}

	defer s.AppendBundleToStatus(bundleStatus)

	path, err := nvkeutils.DownloadAndExtractArchive(calicoBundle, s.BundleSource)
	if err != nil {
		bundleStatus.Status = ErrorStatus
		bundleStatus.Error = fmt.Sprintf("failed to download calico bundle: %v", err.Error())
		return err
	}

	if err := s.applyTigeraManifestOverrides(path, operatorManifest, resourceManifest); err != nil {
		bundleStatus.Error = err.Error()
		bundleStatus.Status = ErrorStatus
		return err
	}

	slog.Info("attempting install of calico bundle", "name", calicoBundle.Name, "url", calicoBundle.URL)
	// Ensure insall file is executable
	_, _ = s.Runner.Run("chmod", []string{"+x", calicoBundle.InstallFile}, &nvkeutils.CommandOptions{Dir: path})
	out, err := s.Runner.Run(calicoBundle.InstallCmd[0],
		calicoBundle.InstallCmd[1:],
		&nvkeutils.CommandOptions{
			Dir: path,
			Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
		})
	if err != nil {
		slog.Error("calico bundle install failed", "error", err, "output", string(out))
		bundleStatus.Error = fmt.Sprintf("failed to install calico bundle: %v", err.Error())
		bundleStatus.Status = ErrorStatus
		bundleStatus.Output = string(out)
		return err
	}

	installLog := filepath.Join(path, "tigera-install.log")
	logContents, _ := os.ReadFile(installLog)
	s.mu.Lock()
	s.NodeStatus.CalicoInstallLog = string(logContents)
	s.mu.Unlock()
	bundleStatus.Status = SuccessStatus
	bundleStatus.Output = string(logContents)

	return nil
}

const (
	ovnValuesFilename       = "values-nvke.yaml"
	legacyOVNValuesFilename = "values-nke.yaml"
)

// helmValuesFileArgs appends -f flags for standard value files present under chartDir.
// Later files win, so the chart's defaults go first and NVKE's override last.
func helmValuesFileArgs(chartDir string) []string {
	var args []string
	for _, name := range []string{"values.yaml", "values-images.yaml", "values-production.yaml", "values-single-node-zone.yaml", legacyOVNValuesFilename, ovnValuesFilename} {
		p := filepath.Join(chartDir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			args = append(args, "-f", p)
		}
	}
	return args
}

// ovnRegistryHelmValue is the controller's registry-override CniConfig key (shared with Calico);
// the OVN chart has no global.registry, so it is rewritten to global.imageRepositoryPrefix.
const ovnRegistryHelmValue = "global.registry"

// ovnImagePrefixHelmValue is the chart value every registry-less image.repository is prefixed with.
const ovnImagePrefixHelmValue = "global.imageRepositoryPrefix"

// ovnStagedImageRepository reads global.imageRepositoryPrefix from the values-nke.yaml bundle.sh wrote,
// the one place the packaged image's registry path lives.
func ovnStagedImageRepository(chartDir string) (string, error) {
	valuesFile := ovnValuesFilename
	raw, err := os.ReadFile(filepath.Join(chartDir, valuesFile))
	if errors.Is(err, os.ErrNotExist) {
		valuesFile = legacyOVNValuesFilename
		raw, err = os.ReadFile(filepath.Join(chartDir, valuesFile))
	}
	if err != nil {
		return "", err
	}
	var v struct {
		Global struct {
			ImageRepositoryPrefix string `yaml:"imageRepositoryPrefix"`
		}
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("parsing %s: %w", valuesFile, err)
	}
	if !strings.Contains(v.Global.ImageRepositoryPrefix, "/") {
		return "", fmt.Errorf("%s global.imageRepositoryPrefix %q has no registry host", valuesFile, v.Global.ImageRepositoryPrefix)
	}
	return v.Global.ImageRepositoryPrefix, nil
}

// ovnImageRepository swaps the registry host of the staged prefix for a mirror, keeping the path.
func ovnImageRepository(registry, upstreamRepo string) string {
	_, path, _ := strings.Cut(upstreamRepo, "/")
	return strings.TrimRight(registry, "/") + "/" + path
}

// ovnKubernetesCNIConfig keeps a container node's management interface out of
// the OVN gateway bridge. Without a dummy gateway bridge, OVN adopts the
// Docker veth as its uplink.
func ovnKubernetesCNIConfig(cniConfig map[string]string, datapathType string) map[string]string {
	if !containerOpenVSwitchDatapath(datapathType) {
		return cniConfig
	}

	config := make(map[string]string, len(cniConfig)+3)
	for key, value := range cniConfig {
		config[key] = value
	}
	if _, ok := config[ovnDummyGatewayBridgeHelmValue]; !ok {
		config[ovnDummyGatewayBridgeHelmValue] = ovnDummyGatewayBridgeEnabled
	}
	if _, ok := config[ovnGatewayOptsHelmValue]; !ok {
		config[ovnGatewayOptsHelmValue] = ovnDummyGatewayOpts
	}
	if _, ok := config[ovnPodHostPortHelmValue]; !ok {
		config[ovnPodHostPortHelmValue] = ovnPodHostPortDisabled
	}
	return config
}

// containerOpenVSwitchDatapath reports whether the node is a container whose
// Open vSwitch datapath must leave the Docker management interface unbridged.
// "system" is the kernel datapath. "netdev" is the userspace datapath. Only
// the container node image sets NKE_OVS_DATAPATH_TYPE, so bare metal and VM
// nodes never reach this even though they also run the kernel datapath.
func containerOpenVSwitchDatapath(datapathType string) bool {
	switch strings.ToLower(strings.TrimSpace(datapathType)) {
	case openVSwitchNetdevDatapath, openVSwitchSystemDatapath:
		return true
	default:
		return false
	}
}

// buildOvnKubernetesHelmInstallArgs returns argv for `helm upgrade --install` for an OVN chart
// directory. imageRegistry is set before cniConfig so a per-request registry wins; upstreamRepo
// is only read when one of them is set.
//
// `upgrade --install` and not `install`: this runs from a Temporal activity that retries, and
// `install` fails once the release exists, so the second attempt could never succeed.
func buildOvnKubernetesHelmInstallArgs(chartDir, apiServer, podNetworkCidr, serviceCidr, imageRegistry, upstreamRepo string, cniConfig map[string]string) []string {
	args := []string{
		"upgrade", "--install",
		"ovn-kubernetes",
		chartDir,
	}
	args = append(args, helmValuesFileArgs(chartDir)...)
	args = append(args,
		"--set", "k8sAPIServer="+apiServer,
		"--set-string", "podNetwork="+helmPodNetworkValue(podNetworkCidr),
		"--set", "serviceNetwork="+serviceCidr,
	)
	if imageRegistry != "" {
		args = append(args, "--set", ovnImagePrefixHelmValue+"="+ovnImageRepository(imageRegistry, upstreamRepo))
	}
	for key, value := range cniConfig {
		if key == "" {
			continue
		}
		if key == ovnRegistryHelmValue {
			key = ovnImagePrefixHelmValue
			value = ovnImageRepository(value, upstreamRepo)
		}
		if value == "" {
			value = "\"\""
		}
		args = append(args, "--set", fmt.Sprintf("%s=%s", key, value))
	}
	return args
}

// helmPodNetworkValue escapes commas so Helm passes all pod CIDR ranges through
// --set-string as one literal value instead of parsing them as separate assignments.
func helmPodNetworkValue(podNetworkCIDR string) string {
	return strings.ReplaceAll(podNetworkCIDR, ",", `\,`)
}

func kubeadmPodNetworkCIDR(podNetworkCIDR string) string {
	parts := strings.Split(podNetworkCIDR, ",")
	selected := make([]string, 0, len(parts))
	seenIPv4 := false
	seenIPv6 := false

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			selected = append(selected, part)
			continue
		}

		switch {
		case prefix.Addr().Is4():
			if seenIPv4 {
				continue
			}
			seenIPv4 = true
		case prefix.Addr().Is6():
			if seenIPv6 {
				continue
			}
			seenIPv6 = true
		}

		selected = append(selected, prefix.Masked().String())
	}
	return strings.Join(selected, ",")
}

func (s *NodeAgentServer) runOvnKubernetesHelmInstallFromDir(chartDir, podNetworkCidr, serviceCidr string, cniConfig map[string]string) error {
	apiServer, err := getKubeAPIServer(KubeConfig)
	if err != nil {
		return fmt.Errorf(
			"failed to parse apiserver endpoint from kubeconfig %s: %w",
			KubeConfig,
			err,
		)
	}
	var upstreamRepo string
	if s.CniImageRegistry != "" || cniConfig[ovnRegistryHelmValue] != "" {
		if upstreamRepo, err = ovnStagedImageRepository(chartDir); err != nil {
			return fmt.Errorf("resolving the OVN image repository for the registry override: %w", err)
		}
	}
	cniConfig = ovnKubernetesCNIConfig(cniConfig, os.Getenv(openVSwitchDatapathTypeEnv))
	args := buildOvnKubernetesHelmInstallArgs(chartDir, apiServer, podNetworkCidr, serviceCidr, s.CniImageRegistry, upstreamRepo, cniConfig)
	stdout, err := s.Runner.Run("helm", args, &nvkeutils.CommandOptions{
		Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
	})
	if err != nil {
		return fmt.Errorf(
			"failed to install chart ovn-kubernetes, stdout: %v, error: %v",
			string(stdout),
			err,
		)
	}
	return nil
}

func (s *NodeAgentServer) installOvnK8sCNIPlugin(req *pb.InstallCNIPluginRequest) error {
	slog.Info("installing OVN Kubernetes CNI plugin")

	if err := checkHostOpenVSwitchReady(s.Runner); err != nil {
		slog.Error("OVS readiness check failed", "error", err)
		return err
	}

	versionStr := req.GetPluginVersion()
	if versionStr == "" {
		return fmt.Errorf("ovn plugin requires a version")
	}
	version, err := semver.NewVersion(versionStr)
	if err != nil {
		return fmt.Errorf("failed to parse ovn version: %w", err)
	}

	ovnBundle, err := nvkeutils.FetchBundleFromIndex(context.Background(), s.BundleServerURL, s.BundleIndexFile, cniPluginOVN, version, s.NodeStatus.Architecture)
	if err != nil {
		return err
	}
	ovnBundle.InstallFile = "install.sh"
	// The downloaded script may live on a noexec /tmp mount.
	ovnBundle.InstallCmd = []string{"/bin/bash", ovnBundle.InstallFile}

	requestID := generateRequestID()
	bundleStatus := &Bundle{
		RequestID:   requestID,
		Name:        ovnBundle.Name,
		Version:     ovnBundle.Version,
		Status:      PendingStatus,
		InstallTime: time.Now().Format(time.RFC3339),
	}
	defer s.AppendBundleToStatus(bundleStatus)

	path, err := nvkeutils.DownloadAndExtractArchive(ovnBundle, s.BundleSource)
	if err != nil {
		return err
	}

	slog.Info("attempting install of ovn bundle", "name", ovnBundle.Name, "url", ovnBundle.URL)
	// Ensure install file is executable
	_, _ = s.Runner.Run("chmod", []string{"+x", ovnBundle.InstallFile}, &nvkeutils.CommandOptions{Dir: path})
	out, err := s.Runner.Run(ovnBundle.InstallCmd[0],
		ovnBundle.InstallCmd[1:],
		&nvkeutils.CommandOptions{
			Dir: path,
			Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
		})
	if err != nil {
		slog.Error("ovn-kubernetes bundle install failed", "error", err, "path", path, "output", string(out))
		bundleStatus.Error = fmt.Sprintf("failed to install ovn-kubernetes bundle: %v, path: %s", err.Error(), path)
		bundleStatus.Status = ErrorStatus
		return err
	}

	if err := s.runOvnKubernetesHelmInstallFromDir(ovnKubernetesHelmChartDir, req.GetPodNetworkCidr(), req.GetServiceCidr(), req.GetCniConfig()); err != nil {
		bundleStatus.Status = ErrorStatus
		bundleStatus.Error = err.Error()
		return err
	}
	bundleStatus.Status = SuccessStatus
	return nil
}

func aptBinaryPackageName(aptSpec string) string {
	aptSpec = strings.TrimSpace(aptSpec)
	if i := strings.IndexByte(aptSpec, '='); i >= 0 {
		return strings.TrimSpace(aptSpec[:i])
	}
	return aptSpec
}

// effectiveCniVariant maps UNSPECIFIED to OVN (default InstallCNI product).
func effectiveCniVariant(v pb.CniVariant) pb.CniVariant {
	if v == pb.CniVariant_CNI_VARIANT_UNSPECIFIED {
		return pb.CniVariant_CNI_VARIANT_OVN
	}
	return v
}

// alternateCniDebName returns the other spelling of a CNI deb name.
//
// New images install nvke-cni-* and Provide nke-cni-*, so apt accepts either
// request. dpkg only records the real name.
func alternateCniDebName(binPkg string) string {
	const current = "nvke-cni-"
	const previous = "nke-cni-"
	switch {
	case strings.HasPrefix(binPkg, current):
		return previous + strings.TrimPrefix(binPkg, current)
	case strings.HasPrefix(binPkg, previous):
		return current + strings.TrimPrefix(binPkg, previous)
	default:
		return ""
	}
}

// nvkeCniAptSpec builds an apt install spec for the discrete NVKE CNI packages.
//
// OVN Debian names include the Helm chart stream, e.g. nvke-cni-ovn-images-2.4.2 and
// nvke-cni-ovn-charts-2.4.2. Request version (after stripping a leading "v") selects that
// stream; when empty, defaultOvnCniChartStream is used. Non-empty version also appends
// =<version> for an apt equality pin on the Debian package version.
//
// Calico uses stream-suffixed names when installed from cni-packages, e.g.
// nvke-cni-calico-images-3.30.1 and nvke-cni-calico-manifests-3.30.1. Request version (after stripping a leading "v")
// selects that stream; when empty, defaultCalicoCniStream is used. Non-empty version also appends
// =<version> for an apt equality pin on the Debian package version.
//
// Legacy pkg-builder names (nke-calico-container-images / nke-calico-charts without stream suffix) remain
// installable but are not emitted by this helper.
//
// A single leading "v" on version is stripped when non-empty.
func nvkeCniAptSpec(variant pb.CniVariant, role pb.CniAptRole, version string) (string, error) {
	variant = effectiveCniVariant(variant)
	pin := strings.TrimSpace(version)
	pin = strings.TrimPrefix(pin, "v")
	pin = strings.TrimSpace(pin)

	var pkg string
	switch variant {
	case pb.CniVariant_CNI_VARIANT_UNSPECIFIED:
		return "", errors.New("cni_variant must be OVN or CALICO")
	case pb.CniVariant_CNI_VARIANT_OVN:
		stream := pin
		if stream == "" {
			stream = defaultOvnCniChartStream
		}
		switch role {
		case pb.CniAptRole_CNI_APT_ROLE_IMAGES:
			pkg = "nvke-cni-ovn-images-" + stream
		case pb.CniAptRole_CNI_APT_ROLE_CHARTS:
			pkg = "nvke-cni-ovn-charts-" + stream
		case pb.CniAptRole_CNI_APT_ROLE_UNSPECIFIED:
			return "", errors.New("cni_apt_role must be IMAGES or CHARTS")
		default:
			return "", errors.New("cni_apt_role must be IMAGES or CHARTS")
		}
		if pin == "" {
			return pkg, nil
		}
		return pkg + "=" + pin, nil
	case pb.CniVariant_CNI_VARIANT_CALICO:
		stream := pin
		if stream == "" {
			stream = defaultCalicoCniStream
		}
		switch role {
		case pb.CniAptRole_CNI_APT_ROLE_IMAGES:
			pkg = "nvke-cni-calico-images-" + stream
		case pb.CniAptRole_CNI_APT_ROLE_CHARTS:
			pkg = "nvke-cni-calico-manifests-" + stream
		case pb.CniAptRole_CNI_APT_ROLE_UNSPECIFIED:
			return "", errors.New("cni_apt_role must be IMAGES or CHARTS")
		default:
			return "", errors.New("cni_apt_role must be IMAGES or CHARTS")
		}
		if pin == "" {
			return pkg, nil
		}
		return pkg + "=" + pin, nil
	default:
		return "", errors.New("cni_variant must be OVN or CALICO")
	}
}

func nvkeCniChartsDebLogicalSpec(req *pb.InstallCNIRequest) (string, error) {
	if req == nil {
		return "", errors.New("request is nil")
	}
	return nvkeCniAptSpec(req.GetCniVariant(), pb.CniAptRole_CNI_APT_ROLE_CHARTS, req.GetVersion())
}

func dpkgInstalledVersion(ctx context.Context, binPkg string) (string, error) {
	cmd := exec.CommandContext(ctx, "dpkg-query", "--show", "--showformat=${Version}", binPkg)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("dpkg-query version for %q: %w", binPkg, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func dpkgListFiles(ctx context.Context, binPkg string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "dpkg-query", "-L", binPkg)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("dpkg-query -L %q: %w", binPkg, err)
	}
	var files []string
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\n"))
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func ovnHelmChartStaged() bool {
	for _, name := range []string{ovnValuesFilename, legacyOVNValuesFilename} {
		if _, err := os.Stat(filepath.Join(ovnKubernetesHelmChartDir, name)); err == nil {
			return true
		}
	}
	return false
}

func helmChartDirFromPackage(ctx context.Context, binPkg string) (string, error) {
	files, err := dpkgListFiles(ctx, binPkg)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if strings.HasSuffix(f, "/Chart.yaml") {
			return filepath.Dir(f), nil
		}
	}
	return "", fmt.Errorf("no Chart.yaml in package %q", binPkg)
}

// calicoInstallRootFromFiles picks the install root from paths listed by dpkg-query -L.
// Prefer a Helm chart directory that contains manifests/tigera-operator.yaml; otherwise use the
// parent of manifests/ for a static manifest-only package.
func calicoInstallRootFromFiles(files []string) (string, error) {
	for _, f := range files {
		if strings.HasSuffix(f, "/Chart.yaml") {
			dir := filepath.Dir(f)
			want := filepath.Join(dir, "manifests", "tigera-operator.yaml")
			for _, g := range files {
				if g == want {
					return dir, nil
				}
			}
		}
	}
	for _, f := range files {
		if strings.HasSuffix(f, "/manifests/tigera-operator.yaml") {
			return filepath.Dir(filepath.Dir(f)), nil
		}
	}
	return "", errors.New("no calico tigera manifests in package file list")
}

// calicoInstallRoot returns the directory that contains manifests/tigera-operator.yaml for Calico:
// either a Helm chart root (with Chart.yaml) or a static tree (e.g. nvke-cni-calico-manifests-* under /usr/share/nvke-cni/calico).
func calicoInstallRoot(ctx context.Context, binPkg string) (string, error) {
	files, err := dpkgListFiles(ctx, binPkg)
	if err != nil {
		return "", err
	}
	root, err := calicoInstallRootFromFiles(files)
	if err != nil {
		return "", fmt.Errorf("%w: %q", err, binPkg)
	}
	return root, nil
}

func ovnHelmChartDir(ctx context.Context, binPkg string) (string, error) {
	if ovnHelmChartStaged() {
		return ovnKubernetesHelmChartDir, nil
	}
	return helmChartDirFromPackage(ctx, binPkg)
}

func (s *NodeAgentServer) runCalicoHelmInstallFromDir(chartDir string, req *pb.InstallCNIRequest) error {
	// See buildOvnKubernetesHelmInstallArgs: retried activity, so the release
	// may already exist.
	args := []string{
		"upgrade", "--install", "tigera-operator", chartDir,
		"--namespace", "tigera-operator",
		"--create-namespace",
	}
	args = append(args, helmValuesFileArgs(chartDir)...)
	if s.CniImageRegistry != "" {
		args = append(args, "--set", "global.registry="+s.CniImageRegistry)
	}
	if pod := strings.TrimSpace(req.GetPodNetworkCidr()); pod != "" {
		// Tigera operator chart renders Installation.spec from .Values.installation (see custom-resources.yaml).
		args = append(args, "--set-string", "installation.calicoNetwork.ipPools[0].cidr="+pod)
	}
	for key, value := range req.GetCniConfig() {
		if key == "" {
			continue
		}
		if value == "" {
			value = "\"\""
		}
		args = append(args, "--set", fmt.Sprintf("%s=%s", key, value))
	}
	stdout, err := s.Runner.Run("helm", args, &nvkeutils.CommandOptions{
		Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
	})
	if err != nil {
		return fmt.Errorf("helm upgrade --install calico charts: %w, output: %s", err, string(stdout))
	}
	return nil
}

// calicoDefaultIPPoolsCIDRLine is the bundled default in cni-packages custom-resources.yaml; must stay in sync
// with debs/calico/custom-resources.yaml (cidr for the default-ipv4-ippool).
const calicoDefaultIPPoolsCIDRLine = "cidr: 192.168.0.0/16"

// calicoKubectlEnv returns os.Environ() plus KUBECONFIG and, when non-empty, CALICO_POD_CIDR for apply-manifests.sh.
func calicoKubectlEnv(podNetworkCidr string) []string {
	env := append(os.Environ(), "KUBECONFIG="+KubeConfig)
	if c := strings.TrimSpace(podNetworkCidr); c != "" {
		env = append(env, "CALICO_POD_CIDR="+c)
	}
	return env
}

// calicoCustomResourcesApplyPath returns the filesystem path to pass to kubectl for Calico custom resources.
// When podNetworkCidr is empty it is the packaged file; otherwise the default pool CIDR line is substituted
// into a temp file (caller must run cleanup). When podNetworkCidr is non-empty and that line is missing, returns an error.
func calicoCustomResourcesApplyPath(manifestsDir, podNetworkCidr string) (string, func(), error) {
	base := filepath.Join(manifestsDir, "custom-resources.yaml")
	c := strings.TrimSpace(podNetworkCidr)
	if c == "" {
		return base, nil, nil
	}
	raw, err := os.ReadFile(base)
	if err != nil {
		return "", nil, err
	}
	s := string(raw)
	const needle = calicoDefaultIPPoolsCIDRLine
	if !strings.Contains(s, needle) {
		return "", nil, fmt.Errorf(
			"calico custom-resources %s: cannot apply requested pod network CIDR %q: expected default pool line %q not found in manifest",
			base, c, needle,
		)
	}
	replacement := "cidr: " + c
	rendered := strings.ReplaceAll(s, needle, replacement)
	tmp, err := os.CreateTemp("", "nke-calico-custom-resources-*.yaml")
	if err != nil {
		return "", nil, err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(rendered); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", nil, err
	}
	return tmpPath, func() { _ = os.Remove(tmpPath) }, nil
}

func (s *NodeAgentServer) runCalicoKubectlApplyFromDir(root, podNetworkCidr string) error {
	applyScript := filepath.Join(root, "apply-manifests.sh")
	if st, err := os.Stat(applyScript); err == nil && !st.IsDir() {
		out, err := s.Runner.Run(applyScript, nil, &nvkeutils.CommandOptions{
			Dir: root,
			Env: calicoKubectlEnv(podNetworkCidr),
		})
		if err != nil {
			return fmt.Errorf("run %s: %w, output: %s", applyScript, err, string(out))
		}
		return nil
	}

	manifestsDir := filepath.Join(root, "manifests")
	opPath := filepath.Join(manifestsDir, "tigera-operator.yaml")
	crPath, crCleanup, err := calicoCustomResourcesApplyPath(manifestsDir, podNetworkCidr)
	if err != nil {
		return fmt.Errorf("calico custom resources prepare: %w", err)
	}
	if crCleanup != nil {
		defer crCleanup()
	}
	paths := []string{opPath, crPath}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("calico manifest %s: %w", p, err)
		}
	}
	// Newer bundles carry CRDs separately. Older bundles can embed them in the
	// operator manifest. Keep the existing install path when this file is absent.
	crdsPath := filepath.Join(manifestsDir, "operator-crds.yaml")
	if _, err := os.Stat(crdsPath); err == nil {
		commands := [][]string{
			{"apply", "--server-side", "-f", crdsPath}, //nolint:goconst // kubectl argv fragment; naming each CLI flag reads worse than the literal
			{"wait", "--for=condition=Established", "--timeout=120s", "-f", crdsPath},
		}
		for _, args := range commands {
			out, err := s.Runner.Run("kubectl", args, &nvkeutils.CommandOptions{
				Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
			})
			if err != nil {
				return fmt.Errorf("kubectl %s calico CRDs: %w, output: %s", args[0], err, out)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat calico CRDs: %w", err)
	}
	args := []string{"apply", "-f", paths[0], "-f", paths[1]}
	stdout, err := s.Runner.Run("kubectl", args, &nvkeutils.CommandOptions{
		Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
	})
	if err != nil {
		return fmt.Errorf("kubectl apply calico manifests: %w, output: %s", err, string(stdout))
	}
	return nil
}

func (s *NodeAgentServer) runCniChartsDeploy(ctx context.Context, req *pb.InstallCNIRequest, binPkg string, bundleStatus *Bundle) error {
	switch effectiveCniVariant(req.GetCniVariant()) {
	case pb.CniVariant_CNI_VARIANT_UNSPECIFIED:
		err := errors.New("cni_variant must be OVN or CALICO")
		bundleStatus.Status = ErrorStatus
		bundleStatus.Error = err.Error()
		return err
	case pb.CniVariant_CNI_VARIANT_OVN:
		chartDir, err := ovnHelmChartDir(ctx, binPkg)
		if err != nil {
			bundleStatus.Status = ErrorStatus
			bundleStatus.Error = err.Error()
			return err
		}
		if err := s.runOvnKubernetesHelmInstallFromDir(chartDir, req.GetPodNetworkCidr(), req.GetServiceCidr(), req.GetCniConfig()); err != nil {
			bundleStatus.Status = ErrorStatus
			bundleStatus.Error = err.Error()
			return err
		}
	case pb.CniVariant_CNI_VARIANT_CALICO:
		root, err := calicoInstallRoot(ctx, binPkg)
		if err != nil {
			bundleStatus.Status = ErrorStatus
			bundleStatus.Error = err.Error()
			return err
		}
		tigeraOpPath := filepath.Join(root, "manifests", "tigera-operator.yaml")
		if req.GetTigeraOperatorManifest() != "" || req.GetTigeraCustomResourceManifest() != "" {
			if _, statErr := os.Stat(tigeraOpPath); statErr != nil {
				err := fmt.Errorf("tigera manifest overrides requested but %s not found: %w", tigeraOpPath, statErr)
				bundleStatus.Status = ErrorStatus
				bundleStatus.Error = err.Error()
				return err
			}
			if err := s.applyTigeraManifestOverrides(root, req.GetTigeraOperatorManifest(), req.GetTigeraCustomResourceManifest()); err != nil {
				bundleStatus.Status = ErrorStatus
				bundleStatus.Error = err.Error()
				return err
			}
		}
		var deployErr error
		if _, statErr := os.Stat(filepath.Join(root, "Chart.yaml")); statErr == nil {
			deployErr = s.runCalicoHelmInstallFromDir(root, req)
		} else {
			deployErr = s.runCalicoKubectlApplyFromDir(root, req.GetPodNetworkCidr())
		}
		if deployErr != nil {
			bundleStatus.Status = ErrorStatus
			bundleStatus.Error = deployErr.Error()
			return deployErr
		}
		installLog := filepath.Join(root, "tigera-install.log")
		if logContents, readErr := os.ReadFile(installLog); readErr == nil {
			s.mu.Lock()
			s.NodeStatus.CalicoInstallLog = string(logContents)
			s.mu.Unlock()
			bundleStatus.Output = string(logContents)
		}
	default:
		err := errors.New("cni_variant must be OVN or CALICO")
		bundleStatus.Status = ErrorStatus
		bundleStatus.Error = err.Error()
		return err
	}
	bundleStatus.Status = SuccessStatus
	return nil
}

func (s *NodeAgentServer) installCNI(ctx context.Context, req *pb.InstallCNIRequest, chartsPkgLogical string) error {
	role := strings.TrimSpace(s.NodeStatus.Role)
	if !strings.EqualFold(role, ControlPlaneRole) {
		return fmt.Errorf("%w (current role=%q)", errCNIApplyRequiresControlPlane, role)
	}

	chartsPkgLogical = strings.TrimSpace(chartsPkgLogical)
	if chartsPkgLogical == "" {
		return errors.New("resolved charts package spec is empty")
	}
	binPkg := aptBinaryPackageName(chartsPkgLogical)
	if binPkg == "" {
		slog.ErrorContext(ctx, "InstallCNI failed", "operation", "validate charts package spec", "error", errors.New("invalid package spec"), "charts_spec", chartsPkgLogical)
		return errors.New("invalid charts package spec")
	}

	if effectiveCniVariant(req.GetCniVariant()) == pb.CniVariant_CNI_VARIANT_OVN {
		if err := checkHostOpenVSwitchReady(s.Runner); err != nil {
			slog.ErrorContext(ctx, "OVS readiness check failed", "error", err)
			return err
		}
	}

	if err := chartsDebPresenceCheck(ctx, binPkg); err != nil {
		// dpkg-query sees the real package name, not a Provides alias. A caller
		// that still names nke-cni-* finds the nvke-cni-* package, and the
		// reverse, so either image satisfies the check.
		alt := alternateCniDebName(binPkg)
		if alt == "" {
			return err
		}
		if altErr := chartsDebPresenceCheck(ctx, alt); altErr != nil {
			return err
		}
		binPkg = alt
	}

	slog.InfoContext(ctx, "InstallCNI charts deploy",
		"charts_spec", chartsPkgLogical,
		"binary_package", binPkg,
		"cni_variant", effectiveCniVariant(req.GetCniVariant()).String(),
	)

	requestID := generateRequestID()
	bundleStatus := &Bundle{
		RequestID:   requestID,
		Name:        binPkg,
		Status:      InstallingStatus,
		InstallTime: time.Now().Format(time.RFC3339),
		Source:      "charts:" + chartsPkgLogical,
	}
	defer s.AppendBundleToStatus(bundleStatus)

	if verStr, err := dpkgInstalledVersion(ctx, binPkg); err == nil && verStr != "" {
		if v, err := semver.NewVersion(verStr); err == nil {
			bundleStatus.Version = v
		}
	}

	return s.runCniChartsDeploy(ctx, req, binPkg, bundleStatus)
}

func getKubeAPIServer(kubeconfig string) (string, error) {
	// Read the kubeconfig file
	content, err := os.ReadFile(kubeconfig)
	if err != nil {
		return "", fmt.Errorf("failed to read kubeconfig: %v", err)
	}

	// Parse YAML into a map
	var config map[string]any
	if err := yaml.Unmarshal(content, &config); err != nil {
		return "", fmt.Errorf("failed to parse kubeconfig: %v", err)
	}

	// Navigate the map to find the server URL
	clusters, ok := config["clusters"].([]any)
	if !ok || len(clusters) == 0 {
		return "", fmt.Errorf("no clusters found in kubeconfig")
	}

	cluster, ok := clusters[0].(map[any]any)
	if !ok {
		return "", fmt.Errorf("invalid cluster format in kubeconfig")
	}

	clusterInfo, ok := cluster["cluster"].(map[any]any)
	if !ok {
		return "", fmt.Errorf("invalid cluster info format in kubeconfig")
	}

	server, ok := clusterInfo["server"].(string)
	if !ok {
		return "", fmt.Errorf("server URL not found in kubeconfig")
	}
	return server, nil
}
