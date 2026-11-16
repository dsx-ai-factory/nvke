#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Offline checks for preservation and cleanup guards. Created with AI assistance."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import mixed
import replay


class GuardTests(unittest.TestCase):
    def test_prepare_preserves_existing_work(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp) / 'existing'
            work.mkdir()
            (work / 'human-work').write_text('keep this')
            with patch.object(replay, 'call') as command:
                with self.assertRaisesRegex(RuntimeError, 'new directory'):
                    replay.prepare(work, 'unused')
            command.assert_not_called()
            self.assertEqual((work / 'human-work').read_text(), 'keep this')

    def test_bad_patch_checksum_stops_before_git_or_directory_creation(self):
        with tempfile.TemporaryDirectory() as tmp:
            package = Path(tmp) / 'package'
            package.mkdir()
            (package / 'change.patch').write_text('corrupted')
            (package / 'versions.json').write_text(json.dumps({'patches': {'change.patch': 'wrong'}}))
            work = Path(tmp) / 'new'
            with patch.object(replay, 'PACKAGE', package), patch.object(replay, 'call') as command:
                with self.assertRaisesRegex(RuntimeError, 'checksum'):
                    replay.prepare(work, 'unused')
            command.assert_not_called()
            self.assertFalse(work.exists())

    def test_empty_pre_controller_cluster_can_be_cleaned_without_tenant_api(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            (work / 'private').mkdir()
            (work / 'private/management.kubeconfig').touch()
            config = {'kind': 'nvke-replay-12345678', 'mirror': 'nvke-replay-bundles-12345678',
                      'bgp': 'nvke-replay-bgp-12345678', 'volume': 'nvke-replay-bundles-12345678'}
            def command(args, **kwargs):
                return config['kind'] if args == ['kind', 'get', 'clusters'] else ''
            with patch.object(replay, 'call', side_effect=command) as calls:
                replay.cleanup(work, config, {})
            commands = [c.args[0] for c in calls.call_args_list]
            self.assertIn(['kind', 'delete', 'cluster', '--name', config['kind']], commands)
            self.assertFalse(any('mixed.py' in ' '.join(c) for c in commands))
            self.assertTrue(json.loads((work / 'cleanup.json').read_text())['managementClusterRemoved'])

    def test_cleanup_rejects_unrecognised_tenant_before_any_api_call(self):
        client = object.__new__(mixed.Client)
        with patch.object(client, 'api') as api, patch.object(client, 'nodes') as nodes:
            with self.assertRaisesRegex(RuntimeError, 'refusing cleanup'):
                client.delete({'name': 'another-engineers-cluster', 'id': 'unknown'})
        api.assert_not_called()
        nodes.assert_not_called()


if __name__ == '__main__':
    unittest.main()
