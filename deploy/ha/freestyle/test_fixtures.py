import base64
import configparser
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
listener = module('listener', 'verify-listener.py')


class Fixtures(unittest.TestCase):
    def test_nfs_listener_accepts_mapped_ipv4_and_rejects_wildcard_or_unexpected_binds(self):
        ip = '10.77.33.20'
        ipv4 = 'LISTEN 0 4096 10.77.33.20:2049 0.0.0.0:*\n'
        mapped = 'LISTEN 0 4096 [::ffff:10.77.33.20]:2049 *:*\n'
        self.assertTrue(listener.listening_only_at(ipv4, ip))
        self.assertTrue(listener.listening_only_at(mapped, ip))
        for text in ['', 'LISTEN 0 4096 0.0.0.0:2049 *:*',
                     'LISTEN 0 4096 [::]:2049 *:*', 'LISTEN 0 4096 *:2049 *:*',
                     'LISTEN 0 4096 10.77.33.21:2049 *:*',
                     mapped + 'LISTEN 0 4096 0.0.0.0:2049 *:*\n']:
            self.assertFalse(listener.listening_only_at(text, ip))
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
        # Seed the Viper key so AutomaticEnv includes AI_AGENT_LLM_API_KEY when
        # unmarshalling this minimal fixture configuration.
        self.assertIn('api_key', config['llm'])
        self.assertEqual(config['llm']['api_key'], '')
        unit = configparser.ConfigParser()
        unit.read(out / 'node-a/ha-fixture.conf')
        cwd = Path(unit['Service']['WorkingDirectory'])
        self.assertTrue(Path(config['api']['tenants']['ha_test']['workspace_root']).is_relative_to(cwd))
        self.assertEqual(unit['Service']['BindReadOnlyPaths'],
                         '/opt/ai-agent-ha/runtime/teams.yaml:/opt/ai-agent/teams.yaml')
        # The fixture working directory is read-only under ProtectSystem=strict.
        self.assertEqual(config['log']['directory'], '/opt/ai-agent/logs')
        self.assertTrue(config['log']['file_enabled'])
        self.assertTrue(config['log']['access_enabled'])
        self.assertTrue(config['telemetry']['enabled'])
        self.assertEqual(config['telemetry']['endpoint'], '127.0.0.1:4318')
        monitor = json.loads((out / 'control/prometheus.json').read_text())['scrape_configs'][0]
        self.assertEqual(monitor['metrics_path'], '/metrics')
        self.assertEqual(monitor['static_configs'][0]['targets'], [cfg['nodes']['node-a'] + ':9464'])
        self.assertNotIn('http_headers', monitor)
        collector = json.loads((out / 'node-a/otel-collector.json').read_text())
        self.assertEqual(collector['service']['pipelines']['metrics']['exporters'], ['prometheus'])
        compose = json.loads((out / 'node-a/collector-compose.json').read_text())['services']['otel-collector']
        self.assertEqual(compose['ports'], ['127.0.0.1:4318:4318', cfg['nodes']['node-a'] + ':9464:9464'])
        teams = json.loads((out / 'node-a/teams.yaml').read_text())
        self.assertEqual(teams['teams']['software']['planner']['tools'], ['read_file'])
        for file in out.rglob('*'):
            self.assertEqual(file.stat().st_mode & 0o077, 0, file)
        self.assertIn('/15', (out / 'control/control.env').read_text())
        self.assertIn('127.0.0.1:9090', (out / 'control/compose.yaml').read_text())
        nfs = (out / 'storage/ganesha.conf').read_text()
        self.assertIn('Bind_addr = ' + cfg['nodes']['storage'] + ';', nfs)
        self.assertIn('Access_Type = None;', nfs)
        self.assertIn('Protocols = 4;', nfs)
        self.assertIn('Clients = ' + ', '.join(cfg['nodes'][role] for role in ('node-a', 'node-b', 'control')) + ';', nfs)
        self.assertEqual(nfs.count('Squash = root_squash;'), 2)
        self.assertNotIn('*', nfs)
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
            ('prepare-telemetry.sh', ['--assets', None]),
        ]
        root = Path(tempfile.mkdtemp(prefix='ai-agent-freestyle-plan-test-'))
        (root / 'bootstrap.env').write_text('HA_ROLE=node-a\nHA_PREFIX=offline-plan\nHA_NODE_A_IP=10.77.33.11\nHA_NODE_B_IP=10.77.33.12\nHA_STORAGE_IP=10.77.33.20\nHA_CONTROL_IP=10.77.33.30\n')
        for file, args in cases:
            args = [str(root) if a is None else a for a in args]
            result = subprocess.run(['bash', str(KIT / file), *args], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('Plan only', result.stdout)

    def test_nfs_helper_is_plan_only_without_apply_and_requires_storage_role(self):
        root = Path(tempfile.mkdtemp(prefix='ai-agent-freestyle-nfs-plan-test-'))
        file = root / 'bootstrap.env'
        file.write_text('HA_ROLE=storage\nHA_PREFIX=offline-plan\n')
        args = ['bash', str(KIT / 'prepare-nfs.sh'), '--assets', str(root)]
        planned = subprocess.run(args, capture_output=True, text=True)
        self.assertEqual(planned.returncode, 0, planned.stderr)
        self.assertIn('Plan only', planned.stdout)
        file.write_text('HA_ROLE=node-a\nHA_PREFIX=offline-plan\n')
        self.assertNotEqual(subprocess.run(args, capture_output=True).returncode, 0)


if __name__ == '__main__':
    unittest.main()
