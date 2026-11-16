#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Check publication boundaries and immutable-tag failures without registry access."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('publish.sh')
REVISION = 'a' * 40
DIGEST = 'sha256:' + 'b' * 64


class Publication(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.log = self.directory / 'calls'
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
                        GITHUB_REF='refs/tags/management-engine/v0.3.0-rc.1', GITHUB_SHA=REVISION,
                        RUNTIME='manager', NVKE_NGC_ORG='example', ARTIFACT_DIR=str(self.directory),
                        CALLS=str(self.log), EXISTING_TAG='v0.3.0-rc.1', EXISTING_DIGEST=DIGEST)
        (self.directory / 'manager.json').write_text(json.dumps({'digest': DIGEST}))

    def run_publisher(self, operation):
        return subprocess.run(['bash', str(SCRIPT), operation], env=self.env, text=True,
                              capture_output=True, check=False)

    def test_version_and_untrusted_ref_boundary(self):
        for ref, expected in [
            ('refs/heads/main', 'v0.0.0-sha.g' + REVISION),
            ('refs/tags/management-engine/v0.3.0-rc.1', 'v0.3.0-rc.1'),
            ('refs/tags/management-engine/v0.3.0', 'v0.3.0'),
        ]:
            self.env['GITHUB_REF'] = ref
            result = self.run_publisher('version')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), expected)
        for ref in ['refs/heads/feature', 'refs/heads/pull-request/1',
                    'refs/tags/controller/v0.3.0', 'refs/tags/management-engine/v0.3.0-rc1', 'refs/tags/management-engine/v00.3.0']:
            self.env['GITHUB_REF'] = ref
            self.assertNotEqual(self.run_publisher('tag').returncode, 0)
        self.assertFalse(self.log.exists(), 'untrusted refs reached the registry')

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
