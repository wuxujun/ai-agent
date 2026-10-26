#!/usr/bin/env python3
"""Create a private, checked PostgreSQL custom archive; never restore or delete data."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from urllib.parse import urlsplit, unquote


class BackupError(Exception):
    pass


def read_settings(env_file, environ):
    values = {}
    if env_file:
        # Read only these literal, single-line dotenv entries. Never source a shell.
        for line in env_file.read_text().splitlines():
            match = re.match(r'^\s*(?:export\s+)?(AI_AGENT_STORE_TYPE|AI_AGENT_STORE_DSN)\s*=\s*(.*)$', line)
            if not match:
                continue
            key, raw = match.groups()
            if raw.startswith(('"', "'")):
                quote = raw[0]
                match = re.fullmatch(re.escape(quote) + r'(.*?)' + re.escape(quote) + r'\s*(?:#.*)?', raw)
                if not match:
                    raise BackupError('database setting must be a literal single-line value')
                value = match[1]
                if quote == '"' and '\\' in value:
                    raise BackupError('escaped dotenv values are unsupported; supply process environment instead')
            else:
                value = re.split(r'\s+#', raw, maxsplit=1)[0].strip()
            if '${' in value or '$(' in value or '`' in value:
                raise BackupError('expanded dotenv values are unsupported; supply literal process environment instead')
            values[key] = value
    for key in ['AI_AGENT_STORE_TYPE', 'AI_AGENT_STORE_DSN']:
        if key in environ:
            values[key] = environ[key]
    if values.get('AI_AGENT_STORE_TYPE', '').strip().lower() not in ['postgres', 'postgresql']:
        raise BackupError('AI_AGENT_STORE_TYPE must explicitly select postgres')
    dsn = values.get('AI_AGENT_STORE_DSN', '')
    if not dsn.strip() or any(c in dsn for c in '\r\n\x00'):
        raise BackupError('AI_AGENT_STORE_DSN must contain one nonempty line')
    return dsn



# Explicit libpq environment mapping; reject options this utility cannot preserve.
PG_OPTIONS = {
    'host': 'PGHOST', 'port': 'PGPORT', 'dbname': 'PGDATABASE', 'user': 'PGUSER',
    'password': 'PGPASSWORD', 'sslmode': 'PGSSLMODE', 'sslrootcert': 'PGSSLROOTCERT',
    'sslcert': 'PGSSLCERT', 'sslkey': 'PGSSLKEY', 'application_name': 'PGAPPNAME',
    'options': 'PGOPTIONS', 'connect_timeout': 'PGCONNECT_TIMEOUT',
    'target_session_attrs': 'PGTARGETSESSIONATTRS', 'channel_binding': 'PGCHANNELBINDING',
}


def connection_env(dsn):
    values = {}
    if dsn.startswith(('postgres://', 'postgresql://')):
        if re.search(r'%(?![0-9a-fA-F]{2})', dsn):
            raise BackupError('invalid URI percent escape')
        url = urlsplit(dsn)
        if url.fragment:
            raise BackupError('URI fragments are unsupported; percent-encode special characters')
        values = {'host': unquote(url.hostname or '', errors='strict'),
                  'dbname': unquote(url.path[1:] if url.path.startswith('/') else url.path, errors='strict')}
        if url.port is not None:
            values['port'] = str(url.port)
        if url.username is not None:
            values['user'] = unquote(url.username, errors='strict')
        if url.password is not None:
            values['password'] = unquote(url.password, errors='strict')
        for item in url.query.split('&') if url.query else []:
            if '=' not in item:
                raise BackupError('URI query options must use key=value')
            key, value = (unquote(part, errors='strict') for part in item.split('=', 1))
            if key in values:
                raise BackupError('duplicate connection option unsupported')
            values[key] = value
    else:
        # libpq keyword/value syntax, including quoted spaces and backslash escapes.
        pattern = re.compile(r"\s*([a-z_]+)\s*=\s*(?:'((?:[^'\\]|\\.)*)'|((?:[^\s'\\]|\\.)*))")
        offset = 0
        while offset < len(dsn.rstrip()):
            match = pattern.match(dsn, offset)
            if not match or match[1] in values:
                raise BackupError('invalid or duplicate connection option')
            raw = match[2] if match[2] is not None else match[3]
            values[match[1]] = re.sub(r'\\(.)', r'\1', raw)
            offset = match.end()
    if not values.get('host') or not values.get('dbname') or not values.get('user'):
        raise BackupError('DSN must explicitly specify host, database and user')
    if any(k not in PG_OPTIONS for k in values):
        raise BackupError('DSN has unsupported connection options; see backup README')
    if any(any(c in value for c in '\r\n\x00') for value in values.values()):
        raise BackupError('connection values must contain no line breaks or NUL')
    result = {PG_OPTIONS[key]: value for key, value in values.items()}
    result['PGCONNECT_TIMEOUT'] = '10'
    return result


def run_checked(command, env, timeout, stdin=None, stdout=None):
    try:
        result = subprocess.run(command, env=env, input=stdin, stdout=stdout or subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=timeout, check=False)
    except subprocess.TimeoutExpired:
        raise BackupError('client timed out; partial output retained') from None
    except OSError:
        raise BackupError('client could not be executed') from None
    if result.returncode != 0:
        raise BackupError(f'client failed (exit {result.returncode}); diagnostic body suppressed')
    # pg_dump can finish with warnings. Do not silently label a warning as verified.
    if result.stderr.strip():
        raise BackupError('client emitted diagnostics; backup not accepted; diagnostic body suppressed')
    return result.stdout


def private_file(path, content):
    with path.open('xb') as stream:
        os.chmod(path, 0o600)
        stream.write(content)
        stream.flush()
        os.fsync(stream.fileno())


def sha256(path):
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(chunk)
    return digest.hexdigest()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--env-file', type=Path, help='explicit literal dotenv file; process environment wins')
    parser.add_argument('--output-dir', type=Path, default=Path(__file__).resolve().parents[2] / 'data/backups')
    parser.add_argument('--pg-bin-dir', type=Path, help='directory containing native pg_dump and pg_restore')
    parser.add_argument('--docker-container', help='use clients in this explicitly named running container')
    parser.add_argument('--timeout-seconds', type=int, default=1800)
    parser.add_argument('--dry-run', action='store_true', help='validate inputs and tools without connecting or writing')
    args = parser.parse_args(argv)
    if args.timeout_seconds <= 0 or (args.pg_bin_dir and args.docker_container):
        parser.error('positive timeout required; select native or Docker clients, not both')
    root = None
    stage = 'preflight'
    try:
        pg_env = connection_env(read_settings(args.env_file, os.environ))
        # Avoid inheriting unrelated model credentials, PG overrides or startup hooks.
        env = {k: os.environ[k] for k in ['PATH', 'HOME', 'LANG', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG']
               if k in os.environ}
        env['PGCONNECT_TIMEOUT'] = '10'
        if args.docker_container:
            if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*', args.docker_container):
                raise BackupError('invalid container name')
            docker = shutil.which('docker')
            if not docker:
                raise BackupError('docker executable not found')
            # Pass the DSN through stdin, never docker argv, an env file or a shell interpolation.
            dump = [docker, 'exec', '-i', args.docker_container, 'sh', '-c',
                    'while IFS= read -r entry && [ -n "$entry" ]; do '
                    'export "$entry" || exit 2; done; exec pg_dump "$@"', 'backup-client']
            restore = [docker, 'exec', '-i', args.docker_container, 'pg_restore']
            dump_input = (''.join(f'{key}={value}\n' for key, value in pg_env.items()) + '\n').encode()
        else:
            def executable(name):
                path = str(args.pg_bin_dir.resolve() / name) if args.pg_bin_dir else shutil.which(name)
                if not path or not os.path.isfile(path) or not os.access(path, os.X_OK):
                    raise BackupError('pg_dump and pg_restore are required; select native binaries or --docker-container')
                return path
            dump, restore = [executable('pg_dump')], [executable('pg_restore')]
            env.update(pg_env)
            dump_input = None
        if args.dry_run:
            print('Preflight passed; no connection made and no backup created. Target configuration not displayed.')
            return 0
        os.umask(0o077)
        args.output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        if args.output_dir.is_symlink():
            raise BackupError('output directory must not be a symlink')
        stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
        root = Path(tempfile.mkdtemp(prefix=f'.postgres-{stamp}-', suffix='.partial', dir=args.output_dir))
        os.chmod(root, 0o700)
        archive = root / 'database.dump'
        stage = 'dump'
        with archive.open('xb') as stream:
            os.chmod(archive, 0o600)
            run_checked(dump + ['--format=custom', '--no-password', '--lock-wait-timeout=10s'],
                        env, args.timeout_seconds, stdin=dump_input, stdout=stream)
            stream.flush()
            os.fsync(stream.fileno())
        with archive.open('rb') as source:
            header = source.read(5)
        if archive.stat().st_size < 5 or header != b'PGDMP':
            raise BackupError('custom archive header missing')
        stage = 'archive-list'
        # Read the archive locally or feed it into the container; never connect a restore client.
        if args.docker_container:
            with archive.open('rb') as source:
                # Stream the dump rather than reading the whole database into Python memory.
                try:
                    result = subprocess.run(restore + ['--list'], stdin=source, stdout=subprocess.DEVNULL,
                                            stderr=subprocess.PIPE, env=env, timeout=args.timeout_seconds)
                except subprocess.TimeoutExpired:
                    raise BackupError('archive inspection timed out') from None
                if result.returncode != 0 or result.stderr.strip():
                    raise BackupError('archive inspection failed; diagnostic body suppressed')
        else:
            run_checked(restore + ['--list', str(archive)], env, args.timeout_seconds, stdout=subprocess.DEVNULL)
        manifest = {
            'format_version': 1, 'status': 'archive_checked', 'created_at_utc': stamp,
            'backend': 'postgres', 'client_mode': 'docker' if args.docker_container else 'native',
            'archive': 'database.dump', 'bytes': archive.stat().st_size, 'sha256': sha256(archive),
            'archive_list_verified': True, 'restore_verified': False,
            'includes': ['single database schema and data'],
            'excludes': ['cluster roles and tablespaces', 'workspace files', 'configuration and encryption keys'],
        }
        stage = 'publish'
        private_file(root / 'MANIFEST.json', (json.dumps(manifest, indent=2) + '\n').encode())
        sums = ''.join(f'{sha256(root / name)}  {name}\n' for name in ['database.dump', 'MANIFEST.json'])
        private_file(root / 'SHA256SUMS', sums.encode())
        destination = root.with_name(root.name[1:].removesuffix('.partial'))
        if destination.exists():
            raise BackupError('refusing to replace an existing backup')
        root.rename(destination)
        root = destination
        print(f'Backup created: {root}\nArchive inspection and SHA-256 passed; restoration has NOT been tested.')
        return 0
    except (BackupError, OSError, ValueError) as error:
        # Never print exception text from libraries/clients: it can contain a DSN/password.
        if root is not None and root.name.endswith('.partial'):
            try:
                private_file(root / 'FAILED.json', json.dumps({'status': 'failed', 'stage': stage}).encode())
            except OSError:
                pass
        reason = str(error) if isinstance(error, BackupError) else 'local input or filesystem error'
        print(f'Backup failed during {stage}: {reason}; no complete backup published. '
              'Partial files, if any, are retained; connection details suppressed.', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
