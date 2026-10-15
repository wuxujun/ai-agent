#!/usr/bin/env python3
"""Run an existing candidate binary with local-only auth and a fresh SQLite DB.
No task execution/model calls; retain all artifacts and stop only owned children.
"""
import argparse
import base64
import hashlib
import http.cookiejar
import http.client
import http.server
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import threading
import urllib.error
import urllib.request


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def stop(proc):
    if proc and proc.poll() is None:
        for sig, timeout in [(signal.SIGINT, 10), (signal.SIGTERM, 3), (signal.SIGKILL, 3)]:
            if proc.poll() is not None:
                break
            os.killpg(proc.pid, sig)  # Group created by this harness only.
            try:
                proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--package', required=True, type=Path)
    parser.add_argument('--docker-image', help='Existing Linux image pinned by sha256, for a Linux candidate')
    args = parser.parse_args()
    if sys.platform == 'darwin' and not args.docker_image:
        parser.error('macOS uses system TLS roots; use a Linux candidate with --docker-image rather than changing system trust')
    if args.docker_image and not args.docker_image.startswith('sha256:'):
        parser.error('--docker-image must be a locally cached sha256 image id')
    package = args.package.resolve()
    manifest = json.loads((package / 'MANIFEST.json').read_text())
    binary = package / 'server'
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    assert digest == manifest['file_sha256']['server'], 'candidate hash mismatch'
    repo = Path(__file__).resolve().parents[2]
    postman = shutil.which('postman')
    assert postman, 'Postman CLI >= 1.70 required'
    root = Path(tempfile.mkdtemp(prefix='ai-agent-candidate-runtime-', dir='/private/tmp'))
    run = root / 'run'
    run.mkdir()
    (run / '.env').write_text('')  # Prevent server parent .env fallback.
    (run / '.env').chmod(0o600)
    shutil.copy2(package / 'teams.yaml', run / 'teams.yaml')
    shutil.copytree(package / 'skills', run / 'skills')
    roots = {tenant: run / tenant for tenant in ['candidate-a', 'candidate-b']}
    for path in roots.values():
        path.mkdir()
    sim_port, app_port = free_port(), free_port()
    while app_port == sim_port:
        app_port = free_port()
    assert app_port != 8088 and sim_port != 8088
    sim_url = f'http://127.0.0.1:{sim_port}'
    tls_port = free_port()
    while tls_port in [app_port, sim_port, 8088]:
        tls_port = free_port()
    identity_host = 'host.docker.internal' if args.docker_image else '127.0.0.1'
    identity_url = f'https://{identity_host}:{tls_port}'
    backend_url = f'http://127.0.0.1:{app_port}'
    app_url = backend_url
    # JSON is valid YAML. Use a complete, dedicated config and minimal env.
    cfg = {
        'api': {'addr': f'{"0.0.0.0" if args.docker_image else "127.0.0.1"}:{app_port}', 'auth': {
            'mode': 'introspection', 'require_tenant_workspace_root': True,
            'bearer': {'require_subject': True},
            'introspection': {'url': identity_url + '/auth/oauth/introspect',
                'tenant_claim': 'data.userid', 'subject_claim': 'data.sub',
                'active_claim': 'data.active', 'issuer': 'https://candidate.invalid',
                'audience': 'candidate-console', 'require_expiration': True,
                'require_known_tenant': True, 'allow_private_network': True,
                'timeout_seconds': 2, 'cache_ttl_seconds': 0}},
            'tenants': {t: {'workspace_root': str(p), 'approval_mode': 'deny'} for t, p in roots.items()}},
        'store': {'type': 'sqlite', 'dsn': str(root / 'candidate.db')},
        'llm': {'provider': 'litellm', 'model': 'candidate-unused',
            'base_url': sim_url + '/auth/unmodeled-llm', 'api_key': '', 'timeout_seconds': 2},
        'orchestrator': {'mode': 'legacy', 'max_concurrent_tasks': 1},
        'answer_pipeline': {'enabled': False}, 'brain': {'enabled': False},
        'langfuse': {'enabled': False, 'bootstrap_missing_prompts': False},
        'telemetry': {'enabled': False},
        'log': {'console': True, 'file_enabled': False, 'access_enabled': False},
    }
    config = run / 'config.yaml'
    config.write_text(json.dumps(cfg, indent=2))
    env = {k: os.environ[k] for k in ['PATH', 'HOME', 'LANG'] if k in os.environ}
    env['TMPDIR'] = '/private/tmp'
    env['AI_AGENT_CONFIG_FILE'] = str(config)
    env['AI_AGENT_CONSOLE_SESSION_KEY'] = base64.b64encode(os.urandom(32)).decode()
    results = []
    # The application requires HTTPS even for private-network opt-in. This relay
    # uses a one-day test certificate trusted only by the child application.
    cert, key = root / 'identity.crt', root / 'identity.key'
    subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
        '-days', '1', '-subj', '/CN=localhost', '-addext',
        'subjectAltName=IP:127.0.0.1,DNS:localhost,DNS:host.docker.internal', '-keyout', str(key),
        '-out', str(cert)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    key.chmod(0o600)
    env['SSL_CERT_FILE'] = str(cert)
    relay_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=str(cert))))

    class TLSIdentityRelay(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            request = urllib.request.Request(sim_url + self.path,
                headers={'Authorization': self.headers.get('Authorization', '')})
            try:
                response = relay_opener.open(request, timeout=3)
            except urllib.error.HTTPError as exc:
                response = exc
            with response:
                body = response.read(1 << 20)
                self.send_response(response.status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        def log_message(self, *args):
            pass

    relay = http.server.ThreadingHTTPServer(('127.0.0.1', tls_port), TLSIdentityRelay)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(cert, key)
    relay.socket = tls.wrap_socket(relay.socket, server_side=True)
    threading.Thread(target=relay.serve_forever, daemon=True).start()
    # A container's non-loopback peer correctly makes Console cookies Secure.
    # Terminate local test HTTPS in front of its loopback-published HTTP port.
    proxy = None
    if args.docker_image:
        proxy_port = free_port()
        while proxy_port in [app_port, sim_port, tls_port, 8088]:
            proxy_port = free_port()
        app_url = f'https://127.0.0.1:{proxy_port}'

        class HTTPSAppProxy(http.server.BaseHTTPRequestHandler):
            def forward(self):
                length = int(self.headers.get('Content-Length', '0'))
                if length > (1 << 20):
                    self.send_error(413)
                    return
                body = self.rfile.read(length) if length else None
                headers = {k: v for k, v in self.headers.items()
                    if k.lower() not in ['connection', 'content-length']}
                upstream = urllib.request.Request(backend_url + self.path,
                    data=body, headers=headers, method=self.command)
                try:
                    response = relay_opener.open(upstream, timeout=5)
                except urllib.error.HTTPError as exc:
                    response = exc
                except (urllib.error.URLError, http.client.HTTPException, ConnectionError, TimeoutError):
                    self.send_error(502)
                    return
                with response:
                    content = response.read(1 << 20)
                    self.send_response(response.status)
                    for key, value in response.headers.items():
                        if key.lower() not in ['connection', 'transfer-encoding', 'content-length']:
                            self.send_header(key, value)
                    self.send_header('Content-Length', str(len(content)))
                    self.end_headers()
                    self.wfile.write(content)

            do_GET = do_POST = do_DELETE = forward

            def log_message(self, *args):
                pass

        proxy = http.server.ThreadingHTTPServer(('127.0.0.1', proxy_port), HTTPSAppProxy)
        proxy.socket = tls.wrap_socket(proxy.socket, server_side=True)
        threading.Thread(target=proxy.serve_forever, daemon=True).start()
    simulation = app = None
    container_name = 'candidate-smoke-' + root.name.removeprefix('ai-agent-candidate-runtime-')
    sim_out = (root / 'simulation.ndjson').open('w')
    sim_err = (root / 'simulation.err').open('w')
    app_log = (root / 'candidate.log').open('w')
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=str(cert))))

    def request(method, path, body=None, headers=None, client=opener, expected=200):
        data = None if body is None else json.dumps(body).encode()
        hdr = dict(headers or {})
        if data is not None:
            hdr['Content-Type'] = 'application/json'
        req = urllib.request.Request(app_url + path, data=data, headers=hdr, method=method)
        try:
            response = client.open(req, timeout=5)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            content = response.read(1 << 20)
            assert response.status == expected, f'{method} {path}: {response.status}, expected {expected}'
            results.append({'method': method, 'path': path, 'status': response.status})
            return content, response.headers

    try:
        simulation = subprocess.Popen([postman, 'simulation', 'run', str(repo / 'postman/simulations/console-candidate.sim.yaml'), '--port', str(sim_port), '--output', 'ndjson', '--no-history'], env=env, stdout=sim_out, stderr=sim_err, start_new_session=True)
        for _ in range(100):
            if simulation.poll() is not None:
                raise RuntimeError('simulation exited; inspect ' + str(root / 'simulation.err'))
            if 'listening' in (root / 'simulation.ndjson').read_text():
                break
            time.sleep(0.1)
        else:
            raise RuntimeError('simulation did not listen')
        command = [str(binary)]
        if args.docker_image:
            assert manifest['goos'] == 'linux', 'container mode requires a Linux candidate'
            command = ['docker', 'run', '--rm', '--name', container_name,
                '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                '--user', f'{os.getuid()}:{os.getgid()}', '--memory', '512m', '--cpus', '1',
                '--pids-limit', '128', '-p', f'127.0.0.1:{app_port}:{app_port}',
                '--mount', f'type=bind,source={package},target=/candidate,readonly',
                '--mount', f'type=bind,source={root},target={root}', '--workdir', str(run)]
            for name in ['AI_AGENT_CONFIG_FILE', 'AI_AGENT_CONSOLE_SESSION_KEY', 'SSL_CERT_FILE']:
                command += ['-e', name]
            command += [args.docker_image, '/candidate/server']
        app = subprocess.Popen(command, cwd=run, env=env, stdout=app_log, stderr=subprocess.STDOUT, start_new_session=True)
        for _ in range(100):
            if app.poll() is not None:
                raise RuntimeError('candidate exited; inspect ' + str(root / 'candidate.log'))
            try:
                with opener.open(app_url + '/ping', timeout=0.3) as response:
                    if response.status == 200:
                        break
            except (urllib.error.URLError, http.client.HTTPException, ConnectionError, TimeoutError):
                pass
            time.sleep(0.1)
        else:
            raise RuntimeError('candidate readiness check failed; inspect ' + str(root / 'candidate.log'))
        request('GET', '/console')
        request('GET', '/console/session', expected=401)
        request('GET', '/api/tasks', expected=401)
        request('POST', '/console/session', {'token': 'invalid-candidate-fixture'}, expected=401)
        jar = http.cookiejar.CookieJar()
        browser = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(jar), urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=str(cert))))
        _, headers = request('POST', '/console/session', {'token': 'candidate-smoke-a'}, client=browser)
        cookie = headers.get('Set-Cookie', '')
        assert 'HttpOnly' in cookie and 'SameSite=Strict' in cookie and 'candidate-smoke-a' not in cookie
        if args.docker_image:
            assert 'Secure' in cookie, 'container cookie must remain Secure'
        content, _ = request('GET', '/console/session', client=browser)
        session = json.loads(content)
        assert session['tenant_id'] == 'candidate-a'
        csrf = session['csrf_token']
        payload = {'goal': 'isolated candidate creation only', 'workspace': str(roots['candidate-a'])}
        request('POST', '/api/tasks', payload, client=browser, expected=403)
        request('POST', '/api/tasks', payload, {'X-Console-CSRF': csrf, 'Origin': 'https://invalid.example'}, browser, 403)
        content, _ = request('POST', '/api/tasks', payload, {'X-Console-CSRF': csrf}, browser, 201)
        task = json.loads(content)
        request('GET', '/api/tasks/' + task['id'], client=browser)
        request('GET', '/api/tasks/' + task['id'], headers={'Authorization': 'Bearer candidate-smoke-b'}, expected=404)
        request('DELETE', '/console/session', headers={'X-Console-CSRF': csrf}, client=browser, expected=204)
        request('GET', '/console/session', client=browser, expected=401)
        log = (root / 'candidate.log').read_text()
        assert manifest['version'] in log, 'candidate version missing from runtime logs'
    finally:
        if args.docker_image and app is not None and app.poll() is None:
            subprocess.run(['docker', 'stop', '-t', '10', container_name], env=env,
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
        stop(app)
        stop(simulation)
        relay.shutdown()
        relay.server_close()
        if proxy is not None:
            proxy.shutdown()
            proxy.server_close()
        sim_out.close()
        sim_err.close()
        app_log.close()
        report = {'artifact_root': str(root), 'candidate_sha256': digest, 'version': manifest['version'], 'checks': results, 'production_service_touched': False, 'real_credentials_used': False, 'tasks_executed': 0, 'docker_image': args.docker_image}
        (root / 'result.json').write_text(json.dumps(report, indent=2))
        print(json.dumps(report))
    assert app.returncode == 0, 'candidate did not shut down cleanly'
    log = (root / 'simulation.ndjson').read_text()
    events = [json.loads(line) for line in log.splitlines() if line.startswith('{')]
    requests = [e for e in events if e.get('type') == 'request' or e.get('event') == 'request']
    assert requests, 'no simulation request evidence'
    assert 'unmodeled-llm' not in log, 'unexpected model request'
    report.update(pass_all=True, dependency_request_events=len(requests), clean_shutdown=True)
    (root / 'result.json').write_text(json.dumps(report, indent=2))
    print('PASS: candidate runtime and dependency request evidence')


if __name__ == '__main__':
    main()
