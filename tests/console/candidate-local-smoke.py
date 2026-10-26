#!/usr/bin/env python3
"""Run a verified macOS candidate with fresh SQLite and a local model fixture.
No real identity/provider credentials; no production port, data, or process changes.
"""
import argparse
import base64
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time

ANSWER = 'Isolated candidate completed deterministically.'


def free_port(excluded):
    for _ in range(20):
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        if port not in excluded:
            return port
    raise RuntimeError('cannot select an isolated port')


def request(port, method, path, key='', body=None, timeout=5):
    headers = {'X-Request-ID': 'candidate-local-smoke'}
    if key:
        headers['X-API-Key'] = key
    data = None
    if body is not None:
        headers['Content-Type'] = 'application/json'
        data = json.dumps(body).encode()
    conn = http.client.HTTPConnection('127.0.0.1', port, timeout=timeout)
    try:
        conn.request(method, path, body=data, headers=headers)
        response = conn.getresponse()
        payload = response.read(2 * 1024 * 1024 + 1)
        if len(payload) > 2 * 1024 * 1024:
            raise RuntimeError('response exceeds smoke-test bound')
        return response.status, {key.lower(): value for key, value in response.getheaders()}, payload
    finally:
        conn.close()


def json_request(port, method, path, key='', body=None, expected=200):
    status, _, payload = request(port, method, path, key, body)
    if status != expected:
        raise RuntimeError(f'{method} {path}: status {status}, expected {expected}; response redacted')
    return json.loads(payload)


def wait_ping(proc, port, path='/ping'):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError('owned child exited before readiness; inspect retained local log')
        try:
            if request(port, 'GET', path, timeout=1)[0] == 200:
                return
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.1)
    raise RuntimeError('owned child did not become ready before timeout')


def stop(proc, grouped=False):
    if proc is None:
        return None
    if proc.poll() is None:
        for sig, timeout in [(signal.SIGINT, 10), (signal.SIGTERM, 3), (signal.SIGKILL, 3)]:
            if proc.poll() is not None:
                break
            if grouped:
                os.killpg(proc.pid, sig)  # This harness created the isolated session.
            else:
                proc.send_signal(sig)
            try:
                proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                pass
    return proc.poll()


def port_closed(port):
    with socket.socket() as sock:
        sock.settimeout(0.3)
        return sock.connect_ex(('127.0.0.1', port)) != 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--package', required=True, type=Path)
    parser.add_argument('--runner', choices=['postman', 'node'], default='postman')
    parser.add_argument('--failures', action='store_true', help='also exercise local dependency failures and recovery')
    parser.add_argument('--lifecycle', action='store_true', help='exercise running/queued cancellation and in-flight shutdown')
    args = parser.parse_args()
    if args.lifecycle and args.failures:
        parser.error('--lifecycle and --failures require separate isolated runs')
    if sys.platform != 'darwin' or not Path('/usr/bin/sandbox-exec').is_file():
        parser.error('this native fixture requires macOS sandbox-exec; use candidate-smoke.py for Linux')
    package = args.package.resolve()
    manifest = json.loads((package / 'MANIFEST.json').read_text())
    binary = package / 'server'
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    if digest != manifest['file_sha256']['server'] or manifest['goos'] != 'darwin':
        parser.error('candidate hash/platform mismatch')
    repo = Path(__file__).resolve().parents[2]
    root = Path(tempfile.mkdtemp(prefix='ai-agent-candidate-local-', dir='/private/tmp'))
    run = root / 'run'
    run.mkdir()
    (run / '.env').write_text('')
    (run / '.env').chmod(0o600)  # Prevent fallback to any parent .env.
    shutil.copy2(repo / 'teams.yaml', run / 'teams.yaml')
    workspace = run / 'workspace'
    workspace.mkdir()
    app_port = free_port({8088, 8080})
    mock_port = free_port({8088, 8080, app_port})
    api_key = os.urandom(32).hex()
    env = {key: os.environ[key] for key in ['PATH', 'HOME', 'LANG'] if key in os.environ}
    env['TMPDIR'] = '/private/tmp'
    model_prefix = '/model' if args.runner == 'postman' else ''
    cfg = {
        'api': {'addr': f'127.0.0.1:{app_port}', 'auth': {'mode': 'api_key'}},
        'store': {'type': 'sqlite', 'dsn': str(root / 'candidate.db')},
        'llm': {'provider': 'litellm', 'model': 'candidate-deterministic',
                'base_url': f'http://127.0.0.1:{mock_port}{model_prefix}/v1/chat/completions',
                'api_key': '', 'timeout_seconds': 60 if args.lifecycle else 3, 'max_calls_per_task': 2,
                'readiness_mode': 'config_only', 'scenes': {}},
        'orchestrator': {'mode': 'legacy', 'max_concurrent_tasks': 1, 'run_all_timeout_seconds': 120 if args.lifecycle else 15},
        'multiagent': {'runtime': 'legacy', 'dag_canary_percent': 0, 'harness_refine': {'enabled': False}},
        'answer_pipeline': {'enabled': False}, 'brain': {'enabled': False},
        'rag': {'search_url': '', 'context_mode': 'jit'}, 'wiki': {'required': False},
        'mcp': {'servers': []}, 'langfuse': {'enabled': False, 'bootstrap_missing_prompts': False},
        'telemetry': {'enabled': False},
        'skill': {'root': str(run / 'skills')},
        'log': {'console': True, 'file_enabled': True, 'access_enabled': True,
                'directory': str(root / 'logs'), 'retention_days': 1},
    }
    config = run / 'config.yaml'
    config.write_text(json.dumps(cfg, indent=2))
    profile = root / 'loopback.sb'
    profile.write_text('(version 1)\n(allow default)\n(deny network-outbound (require-not (remote ip "localhost:*")))\n')
    child_env = dict(env, AI_AGENT_CONFIG_FILE=str(config), AI_AGENT_API_KEY=api_key,
                     AI_AGENT_CONSOLE_SESSION_KEY=base64.b64encode(os.urandom(32)).decode(),
                     AI_AGENT_APPROVAL_ENCRYPTION_KEY=base64.b64encode(os.urandom(32)).decode())
    checks = []
    report = {'root': str(root), 'package': str(package), 'version': manifest['version'],
              'commit': manifest['commit'], 'binary_sha256': digest,
              'app_port': app_port, 'mock_port': mock_port, 'runner': args.runner,
              'real_credentials_used': False, 'production_mutations': False,
              'outbound_network_policy': 'deny non-loopback', 'failure_matrix': args.failures, 'lifecycle_matrix': args.lifecycle, 'checks': checks}
    app, mock = None, None
    handles = []

    def record(name, passed, **details):
        checks.append({'name': name, 'passed': bool(passed), **details})
        if not passed:
            raise RuntimeError(name + ' failed')

    def wait_for_task(task_id, status, timeout=3):
        deadline = time.monotonic() + timeout
        task = {}
        while time.monotonic() < deadline:
            task = json_request(app_port, 'GET', f'/api/tasks/{task_id}', api_key)
            if task.get('status') == status:
                return task
            time.sleep(0.05)
        raise RuntimeError(f'{task_id} did not reach {status}; observed {task.get("status")}')

    def wait_for_holds(expected, timeout=3):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            stats = json_request(mock_port, 'GET', model_prefix + '/_stats')
            if stats['active_holds'] == expected:
                return stats
            time.sleep(0.05)
        raise RuntimeError(f'local model active requests did not reach {expected}')

    def start_lifecycle_task(task_id):
        json_request(app_port, 'POST', '/api/tasks', api_key, {
            'id': task_id, 'goal': 'isolated lifecycle cancellation', 'workspace': str(workspace),
            'mode': 'legacy', 'max_steps': 2, 'tool_budget': 1, 'llm_call_budget': 2,
        }, expected=201)
        json_request(app_port, 'POST', f'/api/tasks/{task_id}/run-all', api_key, expected=202)

    def launch_app(index):
        log = (root / f'app-{index}.log').open('wb')
        handles.append(log)
        proc = subprocess.Popen(['/usr/bin/sandbox-exec', '-f', str(profile), str(binary)],
                                cwd=run, env=child_env, stdout=log, stderr=subprocess.STDOUT)
        return proc

    try:
        if args.runner == 'postman':
            command = [shutil.which('postman') or 'postman', 'simulation', 'run',
                       str(repo / 'postman/simulations/candidate-runtime.sim.yaml'),
                       '--port', str(mock_port), '--output', 'ndjson', '--no-history']
        else:
            command = [shutil.which('node') or 'node', str(repo / 'postman/mocks/candidate-model/default.js')]
        log = (root / 'mock.log').open('wb')
        handles.append(log)
        mock = subprocess.Popen(command, cwd=repo, env=dict(env, PORT=str(mock_port)),
                                stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        wait_ping(mock, mock_port, model_prefix + '/healthz')
        record('local model dependency listening', True)
        app = launch_app(1)
        wait_ping(app, app_port)
        record('candidate listening on isolated port', app_port not in [8088, 8080])
        ready = json_request(app_port, 'GET', '/ready')
        record('config-only readiness', ready.get('ready') and ready.get('llm_readiness_mode') == 'config_only'
               and ready.get('llm_verified') is False)
        record('anonymous protected API rejected', request(app_port, 'GET', '/api/teams')[0] == 401)
        record('invalid synthetic key rejected', request(app_port, 'GET', '/api/teams', 'invalid-smoke-key')[0] == 401)
        record('Console HTML available', request(app_port, 'GET', '/console')[0] == 200)
        status, _, body = request(app_port, 'GET', '/assets/console/app.js')
        record('embedded Console matches verified candidate', status == 200
               and hashlib.sha256(body).hexdigest() == manifest['console_app_sha256'])
        json_request(app_port, 'GET', '/api/teams', api_key)
        stats = json_request(mock_port, 'GET', model_prefix + '/_stats')
        record('startup and reads made no model calls', stats['model_calls'] == 0 and stats['unexpected_calls'] == 0)
        failed_tasks = []
        if args.failures:
            for scenario in ['timeout', 'http-json', 'http-html', 'malformed-stream']:
                json_request(mock_port, 'POST', model_prefix + '/_scenario/' + scenario)
                failed_id = 'isolated-failure-' + scenario
                before = json_request(mock_port, 'GET', model_prefix + '/_stats')['model_calls']
                json_request(app_port, 'POST', '/api/tasks', api_key, {
                    'id': failed_id, 'goal': 'isolated dependency failure', 'workspace': str(workspace),
                    'mode': 'legacy', 'max_steps': 2, 'tool_budget': 1, 'llm_call_budget': 2,
                }, expected=201)
                started = time.monotonic()
                json_request(app_port, 'POST', f'/api/tasks/{failed_id}/run-all', api_key, expected=202)
                task = {}
                while time.monotonic() - started < 18:
                    task = json_request(app_port, 'GET', f'/api/tasks/{failed_id}', api_key)
                    if task.get('status') in ['completed', 'failed', 'partial']:
                        break
                    time.sleep(0.1)
                elapsed = round(time.monotonic() - started, 3)
                record(scenario + ' reaches failed state within bound',
                       task.get('status') == 'failed' and elapsed < 6,
                       status=task.get('status'), elapsed_seconds=elapsed)
                after = json_request(mock_port, 'GET', model_prefix + '/_stats')['model_calls']
                record(scenario + ' dependency attempts bounded', 1 <= after - before <= 2,
                       dependency_calls=after - before)
                status, headers, stream = request(app_port, 'GET', f'/api/tasks/{failed_id}/stream', api_key)
                record(scenario + ' failure readable via terminal SSE', status == 200
                       and headers.get('x-sse-recovery') == 'snapshot' and b'failed' in stream)
                record(scenario + ' raw upstream response absent from task and SSE',
                       'candidate-upstream-secret-sentinel' not in json.dumps(task)
                       and b'candidate-upstream-secret-sentinel' not in stream)
                record(scenario + ' application remains responsive', request(app_port, 'GET', '/ping')[0] == 200)
                failed_tasks.append(failed_id)
            json_request(mock_port, 'POST', model_prefix + '/_scenario/success')
        calls_before_success = json_request(mock_port, 'GET', model_prefix + '/_stats')['model_calls']
        task_id = 'isolated-candidate-task'
        json_request(app_port, 'POST', '/api/tasks', api_key, {
            'id': task_id, 'goal': 'isolated deterministic completion', 'workspace': str(workspace),
            'mode': 'legacy', 'max_steps': 2, 'tool_budget': 1, 'token_budget': 1000, 'llm_call_budget': 2,
        }, expected=201)
        json_request(app_port, 'POST', f'/api/tasks/{task_id}/run-all', api_key, expected=202)
        deadline = time.monotonic() + 15
        task = {}
        while time.monotonic() < deadline:
            task = json_request(app_port, 'GET', f'/api/tasks/{task_id}', api_key)
            if task.get('status') in ['completed', 'failed', 'partial']:
                break
            time.sleep(0.1)
        record('isolated deterministic task completed', task.get('status') == 'completed'
               and task.get('final_answer') == ANSWER, status=task.get('status'))
        stats = json_request(mock_port, 'GET', model_prefix + '/_stats')
        record('one local streaming planner call and no unexpected dependencies',
               stats['model_calls'] == calls_before_success + 1 and stats['streaming_calls'] == 1 and stats['unexpected_calls'] == 0,
               model_calls=stats['model_calls'], unexpected_calls=stats['unexpected_calls'])
        status, headers, stream = request(app_port, 'GET', f'/api/tasks/{task_id}/stream', api_key)
        record('candidate SSE terminal snapshot', status == 200 and headers.get('x-sse-recovery') == 'snapshot'
               and ANSWER.encode() in stream)
        if args.lifecycle:
            json_request(mock_port, 'POST', model_prefix + '/_scenario/hold')
            running_id, queued_id = 'isolated-cancel-running', 'isolated-cancel-queued'
            start_lifecycle_task(running_id)
            held = wait_for_holds(1)
            record('running cancellation starts with live dependency request', held['active_holds'] == 1)
            start_lifecycle_task(queued_id)
            before_cancel = time.monotonic()
            json_request(app_port, 'DELETE', f'/api/tasks/{queued_id}/cancel', api_key)
            queued = wait_for_task(queued_id, 'failed')
            record('queued cancellation completes without dependency call',
                   queued.get('termination_kind') == 'client_cancelled'
                   and json_request(mock_port, 'GET', model_prefix + '/_stats')['model_calls'] == held['model_calls'],
                   elapsed_seconds=round(time.monotonic() - before_cancel, 3))
            before_cancel = time.monotonic()
            json_request(app_port, 'DELETE', f'/api/tasks/{running_id}/cancel', api_key)
            canceled = wait_for_task(running_id, 'failed')
            after_cancel = wait_for_holds(0)
            record('running cancellation closes model request before 60s timeout',
                   canceled.get('termination_kind') == 'client_cancelled'
                   and canceled.get('error_code') == 'task_canceled'
                   and after_cancel['aborted_holds'] == held['aborted_holds'] + 1
                   and time.monotonic() - before_cancel < 3,
                   elapsed_seconds=round(time.monotonic() - before_cancel, 3))
            status, headers, stream = request(app_port, 'GET', f'/api/tasks/{running_id}/stream', api_key)
            record('running cancellation visible in terminal SSE', status == 200
                   and headers.get('x-sse-recovery') == 'snapshot' and b'task_canceled' in stream)
            shutdown_id = 'isolated-shutdown-running'
            start_lifecycle_task(shutdown_id)
            held = wait_for_holds(1)
            record('shutdown begins while model request is active', held['active_holds'] == 1)
            calls_before_restart = held['model_calls']
            shutdown_started = time.monotonic()
            app.send_signal(signal.SIGINT)
            first_exit = app.wait(timeout=5)
            drained = wait_for_holds(0)
            record('SIGINT drains active model request within five seconds', first_exit == 0
                   and drained['aborted_holds'] == held['aborted_holds'] + 1
                   and time.monotonic() - shutdown_started < 5,
                   elapsed_seconds=round(time.monotonic() - shutdown_started, 3))
        else:
            calls_before_restart = stats['model_calls']
            first_exit = stop(app)
        record('first candidate graceful shutdown', first_exit == 0, exit_code=first_exit)
        record('candidate port released before restart', port_closed(app_port))
        app = launch_app(2)
        wait_ping(app, app_port)
        task = json_request(app_port, 'GET', f'/api/tasks/{task_id}', api_key)
        record('fresh SQLite task survives candidate restart', task.get('status') == 'completed'
               and task.get('final_answer') == ANSWER)
        for failed_id in failed_tasks:
            restored = json_request(app_port, 'GET', f'/api/tasks/{failed_id}', api_key)
            record(failed_id + ' failure survives restart', restored.get('status') == 'failed')
        stats = json_request(mock_port, 'GET', model_prefix + '/_stats')
        record('restart made no additional model calls', stats['model_calls'] == calls_before_restart and stats['unexpected_calls'] == 0)
        if args.lifecycle:
            for canceled_id in [running_id, queued_id]:
                restored = json_request(app_port, 'GET', f'/api/tasks/{canceled_id}', api_key)
                record(canceled_id + ' stays canceled after restart', restored.get('status') == 'failed'
                       and restored.get('termination_kind') == 'client_cancelled')
            paused = json_request(app_port, 'GET', f'/api/tasks/{shutdown_id}', api_key)
            record('in-flight shutdown persists paused rollback state', paused.get('status') == 'paused'
                   and paused.get('termination_kind') == 'shutdown_rollback', status=paused.get('status'))
            json_request(mock_port, 'POST', model_prefix + '/_scenario/success')
            json_request(app_port, 'POST', f'/api/tasks/{shutdown_id}/run-all', api_key, expected=202)
            resumed = wait_for_task(shutdown_id, 'completed')
            resumed_stats = json_request(mock_port, 'GET', model_prefix + '/_stats')
            record('explicit resume completes previously interrupted task', resumed.get('final_answer') == ANSWER
                   and resumed_stats['model_calls'] == calls_before_restart + 1
                   and resumed_stats['active_holds'] == 0 and resumed_stats['unexpected_calls'] == 0)
        versions = []
        for p in [root / 'app-1.log', root / 'app-2.log']:
            for line in p.read_text(errors='replace').splitlines():
                try:
                    entry = json.loads(line)
                except ValueError:
                    continue
                if entry.get('msg') == 'HTTP server listening':
                    versions.append(entry.get('app_version'))
        record('both runtime logs report bound candidate version', versions == [manifest['version']] * 2)
        record('application logs exclude raw upstream sentinel', all(
            'candidate-upstream-secret-sentinel' not in p.read_text(errors='replace')
            for p in root.rglob('*.log') if p.name != 'mock.log'))
        record('candidate binary unchanged after execution', hashlib.sha256(binary.read_bytes()).hexdigest() == digest)
        report['passed'] = True
    except Exception as error:
        report['passed'] = False
        report['error'] = str(error)[:300]
    finally:
        app_exit = stop(app)
        mock_exit = stop(mock, grouped=True)
        for handle in handles:
            handle.close()
        report['cleanup'] = {'app_exit': app_exit, 'mock_exit': mock_exit,
                             'app_port_closed': port_closed(app_port), 'mock_port_closed': port_closed(mock_port)}
        if (app_exit != 0 or mock_exit != 0
                or not all(report['cleanup'][key] for key in ['app_port_closed', 'mock_port_closed'])):
            report['passed'] = False
        (root / 'result.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))
    return 0 if report.get('passed') else 1


if __name__ == '__main__':
    sys.exit(main())
