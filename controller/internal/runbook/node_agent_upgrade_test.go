// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentUpgradePruningIgnoresCorruptAndMismatchedRecords(t *testing.T) {
	script, err := Script("", "node-agent-upgrade")
	require.NoError(t, err)
	_, python, found := strings.Cut(script, "<<'PY'\n")
	require.True(t, found)
	definitions, _, found := strings.Cut(python, "if len(sys.argv) == 3")
	require.True(t, found)
	// Exercise the runbook's actual record reader and retention policy without
	// requiring systemd or modifying the host's upgrade directory.
	checks := `
import tempfile
with tempfile.TemporaryDirectory() as temporary:
    root = Path(temporary)
    def record(folder, version, phase, date):
        directory = root / folder
        directory.mkdir()
        (directory / 'operation.json').write_text(json.dumps(dict(
            targetVersion=version, method='bundle',
            phase=phase, createdAt=date)))
    record('v0.1.1_bundle', 'v0.1.1', 'Succeeded', '2026-01-01')
    record('v0.1.2_bundle', 'v0.1.2', 'Succeeded', '2026-01-02')
    record('v0.1.3_bundle', 'v0.1.3', 'FailedRestored', '2026-01-03')
    record('v0.1.4_bundle', 'v0.1.4', 'Running', '2026-01-04')
    record('mismatched', 'v0.1.2', 'Succeeded', '2026-01-05')
    record('v0.1.5_bundle', 'v0.1.6', 'Succeeded', '2026-01-06')
    corrupt = root / 'v0.1.7_bundle'
    corrupt.mkdir()
    (corrupt / 'operation.json').write_text('{truncated')
    records = read_operations(root)
    assert len(records) == 4, records
    prune_operations(records)
    assert not (root / 'v0.1.1_bundle').exists()
    for name in ('v0.1.2_bundle', 'v0.1.3_bundle', 'v0.1.4_bundle',
                 'mismatched', 'v0.1.5_bundle', 'v0.1.7_bundle'):
        assert (root / name).exists(), name
`
	command := exec.CommandContext(t.Context(), "python3", "-c", definitions+checks)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestAgentUpgradeBootstrapAcrossPackageRename(t *testing.T) {
	script, err := Script("", "node-agent-upgrade")
	require.NoError(t, err)
	_, python, found := strings.Cut(script, "<<'PY'\n")
	require.True(t, found)
	definitions, _, found := strings.Cut(python, "if len(sys.argv) == 3")
	require.True(t, found)
	checks := `
import tempfile
from unittest.mock import patch
from pathlib import Path as RealPath
for previous_name, target_name in [('nke-agent', 'nvke-agent'), ('nvke-agent', 'nke-agent')]:
    for fail_target in (False, True):
        with tempfile.TemporaryDirectory() as temporary:
            root = RealPath(temporary)
            def mapped_path(value):
                value = str(value)
                if value.startswith('/') and not value.startswith(temporary + '/'):
                    return root / value.lstrip('/')
                return RealPath(value)
            namespace = {}
            source = DEFINITIONS.replace('from pathlib import Path', 'Path = mapped_path')
            namespace['mapped_path'] = mapped_path
            state_dir = mapped_path('/var/lib/' + ('nke' if previous_name == 'nke-agent' else 'nvke'))
            state_dir.mkdir(parents=True)
            (state_dir / 'state.json').write_text('original state')
            previous_binary = mapped_path('/usr/local/bin/' + previous_name)
            previous_binary.parent.mkdir(parents=True)
            previous_binary.write_text('NVKE Agent Version: v0.1.1')
            previous_unit = mapped_path('/etc/systemd/system/' + previous_name + '.service')
            previous_unit.parent.mkdir(parents=True)
            previous_unit.write_text('original unit')
            exec(source, namespace)
            store = namespace['ROOT']
            store.mkdir(parents=True)
            op_dir = store / 'v0.1.2_bundle'
            op_dir.mkdir()
            op_path = op_dir / 'operation.json'
            op = dict(method='bundle', targetVersion='v0.1.2',
                      previousVersion='v0.1.1', phase='Pending', bundleSource=[], createdAt=namespace['now']())
            namespace['save'](op_path, op)
            fixture_dir = root / 'fixture'
            fixture_dir.mkdir()
            version = 'v9.9.9' if fail_target else 'v0.1.2'
            (fixture_dir / target_name).write_text('NVKE Agent Version: ' + version)
            (fixture_dir / (target_name + '.service')).write_text('target unit')
            if target_name == 'nvke-agent':
                (fixture_dir / 'migrate-agent-paths.sh').write_text('migration fixture')
            archive = root / 'fixture.tgz'
            with namespace['tarfile'].open(archive, 'w:gz') as bundle:
                for file in fixture_dir.iterdir():
                    bundle.add(file, arcname=file.name)
            commands = []
            def fake_run(*args, cwd=None):
                commands.append(args)
                if args[0] == 'systemctl':
                    return ''
                if args[0] == 'install':
                    namespace['shutil'].copyfile(args[-2], mapped_path(args[-1]))
                    return ''
                if args[0] == 'bash':
                    legacy = mapped_path('/var/lib/nke')
                    renamed = mapped_path('/var/lib/nvke')
                    if legacy.is_dir() and not legacy.is_symlink():
                        assert not renamed.exists(), 'bootstrap created a conflicting state tree'
                        legacy.rename(renamed)
                        legacy.symlink_to(renamed)
                    return ''
                if args[-1] == '--version':
                    return mapped_path(args[0]).read_text()
                raise AssertionError(args)
            namespace['run'] = fake_run
            namespace['bundle_url'] = lambda version, source: archive.as_uri()
            with patch.object(namespace['time'], 'sleep', lambda seconds: None):
                namespace['execute'](op_path)
            result = namespace['json'].loads(op_path.read_text())
            assert result['phase'] == ('FailedRestored' if fail_target else 'Succeeded'), result
            expected_name = previous_name if fail_target else target_name
            expected_version = 'v0.1.1' if fail_target else 'v0.1.2'
            assert mapped_path('/usr/local/bin/' + expected_name).read_text().endswith(expected_version)
            assert ('systemctl', 'restart', expected_name + '.service') in commands
            assert (state_dir / 'state.json').read_text() == 'original state'
`
	command := exec.CommandContext(t.Context(), "python3", "-c", "DEFINITIONS = "+fmt.Sprintf("%q", definitions)+"\n"+checks)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
