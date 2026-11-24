// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package containerengine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBin writes an executable script named name into dir.
func fakeBin(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755))
	return path
}

// fakeSudo stands in for `sudo -n [--preserve-env=...] CMD...`: it records what
// it was given, drops its own options and runs CMD as the current user.
func fakeSudo(t *testing.T, dir, log string) {
	t.Helper()
	fakeBin(t, dir, "sudo", `echo "$*" >> '`+log+`'
while [ "${1#-}" != "$1" ]; do shift; done
exec "$@"
`)
}

// runBash runs script under bash with exactly env, and returns its combined
// output and error.
func runBash(t *testing.T, env []string, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{"-c", script, "--"}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// toolsDir returns a directory of links to just the named host tools, so a test
// can put it on PATH without every other program in /usr/bin coming along.
func toolsDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path, err := exec.LookPath(name)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(path, filepath.Join(dir, name)))
	}
	return dir
}

func libPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("lib.sh")
	require.NoError(t, err)
	return abs
}

func shimDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(libPath(t)), "shim")
}

func baseEnv(extra ...string) []string {
	return append([]string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}, extra...)
}

func TestCEInit(t *testing.T) {
	lib := libPath(t)
	shim := shimDir(t)

	tests := []struct {
		name         string
		cli          string
		wantPathHead string
		wantProvider string
		wantError    string
	}{
		{name: "unset means docker and changes nothing", cli: "", wantPathHead: "/usr/bin", wantProvider: ""},
		{name: "docker changes nothing", cli: "docker", wantPathHead: "/usr/bin", wantProvider: ""},
		{name: "nerdctl puts the root shim first and selects kind's provider", cli: "nerdctl", wantPathHead: shim, wantProvider: "nerdctl"},
		{name: "ctrd puts its own shim directory first and has kind run its nerdctl", cli: "ctrd", wantPathHead: shim + "/ctrd", wantProvider: "nerdctl"},
		{name: "podman is not supported", cli: "podman", wantError: "CONTAINER_CLI=podman is not supported"},
		{name: "a path is not supported", cli: "/usr/bin/docker", wantError: "is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			if tt.cli != "" {
				env = append(env, "CONTAINER_CLI="+tt.cli)
			}
			out, err := runBash(t, env, `source "$1"; ce_init && ce_init && echo "PATH=${PATH}" && echo "PROVIDER=${KIND_EXPERIMENTAL_PROVIDER:-}"`, lib)
			if tt.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, out, tt.wantError)
				return
			}
			require.NoError(t, err, out)
			assert.Contains(t, out, "PATH="+tt.wantPathHead)
			assert.Contains(t, out, "PROVIDER="+tt.wantProvider+"\n")
			// ce_init ran twice above: the shim directory is on PATH once.
			wantShimEntries := 0
			if tt.cli == "nerdctl" || tt.cli == "ctrd" {
				wantShimEntries = 1
			}
			pathLine := strings.SplitN(strings.SplitN(out, "PATH=", 2)[1], "\n", 2)[0]
			assert.Equal(t, wantShimEntries, strings.Count(pathLine, shim), pathLine)
		})
	}
}

func TestCEBuild(t *testing.T) {
	lib := libPath(t)
	tests := []struct {
		name string
		cli  string
		want string
	}{
		{name: "docker builds with BuildKit as before", cli: "docker", want: "docker[DOCKER_BUILDKIT=1] build -t img -f Dockerfile ctx"},
		{name: "unset is docker", cli: "", want: "docker[DOCKER_BUILDKIT=1] build -t img -f Dockerfile ctx"},
		{name: "nerdctl builds with the engine", cli: "nerdctl", want: "nerdctl[] build -t img -f Dockerfile ctx"},
		{name: "ctrd builds with the engine", cli: "ctrd", want: "ctrd[] build -t img -f Dockerfile ctx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := t.TempDir()
			for _, name := range []string{"docker", "nerdctl", "ctrd"} {
				fakeBin(t, bin, name, `echo "`+name+`[${DOCKER_BUILDKIT:+DOCKER_BUILDKIT=${DOCKER_BUILDKIT}}] $*"`+"\n")
			}
			env := baseEnv("PATH="+bin+":/usr/bin:/bin", "CONTAINER_CLI="+tt.cli)
			out, err := runBash(t, env, `source "$1"; ce_build -t img -f Dockerfile ctx`, lib)
			require.NoError(t, err, out)
			assert.Equal(t, tt.want+"\n", out)
		})
	}
}

func TestCEKindLoad(t *testing.T) {
	lib := libPath(t)

	setup := func(t *testing.T, nerdctlSave string) ([]string, string) {
		t.Helper()
		bin := t.TempDir()
		log := filepath.Join(t.TempDir(), "calls")
		fakeSudo(t, bin, filepath.Join(t.TempDir(), "sudo"))
		fakeBin(t, bin, "kind", `echo "kind $*" >> '`+log+`'`+"\n")
		fakeBin(t, bin, "nerdctl", nerdctlSave)
		return baseEnv("PATH="+bin+":/usr/bin:/bin", "CONTAINER_CLI=nerdctl", "TMPDIR="+t.TempDir()), log
	}

	t.Run("docker loads the image by name", func(t *testing.T) {
		env, log := setup(t, "")
		env = append(env, "CONTAINER_CLI=docker")
		out, err := runBash(t, env, `source "$1"; ce_kind_load img:tag my-cluster`, lib)
		require.NoError(t, err, out)
		got, err := os.ReadFile(log)
		require.NoError(t, err)
		assert.Equal(t, "kind load docker-image img:tag --name my-cluster\n", string(got))
	})

	t.Run("nerdctl saves an archive and loads that", func(t *testing.T) {
		env, log := setup(t, `echo "nerdctl $*" >> "${CALLS}"
case "$1" in save) : > "$3" ;; esac
`)
		env = append(env, "CALLS="+log)
		out, err := runBash(t, env, `source "$1"; ce_init && ce_kind_load img:tag my-cluster`, lib)
		require.NoError(t, err, out)
		got, err := os.ReadFile(log)
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSpace(string(got)), "\n")
		require.Len(t, lines, 2, string(got))
		save := strings.Fields(lines[0])
		require.Len(t, save, 5, lines[0])
		assert.Equal(t, []string{"nerdctl", "save", "-o"}, save[:3])
		assert.Equal(t, "img:tag", save[4])
		archive := save[3]
		assert.Equal(t, "kind load image-archive "+archive+" --name my-cluster", lines[1])
		// The archive's directory is gone: the engine wrote it as root, and the
		// caller has no use for it.
		assert.NoFileExists(t, archive)
		assert.NoDirExists(t, filepath.Dir(archive))
	})

	t.Run("ctrd saves an archive and loads that, with kind's provider selected", func(t *testing.T) {
		bin := t.TempDir()
		log := filepath.Join(t.TempDir(), "calls")
		fakeSudo(t, bin, filepath.Join(t.TempDir(), "sudo"))
		fakeBin(t, bin, "kind", `echo "kind provider=${KIND_EXPERIMENTAL_PROVIDER} $*" >> '`+log+`'`+"\n")
		engineBin := fakeBin(t, bin, "bin-ctrd", `echo "ctrd $*" >> '`+log+`'
case "$1" in save) : > "$3" ;; esac
`)
		env := baseEnv("PATH="+bin+":/usr/bin:/bin", "CONTAINER_CLI=ctrd", "CTRD_REAL="+engineBin, "TMPDIR="+t.TempDir())
		out, err := runBash(t, env, `source "$1"; ce_init && ce_kind_load img:tag my-cluster`, lib)
		require.NoError(t, err, out)
		got, err := os.ReadFile(log)
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSpace(string(got)), "\n")
		require.Len(t, lines, 2, string(got))
		assert.Regexp(t, `^ctrd save -o \S+/image.tar img:tag$`, lines[0])
		assert.Regexp(t, `^kind provider=nerdctl load image-archive \S+/image.tar --name my-cluster$`, lines[1])
	})

	t.Run("a failed save loads nothing and fails", func(t *testing.T) {
		env, log := setup(t, "exit 3\n")
		out, err := runBash(t, env, `source "$1"; ce_init && ce_kind_load img:tag my-cluster`, lib)
		require.Error(t, err, out)
		assert.NoFileExists(t, log)
	})
}

func TestCEBridgeForwarding(t *testing.T) {
	lib := libPath(t)
	const networkID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const bridge = "br-0123456789ab"

	setup := func(t *testing.T, ruleExists bool) ([]string, string, string) {
		t.Helper()
		bin := t.TempDir()
		log := filepath.Join(t.TempDir(), "iptables")
		fakeSudo(t, bin, filepath.Join(t.TempDir(), "sudo"))
		fakeBin(t, bin, "nerdctl", `case "$1 $2" in "network inspect") echo `+networkID+` ;; *) exit 1 ;; esac`+"\n")
		check := "exit 1"
		if ruleExists {
			check = "exit 0"
		}
		fakeBin(t, bin, "iptables", `echo "$*" >> '`+log+`'
case "$1" in -C) `+check+` ;; esac
`)
		return baseEnv("PATH="+bin+":/usr/bin:/bin", "CONTAINER_CLI=nerdctl"), log, bin
	}

	t.Run("adds the rule once, at the top of FORWARD", func(t *testing.T) {
		env, log, _ := setup(t, false)
		out, err := runBash(t, env, `source "$1"; ce_init && ce_allow_same_bridge_forwarding kind`, lib)
		require.NoError(t, err, out)
		got, err := os.ReadFile(log)
		require.NoError(t, err)
		assert.Equal(t, "-C FORWARD -i "+bridge+" -o "+bridge+" -j ACCEPT\n-I FORWARD 1 -i "+bridge+" -o "+bridge+" -j ACCEPT\n", string(got))
	})

	t.Run("leaves an existing rule alone", func(t *testing.T) {
		env, log, _ := setup(t, true)
		out, err := runBash(t, env, `source "$1"; ce_init && ce_allow_same_bridge_forwarding kind`, lib)
		require.NoError(t, err, out)
		got, err := os.ReadFile(log)
		require.NoError(t, err)
		assert.Equal(t, "-C FORWARD -i "+bridge+" -o "+bridge+" -j ACCEPT\n", string(got))
	})

	t.Run("removes the rule", func(t *testing.T) {
		env, log, _ := setup(t, true)
		out, err := runBash(t, env, `source "$1"; ce_init && ce_remove_same_bridge_forwarding kind`, lib)
		require.NoError(t, err, out)
		got, err := os.ReadFile(log)
		require.NoError(t, err)
		assert.Equal(t, "-D FORWARD -i "+bridge+" -o "+bridge+" -j ACCEPT\n", string(got))
	})

	t.Run("removal with the network gone is not an error", func(t *testing.T) {
		env, log, bin := setup(t, true)
		fakeBin(t, bin, "nerdctl", "exit 1\n")
		out, err := runBash(t, env, `source "$1"; ce_init && ce_remove_same_bridge_forwarding kind`, lib)
		require.NoError(t, err, out)
		assert.NoFileExists(t, log)
	})

	t.Run("ctrd needs the rule too", func(t *testing.T) {
		env, log, bin := setup(t, false)
		engineBin := fakeBin(t, bin, "bin-ctrd", `case "$1 $2" in "network inspect") echo `+networkID+` ;; *) exit 1 ;; esac`+"\n")
		env = append(env, "CONTAINER_CLI=ctrd", "CTRD_REAL="+engineBin)
		out, err := runBash(t, env, `source "$1"; ce_init && ce_allow_same_bridge_forwarding kind && ce_remove_same_bridge_forwarding kind`, lib)
		require.NoError(t, err, out)
		got, err := os.ReadFile(log)
		require.NoError(t, err)
		assert.Equal(t, "-C FORWARD -i "+bridge+" -o "+bridge+" -j ACCEPT\n-I FORWARD 1 -i "+bridge+" -o "+bridge+" -j ACCEPT\n-D FORWARD -i "+bridge+" -o "+bridge+" -j ACCEPT\n", string(got))
	})

	t.Run("docker does nothing", func(t *testing.T) {
		env, log, _ := setup(t, false)
		env = append(env, "CONTAINER_CLI=docker")
		out, err := runBash(t, env, `source "$1"; ce_allow_same_bridge_forwarding kind && ce_remove_same_bridge_forwarding kind`, lib)
		require.NoError(t, err, out)
		assert.NoFileExists(t, log)
	})
}

func TestNerdctlShim(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the shim runs nerdctl directly as root; this covers the sudo path")
	}
	shim := filepath.Join(shimDir(t), "nerdctl")

	t.Run("runs the real nerdctl, not itself, through sudo -n", func(t *testing.T) {
		bin := t.TempDir()
		sudoLog := filepath.Join(t.TempDir(), "sudo")
		fakeSudo(t, bin, sudoLog)
		fakeBin(t, bin, "nerdctl", `echo "real nerdctl $*"`+"\n")
		cmd := exec.CommandContext(t.Context(), shim, "ps", "-a")
		cmd.Env = baseEnv("PATH="+shimDir(t)+":"+bin+":/usr/bin:/bin", "CONTAINERD_ADDRESS=/run/test.sock", "BUILDKIT_HOST=unix:///run/bk.sock", "UNRELATED=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		assert.Equal(t, "real nerdctl ps -a\n", string(out))
		got, err := os.ReadFile(sudoLog)
		require.NoError(t, err)
		assert.Equal(t, "-n --preserve-env=CONTAINERD_ADDRESS,BUILDKIT_HOST "+filepath.Join(bin, "nerdctl")+" ps -a\n", string(got))
	})

	t.Run("asks sudo to preserve nothing when nothing is set", func(t *testing.T) {
		bin := t.TempDir()
		sudoLog := filepath.Join(t.TempDir(), "sudo")
		fakeSudo(t, bin, sudoLog)
		fakeBin(t, bin, "nerdctl", "true\n")
		cmd := exec.CommandContext(t.Context(), shim, "info")
		cmd.Env = baseEnv("PATH=" + shimDir(t) + ":" + bin + ":/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		got, err := os.ReadFile(sudoLog)
		require.NoError(t, err)
		assert.Equal(t, "-n "+filepath.Join(bin, "nerdctl")+" info\n", string(got))
	})

	t.Run("passes the exit status and standard input through", func(t *testing.T) {
		bin := t.TempDir()
		fakeSudo(t, bin, filepath.Join(t.TempDir(), "sudo"))
		fakeBin(t, bin, "nerdctl", "cat; exit 7\n")
		cmd := exec.CommandContext(t.Context(), shim, "exec", "-i", "c")
		cmd.Env = baseEnv("PATH=" + shimDir(t) + ":" + bin + ":/usr/bin:/bin")
		cmd.Stdin = strings.NewReader("payload")
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		assert.Equal(t, 7, exitErr.ExitCode())
		assert.Equal(t, "payload", string(out))
	})

	t.Run("NERDCTL_REAL names the binary", func(t *testing.T) {
		bin := t.TempDir()
		fakeSudo(t, bin, filepath.Join(t.TempDir(), "sudo"))
		elsewhere := fakeBin(t, bin, "nerdctl-elsewhere", `echo "elsewhere $*"`+"\n")
		cmd := exec.CommandContext(t.Context(), shim, "version")
		cmd.Env = baseEnv("PATH="+shimDir(t)+":"+bin+":/usr/bin:/bin", "NERDCTL_REAL="+elsewhere)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		assert.Equal(t, "elsewhere version\n", string(out))
	})

	t.Run("no other nerdctl on PATH is an error that says so", func(t *testing.T) {
		tools := toolsDir(t, "bash", "dirname")
		cmd := exec.CommandContext(t.Context(), shim, "version")
		cmd.Env = baseEnv("PATH=" + shimDir(t) + ":" + tools)
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		assert.Equal(t, 127, exitErr.ExitCode())
		assert.Contains(t, string(out), "no nerdctl on PATH besides")
	})
}

// dockerBridgeAddrs is `ip -o -6 addr show` on a host where Docker's kind
// network left its bridge behind.
const dockerBridgeAddrs = `1: lo    inet6 ::1/128 scope host noprefixroute \       valid_lft forever preferred_lft forever
2: wlan0    inet6 2001:db8::4/64 scope global dynamic noprefixroute \       valid_lft 100sec preferred_lft 100sec
30: br-0123456789ab    inet6 fc00:f853:ccd:e793::1/64 scope global \       valid_lft forever preferred_lft forever
30: br-0123456789ab    inet6 fe80::1/64 scope link \       valid_lft forever preferred_lft forever
`

func TestCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("check.sh skips the sudo probe as root; this covers the sudo path")
	}
	check, err := filepath.Abs("check.sh")
	require.NoError(t, err)

	type host struct {
		sudoFails      bool
		version        string
		infoFails      bool
		missingPlugins []string
		noBuildctl     bool
		noBuildkitd    bool
		ociWorker      bool
		kindNetwork    bool
		ipv6Addrs      string
	}
	run := func(t *testing.T, h host) (string, error) {
		t.Helper()
		bin := t.TempDir()
		cni := t.TempDir()
		dataRoot := t.TempDir()
		toml := filepath.Join(t.TempDir(), "nerdctl.toml")
		require.NoError(t, os.WriteFile(toml, []byte("data_root = \""+dataRoot+"\"\n"), 0o644))

		if h.sudoFails {
			fakeBin(t, bin, "sudo", "echo 'sudo: a password is required' >&2; exit 1\n")
		} else {
			fakeSudo(t, bin, filepath.Join(t.TempDir(), "sudo"))
		}
		version := h.version
		if version == "" {
			version = "2.4.2"
		}
		kindNetwork := "exit 1"
		if h.kindNetwork {
			kindNetwork = "exit 0"
		}
		info := `echo v2.4.2`
		if h.infoFails {
			info = `echo 'cannot access containerd socket' >&2; exit 1`
		}
		fakeBin(t, bin, "nerdctl", `case "$1" in
  --version) echo "nerdctl version `+version+`" ;;
  info) `+info+` ;;
  network) `+kindNetwork+` ;;
esac
`)
		plugins := map[string]bool{"bridge": true, "portmap": true, "firewall": true, "tuning": true, "host-local": true, "loopback": true}
		for _, p := range h.missingPlugins {
			delete(plugins, p)
		}
		for p := range plugins {
			fakeBin(t, cni, p, "true\n")
		}
		if !h.noBuildctl {
			worker := `"org.mobyproject.buildkit.worker.executor":"containerd","org.mobyproject.buildkit.worker.containerd.namespace":"default"`
			if h.ociWorker {
				worker = `"org.mobyproject.buildkit.worker.executor":"oci"`
			}
			fakeBin(t, bin, "buildctl", `if [ "`+map[bool]string{true: "no", false: "yes"}[h.noBuildkitd]+`" = no ]; then echo 'connection refused' >&2; exit 1; fi
echo '[{"labels":{`+worker+`}}]'
`)
		}
		fakeBin(t, bin, "iptables", "true\n")
		fakeBin(t, bin, "ip", "cat <<'EOF'\n"+h.ipv6Addrs+"EOF\n")
		fakeBin(t, bin, "tar", "true\n")

		// Only these host tools: a buildctl or an iptables in /usr/bin must not
		// answer for the one a case removes.
		tools := toolsDir(t, "bash", "sh", "sed", "sort", "head", "stat", "dirname", "env", "true", "awk", "cat")
		env := baseEnv("PATH="+bin+":"+tools, "CONTAINER_CLI=nerdctl", "CNI_PATH="+cni, "NERDCTL_TOML="+toml)
		return runBash(t, env, `exec bash "$1"`, check)
	}

	t.Run("a host that is ready passes", func(t *testing.T) {
		out, err := run(t, host{})
		require.NoError(t, err, out)
		for _, want := range []string{"ok: nerdctl 2.4.2", "ok: containerd v2.4.2 answers", "ok: CNI plugins", "ok: buildkitd answers", "ok: nerdctl data root"} {
			assert.Contains(t, out, want)
		}
		assert.NotContains(t, out, "WARN")
		assert.Contains(t, out, "ok: no interface holds kind's IPv6 subnet")
	})

	t.Run("docker is not checked", func(t *testing.T) {
		out, err := runBash(t, baseEnv(), `exec bash "$1"`, check)
		require.NoError(t, err, out)
		assert.Empty(t, out)
	})

	t.Run("an unsupported engine is rejected", func(t *testing.T) {
		out, err := runBash(t, baseEnv("CONTAINER_CLI=podman"), `exec bash "$1"`, check)
		require.Error(t, err)
		assert.Contains(t, out, "CONTAINER_CLI=podman is not supported")
	})

	failures := map[string]struct {
		host host
		want []string
	}{
		"sudo needs a password":            {host{sudoFails: true}, []string{"passwordless sudo is required"}},
		"nerdctl older than kind supports": {host{version: "1.6.2"}, []string{"nerdctl 1.6.2 is older than 1.7.0"}},
		"containerd is unreachable":        {host{infoFails: true}, []string{"cannot reach containerd", "cannot access containerd socket"}},
		"CNI plugins are missing":          {host{missingPlugins: []string{"firewall", "bridge"}}, []string{"CNI plugins missing", "firewall", "bridge"}},
		"buildctl is missing":              {host{noBuildctl: true}, []string{"buildctl is not on root's PATH"}},
		"buildkitd is not running":         {host{noBuildkitd: true}, []string{"buildkitd does not answer"}},
		"several problems are all reported": {
			host{missingPlugins: []string{"portmap"}, noBuildkitd: true},
			[]string{"CNI plugins missing", "buildkitd does not answer", "2 problem(s)"},
		},
	}
	for name, tt := range failures {
		t.Run(name, func(t *testing.T) {
			out, err := run(t, tt.host)
			require.Error(t, err, out)
			for _, want := range tt.want {
				assert.Contains(t, out, want)
			}
		})
	}

	t.Run("a leftover Docker bridge holding kind's subnet is named, with the command to remove it", func(t *testing.T) {
		out, err := run(t, host{ipv6Addrs: dockerBridgeAddrs})
		require.Error(t, err, out)
		assert.Contains(t, out, "interface br-0123456789ab holds fc00:f853:ccd:e793::/64")
		assert.Contains(t, out, "sudo ip link delete br-0123456789ab")
		assert.Contains(t, out, "1 problem(s)")
		assert.NotContains(t, out, "wlan0")
	})

	t.Run("the bridge of nerdctl's own kind network holding the subnet is fine", func(t *testing.T) {
		out, err := run(t, host{ipv6Addrs: dockerBridgeAddrs, kindNetwork: true})
		require.NoError(t, err, out)
		assert.NotContains(t, out, "holds fc00")
	})

	t.Run("an OCI worker is a warning, not a failure", func(t *testing.T) {
		out, err := run(t, host{ociWorker: true})
		require.NoError(t, err, out)
		assert.Contains(t, out, "buildkitd is not running a containerd worker")
	})
}

func TestCtrdShim(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the shim runs ctrd directly as root; this covers the sudo path")
	}
	dir := filepath.Join(shimDir(t), "ctrd")

	for _, name := range []string{"ctrd", "nerdctl"} {
		t.Run(name+" runs ctrd through sudo -n with its settings", func(t *testing.T) {
			bin := t.TempDir()
			sudoLog := filepath.Join(t.TempDir(), "sudo")
			fakeSudo(t, bin, sudoLog)
			engineBin := fakeBin(t, bin, "bin-ctrd", `echo "ctrd $*"`+"\n")
			cmd := exec.CommandContext(t.Context(), filepath.Join(dir, name), "ps", "-a")
			cmd.Env = baseEnv("PATH="+bin+":/usr/bin:/bin", "CTRD_REAL="+engineBin, "CTRD_DNS=1.1.1.1,8.8.8.8", "CNI_PATH=/opt/cni/bin", "CTRD_ROOT=/var/lib/ctrd", "UNRELATED=1")
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
			assert.Equal(t, "ctrd ps -a\n", string(out))
			got, err := os.ReadFile(sudoLog)
			require.NoError(t, err)
			assert.Equal(t, "-n --preserve-env=CTRD_ROOT,CTRD_DNS,CNI_PATH "+engineBin+" ps -a\n", string(got))
		})
	}

	t.Run("asks sudo to preserve nothing when nothing is set", func(t *testing.T) {
		bin := t.TempDir()
		sudoLog := filepath.Join(t.TempDir(), "sudo")
		fakeSudo(t, bin, sudoLog)
		engineBin := fakeBin(t, bin, "bin-ctrd", "true\n")
		cmd := exec.CommandContext(t.Context(), filepath.Join(dir, "ctrd"), "info")
		cmd.Env = baseEnv("PATH="+bin+":/usr/bin:/bin", "CTRD_REAL="+engineBin)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		got, err := os.ReadFile(sudoLog)
		require.NoError(t, err)
		assert.Equal(t, "-n "+engineBin+" info\n", string(got))
	})

	t.Run("passes the exit status and standard input through", func(t *testing.T) {
		bin := t.TempDir()
		fakeSudo(t, bin, filepath.Join(t.TempDir(), "sudo"))
		engineBin := fakeBin(t, bin, "bin-ctrd", "cat; exit 7\n")
		cmd := exec.CommandContext(t.Context(), filepath.Join(dir, "nerdctl"), "exec", "-i", "c")
		cmd.Env = baseEnv("PATH="+bin+":/usr/bin:/bin", "CTRD_REAL="+engineBin)
		cmd.Stdin = strings.NewReader("payload")
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		assert.Equal(t, 7, exitErr.ExitCode())
		assert.Equal(t, "payload", string(out))
	})

	t.Run("a ctrd that is not built is an error that says what to run", func(t *testing.T) {
		cmd := exec.CommandContext(t.Context(), filepath.Join(dir, "ctrd"), "version")
		cmd.Env = baseEnv("PATH=/usr/bin:/bin", "CTRD_REAL=/nonexistent/ctrd")
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		assert.Equal(t, 127, exitErr.ExitCode())
		assert.Contains(t, string(out), "make -C controller ctrd")
	})

	t.Run("nerdctl in this directory is ctrd, not a nerdctl", func(t *testing.T) {
		target, err := os.Readlink(filepath.Join(dir, "nerdctl"))
		require.NoError(t, err)
		assert.Equal(t, "ctrd", target)
	})
}

func TestCheckCtrd(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("check-ctrd.sh skips the sudo probe as root; this covers the sudo path")
	}
	check, err := filepath.Abs("check.sh")
	require.NoError(t, err)

	type host struct {
		sudoFails      bool
		noCtrd         bool
		containerdDown bool
		missingPlugins []string
		noBuildctl     bool
		noBuildkitd    bool
		workerNS       string
		gpus           string
		noNvidiaCtk    bool
		cdiLacksGPU    bool
	}
	run := func(t *testing.T, h host) (string, error) {
		t.Helper()
		bin := t.TempDir()
		cni := t.TempDir()
		if h.sudoFails {
			fakeBin(t, bin, "sudo", "echo 'sudo: a password is required' >&2; exit 1\n")
		} else {
			fakeSudo(t, bin, filepath.Join(t.TempDir(), "sudo"))
		}
		engineBin := filepath.Join(bin, "bin-ctrd")
		if !h.noCtrd {
			version := `echo v2.4.2`
			if h.containerdDown {
				version = `echo 'ctr: connection refused' >&2; exit 1`
			}
			fakeBin(t, bin, "bin-ctrd", `case "$1" in version) `+version+` ;; esac`+"\n")
		}
		plugins := map[string]bool{"bridge": true, "portmap": true, "firewall": true, "host-local": true, "loopback": true}
		for _, p := range h.missingPlugins {
			delete(plugins, p)
		}
		for p := range plugins {
			fakeBin(t, cni, p, "true\n")
		}
		if !h.noBuildctl {
			namespace := h.workerNS
			if namespace == "" {
				namespace = "ctrd"
			}
			listening := "yes"
			if h.noBuildkitd {
				listening = "no"
			}
			fakeBin(t, bin, "buildctl", `if [ "`+listening+`" = no ]; then echo 'connection refused' >&2; exit 1; fi
echo '[{"labels":{"org.mobyproject.buildkit.worker.executor":"containerd","org.mobyproject.buildkit.worker.containerd.namespace":"`+namespace+`"}}]'
`)
		}
		for _, tool := range []string{"iptables", "ip", "mount", "umount", "cp"} {
			fakeBin(t, bin, tool, "true\n")
		}
		if h.gpus != "" && !h.noNvidiaCtk {
			list := "echo nvidia.com/gpu=all"
			if h.cdiLacksGPU {
				list = "echo vendor.example/other=x"
			}
			fakeBin(t, bin, "nvidia-ctk", list+"\n")
		}
		tools := toolsDir(t, "bash", "sh", "sed", "sort", "head", "stat", "dirname", "env", "true", "awk", "cat", "tr", "grep")
		env := baseEnv("PATH="+bin+":"+tools, "CONTAINER_CLI=ctrd", "CNI_PATH="+cni, "CTRD_REAL="+engineBin, "CTRD_DNS=1.1.1.1", "CTRD_ROOT="+t.TempDir())
		if h.gpus != "" {
			env = append(env, "NICO_CONTAINERS_NODE_GPUS="+h.gpus)
		}
		return runBash(t, env, `exec bash "$1"`, check)
	}
	// Whether the machine running the test has cgroup v2, and a disk for /tmp,
	// is not what these cases are about.
	onlyHostLimits := func(out string) bool {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "FAIL") && !strings.Contains(line, "cgroup v2") && !strings.Contains(line, "state directory") {
				return false
			}
		}
		return true
	}

	t.Run("a host that is ready passes the checks of the engine", func(t *testing.T) {
		out, _ := run(t, host{gpus: "all"})
		assert.True(t, onlyHostLimits(out), out)
		for _, want := range []string{"ok: containerd v2.4.2 answers", "ok: CNI plugins", "ok: buildkitd answers", "with a containerd worker in the 'ctrd' namespace", "ok: containers use the resolvers in CTRD_DNS: 1.1.1.1", "ok: CDI specification names the GPUs"} {
			assert.Contains(t, out, want)
		}
	})

	failures := map[string]struct {
		host host
		want []string
	}{
		"sudo needs a password":   {host{sudoFails: true}, []string{"passwordless sudo is required"}},
		"ctrd is not built":       {host{noCtrd: true}, []string{"is not built. Run: make -C controller ctrd"}},
		"containerd is down":      {host{containerdDown: true}, []string{"ctrd cannot reach containerd", "connection refused"}},
		"CNI plugins are missing": {host{missingPlugins: []string{"firewall", "bridge"}}, []string{"CNI plugins missing", "firewall", "bridge"}},
		"buildctl is missing":     {host{noBuildctl: true}, []string{"buildctl is not on root's PATH"}},
		"buildkitd is down":       {host{noBuildkitd: true}, []string{"buildkitd does not answer"}},
		"buildkitd builds into another namespace": {
			host{workerNS: "default"}, []string{"no containerd worker in the 'ctrd' namespace", "--containerd-worker-namespace ctrd"},
		},
		"GPUs asked for and nvidia-ctk missing":      {host{gpus: "all", noNvidiaCtk: true}, []string{"nvidia-ctk is not installed"}},
		"GPUs asked for and a CDI spec without them": {host{gpus: "all", cdiLacksGPU: true}, []string{"the CDI specification names no GPU", "nvidia-ctk cdi generate"}},
	}
	for name, tt := range failures {
		t.Run(name, func(t *testing.T) {
			out, err := run(t, tt.host)
			require.Error(t, err, out)
			for _, want := range tt.want {
				assert.Contains(t, out, want)
			}
		})
	}

	t.Run("GPUs are not checked unless nico-sim is asked for them", func(t *testing.T) {
		out, _ := run(t, host{})
		assert.NotContains(t, out, "CDI")
		assert.NotContains(t, out, "nvidia-ctk")
	})
}
