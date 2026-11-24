// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The engine is tested against fakes on PATH, as the shell library is: a ctr
// that keeps its tasks and images in files, the CNI plugins, ip, buildctl and the
// supervisor. Every call they receive is appended to one log.

// fakeCtr answers the ctr subcommands the engine uses. $FAKE is its directory.
const fakeCtr = `#!/bin/sh
echo "ctr $*" >> "$FAKE/calls"
shift 4
case "$1 $2" in
  "version "*) printf 'Client:\n  Version:  v2.4.2\n\nServer:\n  Version:  v2.4.2\n  Revision: abc\n' ;;
  "tasks list") echo "TASK    PID    STATUS"; cat "$FAKE/tasks" 2>/dev/null || true ;;
  "tasks kill")
    id="$5"
    echo 137 > "$FAKE_ROOT/containers/$id/exitcode"
    grep -v "^$id " "$FAKE/tasks" > "$FAKE/tasks.new"; mv "$FAKE/tasks.new" "$FAKE/tasks" ;;
  "tasks exec") if [ "$3" = --exec-id ]; then cat >/dev/null <&0 || true; echo "exec output"; exit "${EXEC_STATUS:-0}"; fi ;;
  "tasks delete") exit 0 ;;
  "tasks start") echo "console line"; echo "second line"; exit "${START_STATUS:-3}" ;;
  "images list") echo "REF    TYPE    DIGEST    SIZE    PLATFORMS    LABELS"; cat "$FAKE/images" 2>/dev/null || true ;;
  "images pull") echo "$5 application/vnd.oci.image.index.v1+json sha256:aaa 1.0_MiB linux/arm64 -" >> "$FAKE/images" ;;
  "images export") out="$5"; cp "$FAKE/export.tar" "$out" ;;
  "images import") exit 0 ;;
  "containers info") echo '{"ID":"x","Labels":{"io.containerd.image.config.stop-signal":"SIGRTMIN+3"}}' ;;
  "containers create"|"containers delete") exit 0 ;;
  "snapshots mounts") mkdir -p "$3/etc/frr" "$3/var/lib/dpkg"; echo status > "$3/var/lib/dpkg/status"; echo true ;;
  *) echo "fake ctr: unexpected $*" >&2; exit 9 ;;
esac
`

// fakeCNI is every CNI plugin: it records what it was given and answers an ADD.
const fakeCNI = `#!/bin/sh
name=$(basename "$0")
echo "$name $CNI_COMMAND id=$CNI_CONTAINERID ifname=$CNI_IFNAME netns=$CNI_NETNS" >> "$FAKE/calls"
cat > "$FAKE/cni-$name-$CNI_COMMAND.json"
if [ "$CNI_COMMAND" = ADD ]; then
  echo '{"cniVersion":"1.0.0","interfaces":[{"name":"eth0","mac":"aa:bb:cc:dd:ee:ff","sandbox":"/run/netns/x"}],"ips":[{"address":"10.4.0.5/24"},{"address":"fc00:4::5/64"}]}'
fi
`

type harness struct {
	t      *testing.T
	eng    *engine
	dir    string
	root   string
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	cni := filepath.Join(dir, "cni")
	root := filepath.Join(dir, "root")
	for _, d := range []string{bin, cni, root, filepath.Join(dir, "fake")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	writeScript(t, filepath.Join(bin, "ctr"), fakeCtr)
	writeScript(t, filepath.Join(bin, "umount"), "#!/bin/sh\necho \"umount $*\" >> \"$FAKE/calls\"\nmkdir -p \"$FAKE/mounted\"\ncp -a \"$1/.\" \"$FAKE/mounted/\"\n")
	for _, name := range []string{"ip", "buildctl"} {
		writeScript(t, filepath.Join(bin, name), "#!/bin/sh\necho \""+name+" $*\" >> \"$FAKE/calls\"\n")
	}
	for _, name := range []string{"bridge", "portmap", "firewall", "loopback"} {
		writeScript(t, filepath.Join(cni, name), fakeCNI)
	}
	supervisor := filepath.Join(dir, "supervisor")
	writeScript(t, supervisor, "#!/bin/sh\necho \"supervisor $*\" >> \"$FAKE/calls\"\necho \"$2 4242 RUNNING\" >> \"$FAKE/tasks\"\n")

	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE", filepath.Join(dir, "fake"))
	t.Setenv("FAKE_ROOT", root)
	resolv := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(resolv, []byte("nameserver 192.0.2.53\n"), 0o644))

	h := &harness{t: t, dir: dir, root: root, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	h.eng = &engine{
		ctx: t.Context(),
		cfg: settings{
			Root: root, Namespace: "ns", Address: "/run/test.sock", CNIDirs: []string{cni}, BuildkitHost: "unix:///run/bk.sock",
			ResolvConfs: []string{resolv}, SupervisorCgroups: filepath.Join(dir, "cgroups"),
		},
		stdin: strings.NewReader(""), stdout: h.stdout, stderr: h.stderr, exe: supervisor,
	}
	return h
}

// run executes one command line and returns its status, with the output of the run so far.
func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	return h.eng.run(args)
}

// calls is every call the fakes received so far.
func (h *harness) calls() []string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.dir, "fake", "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(h.t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (h *harness) setFile(name, content string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(filepath.Join(h.dir, "fake", name), []byte(content), 0o644))
}

func (h *harness) readFile(name string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.dir, "fake", name))
	require.NoError(h.t, err)
	return string(data)
}
