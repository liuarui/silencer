#!/usr/bin/env python3
"""Disposable local gateway test; never edits an existing service or trust store."""
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

root = Path(__file__).resolve().parents[2]
binary = Path(sys.argv[1]).resolve()
assert binary.is_file()
with tempfile.TemporaryDirectory(prefix='webchat-native-') as directory:
    data = Path(directory)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    address = 'http://127.0.0.1:' + str(port)
    token = secrets.token_hex(32)
    env = {k: v for k, v in os.environ.items() if not k.startswith(('SILENCER_', 'OTEL_'))}
    env.update(SILENCER_GATEWAY_ADDR='127.0.0.1:' + str(port), SILENCER_GATEWAY_TOKEN=token,
               SILENCER_CONTROL_CONFIG_FILE=str(data / 'control.json'), SILENCER_ENGINE='cpp',
               SILENCER_STORAGE_MODE='sqlite', SILENCER_SQLITE_PATH=str(data / 'test.db'),
               SILENCER_EXTENSION_DISABLED='0', SILENCER_LOCAL_MODE='0', SILENCER_REQUIRE_HTTPS='0',
               SILENCER_REQUIRE_UPSTREAM='0', SILENCER_TEST_URL=address, SILENCER_TEST_TOKEN=token,
               SILENCER_TEST_STATE=str(data / 'synthetic-state.json'))
    log = (data / 'gateway.log').open('wb')
    process = None
    try:
        for phase in ['seed', 'restart']:
            process = subprocess.Popen([str(binary)], cwd=data, env=env, stdout=log, stderr=log)
            for attempt in range(100):
                assert process.poll() is None, 'isolated gateway exited'
                try:
                    with urllib.request.urlopen(address + '/readyz', timeout=1) as response:
                        if response.status == 200:
                            break
                except OSError:
                    time.sleep(.1)
            else:
                raise RuntimeError('isolated gateway readiness timeout')
            subprocess.run(['node', str(root / 'extension/tests/native.mjs')], cwd=root,
                           env={**env, 'SILENCER_TEST_PHASE': phase}, check=True, timeout=120)
            process.terminate()
            process.wait(timeout=30)
            process = None
    finally:
        if process and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        log.close()
    print(json.dumps({'native_webchat_protocols': 'passed', 'restart_persistence': 'passed', 'scope_isolation': 'passed', 'existing_services_modified': False}))
