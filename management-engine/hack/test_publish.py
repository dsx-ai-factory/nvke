#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Check publication boundaries and immutable-tag failures without registry access."""
import json
import os
import shutil
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('publish.sh')
DIGEST = 'sha256:' + 'b' * 64


class Publication(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.log = self.directory / 'calls'
        self.repo = self.directory / 'repo'
        self.script = self.repo / 'management-engine/hack/publish.sh'
        helper = self.repo / 'release/hack/describe-version.sh'
        self.script.parent.mkdir(parents=True)
        helper.parent.mkdir(parents=True)
        shutil.copy2(SCRIPT, self.script)
        shutil.copy2(SCRIPT.parents[2] / 'release/hack/describe-version.sh', helper)
        self.git('init', '-q')
        self.git('add', '.')
        self.git('commit', '-qm', 'fixture')
        self.revision = self.git('rev-parse', 'HEAD').stdout.strip()
        self.git('tag', 'v0.3.0')
        crane = self.directory / 'crane'
        crane.write_text('''#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> "$CALLS"
case "$1" in
  ls) test "${LOOKUP_FAIL:-false}" != true; printf '%s\\n' "${EXISTING_TAG:-}" ;;
  digest) printf '%s\\n' "$EXISTING_DIGEST" ;;
  tag) exit 0 ;;
  *) echo "unexpected registry operation" >&2; exit 1 ;;
esac
''')
        crane.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.directory) + os.pathsep + os.environ['PATH'],
                        GITHUB_REF='refs/tags/v0.3.0', GITHUB_SHA=self.revision,
                        RUNTIME='manager', NVKE_NGC_ORG='example', ARTIFACT_DIR=str(self.directory),
                        CALLS=str(self.log), EXISTING_TAG='v0.3.0', EXISTING_DIGEST=DIGEST)
        (self.directory / 'manager.json').write_text(json.dumps({'digest': DIGEST}))

    def git(self, *args):
        return subprocess.run(['git', '-C', str(self.repo), '-c', 'commit.gpgsign=false',
                               '-c', 'tag.gpgsign=false', '-c', 'user.name=Fixture',
                               '-c', 'user.email=fixture@example.invalid', *args],
                              text=True, capture_output=True, check=True)

    def run_publisher(self, operation):
        return subprocess.run(['bash', str(self.script), operation], env=self.env, text=True,
                              capture_output=True, check=False)

    def test_version_and_untrusted_ref_boundary(self):
        for ref, expected in [
            ('refs/heads/main', 'v0.3.0'),
            ('refs/tags/v0.3.0', 'v0.3.0'),
        ]:
            self.env['GITHUB_REF'] = ref
            result = self.run_publisher('version')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), expected)
        for ref in ['refs/heads/feature', 'refs/heads/pull-request/1',
                    'refs/tags/controller/v0.3.0', 'refs/tags/management-engine/v0.3.0',
                    'refs/tags/v0.3.0-rc1', 'refs/tags/v0.3.1']:
            self.env['GITHUB_REF'] = ref
            self.assertNotEqual(self.run_publisher('tag').returncode, 0)
        self.assertFalse(self.log.exists(), 'untrusted refs reached the registry')

    def test_main_uses_shared_describe_version(self):
        self.git('commit', '--allow-empty', '-qm', 'next commit')
        self.env.update(GITHUB_REF='refs/heads/main',
                        GITHUB_SHA=self.git('rev-parse', 'HEAD').stdout.strip())
        expected = self.git('describe', '--tags', '--always', '--match', 'v*').stdout.strip()
        result = self.run_publisher('version')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), expected)
        self.assertTrue(expected.startswith('v0.3.0-1-g'))

    def test_beta_uses_its_own_tag_on_a_multiply_tagged_commit(self):
        self.git('tag', 'v0.4.0-beta.1')
        self.env['GITHUB_REF'] = 'refs/tags/v0.4.0-beta.1'
        result = self.run_publisher('version')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'v0.4.0-beta.1')

    def test_release_tag_must_name_the_built_commit(self):
        self.git('commit', '--allow-empty', '-qm', 'next commit')
        self.env['GITHUB_SHA'] = self.git('rev-parse', 'HEAD').stdout.strip()
        self.assertNotEqual(self.run_publisher('tag').returncode, 0)
        self.assertFalse(self.log.exists())

    def test_identical_version_is_reusable_without_writes(self):
        result = self.run_publisher('tag')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('tag ', self.log.read_text())

    def test_conflicting_version_is_not_overwritten(self):
        self.env['EXISTING_DIGEST'] = 'sha256:' + 'c' * 64
        result = self.run_publisher('tag')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('refusing to replace', result.stderr)
        self.assertNotIn('tag ', self.log.read_text())

    def test_lookup_failure_does_not_authorize_a_push(self):
        self.env['LOOKUP_FAIL'] = 'true'
        result = self.run_publisher('tag')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.log.read_text().splitlines()), 1)

    def test_corrupt_receipt_does_not_reach_the_registry(self):
        (self.directory / 'manager.json').write_text(json.dumps({'digest': 'latest'}))
        self.assertNotEqual(self.run_publisher('tag').returncode, 0)
        self.assertFalse(self.log.exists())


if __name__ == '__main__':
    unittest.main()
