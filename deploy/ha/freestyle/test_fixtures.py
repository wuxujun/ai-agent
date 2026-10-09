import base64
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

KIT = Path(__file__).resolve().parent


def module(name, file):
    spec = importlib.util.spec_from_file_location(name, KIT / file)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


renderer = module('renderer', 'render.py')
contracts = module('contracts', 'verify-contracts.py')
stub = module('stub', 'offline_stub.py')


class Fixtures(unittest.TestCase):
    def test_contract_summary_rejects_missing_failed_and_skipped_subtests(self):
        events = [{'Test': name, 'Action': 'pass'} for name in contracts.EXPECTED]
        self.assertTrue(contracts.verify(events, 0)['passed'])
        self.assertFalse(contracts.verify(events[:-1], 0)['passed'])
        self.assertFalse(contracts.verify(events + [{'Test': 'TestPostgresPoolExternal/capacity', 'Action': 'skip'}], 0)['passed'])
        self.assertFalse(contracts.verify(events + [{'Action': 'fail'}], 0)['passed'])
        self.assertFalse(contracts.verify(events, 1)['passed'])

    def test_rendered_assets_are_private_and_initial_runtime_is_zero(self):
        cfg = renderer.topology(KIT / 'topology.example.json')
        secret = {k: str(i) * 64 for i, k in enumerate(('api_admin_key', 'api_tenant_key', 'postgres_password', 'redis_password', 'stub_token'), 1)}
        secret['approval_key'] = base64.b64encode(b'x' * 32).decode()
        # Retain files: this repository forbids bulk cleanup, including test helpers.
        out = Path(tempfile.mkdtemp(prefix='ai-agent-freestyle-test-')) / 'assets'
        renderer.render(cfg, secret, out)
        a, b = [(out / role / 'ha-config.json').read_bytes() for role in ('node-a', 'node-b')]
        self.assertEqual(a, b)
        config = json.loads(a)
        self.assertEqual(config['multiagent']['runtime'], 'legacy')
        self.assertEqual(config['multiagent']['dag_canary_percent'], 0)
        self.assertEqual(config['api']['tenants']['ha_test']['admin'], False)
        teams = json.loads((out / 'node-a/teams.yaml').read_text())
        self.assertEqual(teams['teams']['software']['planner']['tools'], ['read_file'])
        for file in out.rglob('*'):
            self.assertEqual(file.stat().st_mode & 0o077, 0, file)
        self.assertIn('/15', (out / 'control/control.env').read_text())
        self.assertIn('127.0.0.1:9090', (out / 'control/compose.yaml').read_text())
        with self.assertRaises(ValueError):
            renderer.new_path(out)
        with self.assertRaises(ValueError):
            renderer.new_path(KIT / 'private-output')

    def test_stub_refuses_unrestricted_tools_or_unknown_schemas(self):
        step = {'type': 'object', 'properties': {'action': {'type': 'string', 'enum': ['read_file']},
            'file_path': {'type': 'string'}, 'graph_depth': {'type': 'integer'}}}
        schema = {'properties': {'thought_summary': {'type': 'string'}, 'steps': {'items': step}}}
        self.assertEqual(stub.response_for(schema)['steps'][0]['file_path'], 'README.md')
        step['properties']['action']['enum'].append('execute_code')
        with self.assertRaises(ValueError):
            stub.response_for(schema)
        with self.assertRaises(ValueError):
            stub.response_for({'properties': {'supported': {'type': 'boolean'}}})

    def test_shell_entrypoints_default_to_plan_on_non_linux_host(self):
        cases = [
            ('bootstrap.sh', ['--role', 'node-a', '--assets', None]),
            ('deploy-agent.sh', ['--bundle', '/does-not-exist']),
            ('run-control.sh', ['--mode', 'soak', '--bundle', '/does-not-exist', '--run', 'offline-plan']),
            ('fault-node.sh', ['--action', 'kill', '--task-id', 'fixture-1', '--output', '/does-not-exist/result.json']),
        ]
        root = Path(tempfile.mkdtemp(prefix='ai-agent-freestyle-plan-test-'))
        (root / 'bootstrap.env').write_text('HA_ROLE=node-a\nHA_PREFIX=offline-plan\nHA_NODE_A_IP=10.77.33.11\nHA_NODE_B_IP=10.77.33.12\nHA_STORAGE_IP=10.77.33.20\nHA_CONTROL_IP=10.77.33.30\n')
        for file, args in cases:
            args = [str(root) if a is None else a for a in args]
            result = subprocess.run(['bash', str(KIT / file), *args], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('Plan only', result.stdout)


if __name__ == '__main__':
    unittest.main()
