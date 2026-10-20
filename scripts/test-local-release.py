#!/usr/bin/env python3
"""Smoke-test an extracted native release without installing host components."""
import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import http.client
import importlib.util
import json
import os
from pathlib import Path
import platform
import queue
import socket
import subprocess
import tarfile
import tempfile
import threading
import time
import zipfile

_probe_spec = importlib.util.spec_from_file_location('capture_runtime_check', Path(__file__).with_name('capture-runtime-check.py'))
_probe = importlib.util.module_from_spec(_probe_spec)
_probe_spec.loader.exec_module(_probe)


def ensure(condition, message):
    if not condition: raise RuntimeError(message)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    args = parser.parse_args()
    archive = args.archive.resolve()
    manifest = json.loads((archive.parent/'manifest.json').read_text())
    entries = [item for item in manifest['platforms'].values() if item['file'] == archive.name]
    if len(entries) != 1 or entries[0]['sha256'] != hashlib.sha256(archive.read_bytes()).hexdigest():
        parser.error('release manifest checksum mismatch')
    received = queue.Queue()

    class Model(BaseHTTPRequestHandler):
        def do_POST(self):
            if self.headers.get('Transfer-Encoding', '').lower() == 'chunked':
                chunks = []
                while True:
                    size = int(self.rfile.readline().split(b';', 1)[0], 16)
                    if not size:
                        while self.rfile.readline() != b'\r\n': pass
                        break
                    chunks.append(self.rfile.read(size))
                    self.rfile.read(2)
                body = b''.join(chunks)
            else:
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
            received.put((self.path, dict(self.headers), body))
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args): pass

    with tempfile.TemporaryDirectory(prefix='silencer-release-smoke-') as temporary:
        root = Path(temporary)
        bundle = root/'bundle'
        bundle.mkdir()
        if archive.suffix == '.zip':
            with zipfile.ZipFile(archive) as source: source.extractall(bundle)
        else:
            with tarfile.open(archive) as source: source.extractall(bundle, filter='data')
        executable = 'mitmdump.exe' if os.name == 'nt' else 'mitmdump'
        env = {key:value for key,value in os.environ.items() if not key.startswith(('SILENCER_', 'OTEL_')) and key not in ('LD_LIBRARY_PATH', 'DYLD_LIBRARY_PATH')}
        version = subprocess.check_output([str(bundle/'runtime'/executable), '--version'], env=env, text=True, timeout=15)
        if 'Mitmproxy: 12.2.3' not in version: raise RuntimeError('unexpected bundled runtime')
        system = platform.system().lower()
        if system in ('linux', 'windows'):
            actual = _probe.inspect_runtime(bundle/'runtime'/executable, system, bundle/'runtime')
            expected = json.loads((bundle/'capture-runtime-manifest.json').read_text())
            ensure(actual == expected, 'bundled capture helper integrity mismatch')
        if platform.system() == 'Darwin':
            subprocess.run(['codesign', '--verify', '--deep', '--strict', str(bundle/'runtime/mitmproxy.app')], check=True)
        model = ThreadingHTTPServer(('127.0.0.1', 0), Model)
        model_thread = threading.Thread(target=model.serve_forever, daemon=True)
        model_thread.start()
        reservation = socket.socket()
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
        reservation.close()
        target = f'http://127.0.0.1:{model.server_port}'
        state = root/'state'
        state.mkdir()
        env.update({'SILENCER_LOCAL_MODE':'true', 'SILENCER_GATEWAY_ADDR':f'127.0.0.1:{port}', 'SILENCER_ENGINE':'cpp', 'SILENCER_GATEWAY_TOKEN':'release-smoke-fixture', 'SILENCER_CONTROL_CONFIG_FILE':str(state/'control.json'), 'SILENCER_SQLITE_PATH':str(state/'test.db'), 'SILENCER_WEB_DIST':str(bundle/'web_dist'), 'SILENCER_MITMDUMP_PATH':str(bundle/'runtime'/executable), 'SILENCER_CAPTURE_FORWARD_ENABLED':'true', 'SILENCER_CAPTURE_ALLOWED_TARGETS':f'127.0.0.1:{model.server_port}'})
        env['SILENCER_INSTALLATION_ID'] = entries[0]['sha256']
        if platform.system() == 'Linux': env['LD_LIBRARY_PATH'] = str(bundle/'lib')
        if os.name == 'nt':
            # Do not let a build-machine MinGW PATH hide missing release DLLs.
            windows = Path(os.environ['SystemRoot'])
            env['PATH'] = os.pathsep.join(str(path) for path in [bundle/'lib', windows/'System32', windows, windows/'System32/WindowsPowerShell/v1.0'])
        name = 'silencer-gateway.exe' if os.name == 'nt' else 'silencer-gateway'
        log = (root/'gateway.log').open('wb')
        process = subprocess.Popen([str(bundle/name)], cwd=state, env=env, stdout=log, stderr=log)

        def request(method, path, body=None, headers=None):
            connection = http.client.HTTPConnection('127.0.0.1', port, timeout=3)
            try:
                connection.request(method, path, body, {'X-Silencer-Token':'release-smoke-fixture', **(headers or {})})
                response = connection.getresponse()
                return response.status, response.read()
            finally: connection.close()

        try:
            deadline = time.monotonic()+15
            while time.monotonic() < deadline:
                if process.poll() is not None: raise RuntimeError('packaged gateway exited before becoming ready')
                try:
                    if request('GET', '/api/auth/status')[0] == 200: break
                except OSError: pass
                time.sleep(0.1)
            else: raise RuntimeError('packaged gateway readiness timeout')
            code, data = request('GET', '/api/capabilities')
            capabilities = json.loads(data)
            ensure(code == 200 and capabilities['local'] and capabilities['protocol'] == 1, 'capability check failed')
            code, identity = request('GET', '/local-readyz')
            ensure(code == 200 and identity.decode() == entries[0]['sha256'], 'incorrect running bundle identity')
            original = b'{"text":"alice@example.com"}'
            code, restored = request('POST', '/api/capture/forward', original, {'Content-Type':'application/json', 'Authorization':'Bearer provider-fixture', 'X-Silencer-Capture-Protocol':'1', 'X-Silencer-Capture-Target':target+'/original?z=2&a=1'})
            ensure(code == 200 and restored == original, f'packaged mask/restore failed: HTTP {code}')
            path, headers, masked = received.get(timeout=2)
            ensure(path == '/original?z=2&a=1' and b'alice@example.com' not in masked and b'EMAIL_' in masked, 'original target or masking changed')
            ensure(headers.get('Authorization') == 'Bearer provider-fixture' and 'X-Silencer-Token' not in headers, 'credential separation failed')
            code, data = request('GET', '/api/local/setup/status')
            status = json.loads(data)
            ensure(code == 200 and status['checks']['component']['ok'] and not status['config']['enabled'] and not status['checks']['permissions']['ok'], 'unexpected capture activation or runtime failure')
            ensure(not list((state/'control.json.capture').glob('mitmproxy-ca*')), 'smoke check created capture CA')
            print(json.dumps({'platform':platform.system(), 'runtime':'12.2.3', 'native_gateway':True, 'mask_restore':True, 'original_target':True, 'provider_credentials':True, 'capture_enabled':False}))
        finally:
            process.terminate()
            try: process.wait(timeout=5)
            except subprocess.TimeoutExpired: process.kill(); process.wait()
            model.shutdown()
            model.server_close()
            model_thread.join(timeout=2)
            log.close()


if __name__ == '__main__': main()
