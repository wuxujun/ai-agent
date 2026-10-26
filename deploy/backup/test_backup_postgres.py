import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
import uuid
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch

SCRIPT = Path(__file__).with_name('backup-postgres.py')
spec = importlib.util.spec_from_file_location('backup_postgres', SCRIPT)
backup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(backup)
SECRET = 'backup-test-password'
DSN = f'postgresql://fixture:{SECRET}@127.0.0.1:55499/fixture?sslmode=disable'


class BackupTests(unittest.TestCase):
    def setUp(self):
        # Retain each explicit test directory; no recursive cleanup of backup evidence.
        self.root = Path(tempfile.mkdtemp(prefix='ai-agent-backup-test-'))
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.out = self.root / 'backups'
        self.env = {'PATH': str(self.bin) + os.pathsep + os.environ.get('PATH', ''),
                    'AI_AGENT_STORE_TYPE': 'postgres', 'AI_AGENT_STORE_DSN': DSN}

    def fake(self, name, body):
        path = self.bin / name
        path.write_text('#!' + sys.executable + '\n' + body)
        path.chmod(0o700)

    def clients(self, mode='ok'):
        body = """import os,sys,time
assert not any('backup-test-password' in arg for arg in sys.argv)
assert os.environ['PGDATABASE']=='fixture'
assert os.environ['PGPASSWORD']=='backup-test-password'
assert 'OPENAI_API_KEY' not in os.environ
"""
        if mode == 'error':
            body += "sys.stdout.buffer.write(b'PGDMPpartial'); sys.stderr.write('backup-test-password'); sys.exit(3)\n"
        elif mode == 'warning':
            body += "sys.stdout.buffer.write(b'PGDMPfake'); sys.stderr.write('warning backup-test-password')\n"
        elif mode == 'timeout':
            body += "time.sleep(3)\n"
        elif mode == 'bad-header':
            body += "sys.stdout.buffer.write(b'not-a-dump')\n"
        else:
            body += "sys.stdout.buffer.write(b'PGDMPfake-custom-archive')\n"
        self.fake('pg_dump', body)
        self.fake('pg_restore', "import sys\nassert sys.argv[1]=='--list'\n"
                  + ("sys.stderr.write('backup-test-password');sys.exit(2)\n" if mode == 'bad-list' else ''))

    def invoke(self, *args):
        output, error = io.StringIO(), io.StringIO()
        # main() tightens umask, matching its normal separate-process execution.
        old_mask = os.umask(0o022)
        try:
            with patch.dict(os.environ, dict(self.env, OPENAI_API_KEY='unrelated-secret'), clear=True):
                with redirect_stdout(output), redirect_stderr(error):
                    code = backup.main(['--output-dir', str(self.out), '--pg-bin-dir', str(self.bin), *args])
        finally:
            os.umask(old_mask)
        text = output.getvalue() + error.getvalue()
        self.assertNotIn(SECRET, text)
        self.assertNotIn('unrelated-secret', text)
        return code, text

    def test_success_permissions_checksums_and_unique_runs(self):
        self.clients()
        for _ in range(2):
            self.assertEqual(self.invoke()[0], 0)
        roots = list(self.out.iterdir())
        self.assertEqual(len(roots), 2)
        for root in roots:
            self.assertFalse(root.name.endswith('.partial'))
            self.assertEqual(stat.S_IMODE(root.stat().st_mode), 0o700)
            manifest = json.loads((root / 'MANIFEST.json').read_text())
            self.assertTrue(manifest['archive_list_verified'])
            self.assertFalse(manifest['restore_verified'])
            for line in (root / 'SHA256SUMS').read_text().splitlines():
                digest, name = line.split('  ')
                self.assertEqual(backup.sha256(root / name), digest)
            for file in root.iterdir():
                self.assertEqual(stat.S_IMODE(file.stat().st_mode), 0o600)
                self.assertNotIn(SECRET.encode(), file.read_bytes())

    def test_failures_never_publish_complete_backup_or_diagnostics(self):
        for mode in ['error', 'warning', 'timeout', 'bad-header', 'bad-list']:
            with self.subTest(mode=mode):
                self.clients(mode)
                self.assertEqual(self.invoke('--timeout-seconds', '1')[0], 1)
        for root in self.out.iterdir():
            self.assertTrue(root.name.endswith('.partial'))
            self.assertEqual(json.loads((root / 'FAILED.json').read_text())['status'], 'failed')
            self.assertFalse((root / 'MANIFEST.json').exists())

    def test_dry_run_never_executes_clients_or_creates_output(self):
        self.clients('error')
        self.assertEqual(self.invoke('--dry-run')[0], 0)
        self.assertFalse(self.out.exists())

    def test_missing_tools_fail_before_output(self):
        self.assertEqual(self.invoke()[0], 1)
        self.assertFalse(self.out.exists())

    def test_dotenv_literal_only_and_environment_precedence(self):
        path = self.root / '.env'
        path.write_text(f'export AI_AGENT_STORE_TYPE="postgres"\nAI_AGENT_STORE_DSN=\'{DSN}\' # comment\nOTHER=$(touch should-not-exist)\n')
        self.assertEqual(backup.read_settings(path, {}), DSN)
        self.assertEqual(backup.read_settings(path, {'AI_AGENT_STORE_DSN': 'override'}), 'override')
        path.write_text('AI_AGENT_STORE_TYPE=postgres\nAI_AGENT_STORE_DSN=${SECRET}\n')
        with self.assertRaises(backup.BackupError):
            backup.read_settings(path, {})

    def test_uri_and_keyword_options_preserve_credentials_and_tls(self):
        values = backup.connection_env("host=127.0.0.1 dbname=fixture user=fixture password='a b\\\\c' sslmode=verify-full")
        self.assertEqual(values['PGPASSWORD'], 'a b\\c')
        self.assertEqual(values['PGSSLMODE'], 'verify-full')
        values = backup.connection_env('postgresql://fixture:p%40ss@localhost:5432/fixture?sslmode=disable')
        self.assertEqual(values['PGPASSWORD'], 'p@ss')
        self.assertEqual(values['PGDATABASE'], 'fixture')
        values = backup.connection_env('postgresql://u:p@%2Ftmp/fixture?application_name=a+b')
        self.assertEqual(values['PGHOST'], '/tmp')
        self.assertEqual(values['PGAPPNAME'], 'a+b')
        for dsn in ['postgresql://localhost/fixture', 'dbname=fixture user=u',
                    DSN + '&unknown=value', DSN + '&sslmode=require', DSN + '&options=%ZZ',
                    'postgresql://u:p@localhost/fixture?options=a%0Ab']:
            with self.subTest(dsn='redacted'), self.assertRaises(backup.BackupError):
                backup.connection_env(dsn)

    def test_docker_streams_secrets_on_stdin_and_archive_into_list(self):
        self.fake('docker', """import sys
args=sys.argv[1:]
assert args[:3]==['exec','-i','fixture-container']
assert not any('backup-test-password' in arg for arg in args)
data=sys.stdin.buffer.read()
if 'sh' in args:
    assert b'PGPASSWORD=backup-test-password\\n' in data
    assert b'PGDATABASE=fixture\\n' in data
    sys.stdout.buffer.write(b'PGDMPdocker-fake')
else:
    assert args[3:]==['pg_restore','--list']
    assert data==b'PGDMPdocker-fake'
""")
        completed = subprocess.run([sys.executable, str(SCRIPT), '--docker-container', 'fixture-container',
                                    '--output-dir', str(self.out)], env=self.env, capture_output=True)
        self.assertEqual(completed.returncode, 0, completed.stderr.decode())
        self.assertNotIn(SECRET.encode(), completed.stdout + completed.stderr)


    @unittest.skipUnless(os.environ.get('AI_AGENT_BACKUP_DOCKER_TEST') == '1',
                         'opt in to a local, network-isolated PostgreSQL container')
    def test_docker_isolated_roundtrip(self):
        name = 'ai-agent-backup-test-' + uuid.uuid4().hex[:12]
        env = {key: os.environ[key] for key in
               ['PATH', 'HOME', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG'] if key in os.environ}

        def docker(*args, data=None, check=True):
            result = subprocess.run(['docker', *args], input=data, capture_output=True, env=env, timeout=30)
            if check:
                self.assertEqual(result.returncode, 0, 'isolated Docker operation failed')
            return result

        started = False
        try:
            docker('run', '--detach', '--rm', '--pull=never', '--network', 'none', '--name', name,
                   '--tmpfs', '/var/lib/postgresql/data:rw',
                   '--env', 'POSTGRES_HOST_AUTH_METHOD=trust', '--env', 'POSTGRES_DB=backup_fixture',
                   'postgres:16-alpine')
            started = True
            for _ in range(100):
                if docker('exec', name, 'pg_isready', '-h', '127.0.0.1', '-U', 'postgres', '-d', 'backup_fixture', check=False).returncode == 0:
                    break
                time.sleep(0.1)
            else:
                self.fail('isolated PostgreSQL did not become ready')
            sql = "CREATE TABLE fixture(id serial PRIMARY KEY, value text NOT NULL); INSERT INTO fixture(value) VALUES ('备份恢复'), ('second');"
            docker('exec', '-i', name, 'psql', '-U', 'postgres', '-d', 'backup_fixture',
                   '-v', 'ON_ERROR_STOP=1', data=sql.encode())
            backup_env = dict(env, AI_AGENT_STORE_TYPE='postgres',
                              AI_AGENT_STORE_DSN='postgresql://postgres@127.0.0.1/backup_fixture?sslmode=disable')
            result = subprocess.run([sys.executable, str(SCRIPT), '--docker-container', name,
                                     '--output-dir', str(self.out)], capture_output=True, env=backup_env, timeout=60)
            self.assertEqual(result.returncode, 0, result.stderr.decode())
            roots = list(self.out.iterdir())
            self.assertEqual(len(roots), 1)
            archive = roots[0] / 'database.dump'
            self.assertTrue(json.loads((roots[0] / 'MANIFEST.json').read_text())['archive_list_verified'])
            docker('exec', name, 'createdb', '-U', 'postgres', 'backup_restore')
            docker('exec', '-i', name, 'pg_restore', '-U', 'postgres', '--exit-on-error',
                   '--dbname=backup_restore', data=archive.read_bytes())
            rows = docker('exec', name, 'psql', '-U', 'postgres', '-d', 'backup_restore', '-At',
                          '-c', 'SELECT id,value FROM fixture ORDER BY id;').stdout.decode()
            self.assertEqual(rows, '1|备份恢复\n2|second\n')
            sequence = docker('exec', name, 'psql', '-U', 'postgres', '-d', 'backup_restore', '-At',
                              '-c', "INSERT INTO fixture(value) VALUES ('after restore') RETURNING id;").stdout.decode()
            self.assertTrue(sequence.startswith('3\n'))
            (self.root / 'roundtrip.json').write_text(json.dumps({
                'passed': True, 'image': 'postgres:16-alpine', 'network': 'none',
                'restored_rows': 2, 'sequence_preserved': True,
                'production_database_accessed': False,
            }, indent=2))
            print('Isolated backup/restore evidence:', self.root)
        finally:
            if started:
                docker('stop', '--time', '5', name)


if __name__ == '__main__':
    unittest.main()
