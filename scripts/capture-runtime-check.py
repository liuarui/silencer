"""Read-only helper integrity probe, also loaded inside the frozen runtime."""
import hashlib
import json
import os
from pathlib import Path
import platform
import struct
import subprocess
import tempfile

PREFIX = 'SILENCER_RUNTIME_HELPERS='


def helper_info(path, system):
    path = Path(path)
    if not path.is_file() or (system == 'linux' and not os.access(path, os.X_OK)):
        raise RuntimeError('Capture helper is missing or not executable')
    with path.open('rb') as source:
        header = source.read(64)
        if system == 'linux':
            valid = len(header) == 64 and header[:6] == b'\x7fELF\x02\x01' and struct.unpack_from('<H', header, 18)[0] == 62
        else:
            valid = len(header) == 64 and header[:2] == b'MZ'
            if valid:
                source.seek(struct.unpack_from('<I', header, 60)[0])
                pe = source.read(6)
                valid = len(pe) == 6 and pe[:4] == b'PE\0\0' and struct.unpack_from('<H', pe, 4)[0] == 0x8664
        if not valid:
            raise RuntimeError('Capture helper architecture is invalid')
    return {'sha256': hashlib.sha256(path.read_bytes()).hexdigest(), 'size': path.stat().st_size}


def load(loader):
    # Invoked by mitmdump in ordinary proxy mode; no helper is launched.
    from mitmproxy.version import VERSION
    system = platform.system().lower()
    packages = {}
    if system == 'linux':
        from importlib.metadata import version
        import mitmproxy_linux
        packages = {name: version(name) for name in ('mitmproxy-rs', 'mitmproxy-linux')}
        files = [mitmproxy_linux.executable_path()]
    elif system == 'windows':
        import mitmproxy_windows
        executable = mitmproxy_windows.executable_path()
        files = [executable, executable.parent/'WinDivert.dll', executable.parent/'WinDivert64.sys']
    else:
        raise RuntimeError('Unsupported helper probe platform')
    record = {'system': system, 'mitmproxy': VERSION, 'packages': packages,
              'helpers': {path.name: helper_info(path, system) for path in files}}
    print(PREFIX+json.dumps(record, sort_keys=True), flush=True)
    raise SystemExit(0)


def inspect_runtime(executable, system, runtime_root):
    executable, runtime_root = Path(executable), Path(runtime_root)
    with tempfile.TemporaryDirectory(prefix='silencer-helper-probe-') as directory:
        result = subprocess.run([str(executable), '-q', '--mode', 'regular@127.0.0.1:0',
            '--set', 'confdir='+directory, '-s', str(Path(__file__).resolve())],
            capture_output=True, text=True, timeout=30)
    records = [json.loads(line[len(PREFIX):]) for line in result.stdout.splitlines() if line.startswith(PREFIX)]
    if result.returncode != 0 or len(records) != 1:
        raise RuntimeError('Standalone capture helper verification failed')
    record = records[0]
    expected = {'mitmproxy-linux-redirector'} if system == 'linux' else {'windows-redirector.exe', 'WinDivert.dll', 'WinDivert64.sys'}
    if record['system'] != system or record['mitmproxy'] != '12.2.3' or set(record['helpers']) != expected:
        raise RuntimeError('Unexpected standalone capture helper inventory')
    if system == 'linux':
        if record['packages'] != {'mitmproxy-rs': '0.12.11', 'mitmproxy-linux': '0.12.11'}:
            raise RuntimeError('Linux backend packages must both be 0.12.11')
        manifest = json.loads((runtime_root/'build-manifest.json').read_text())
        if manifest['mitmdump_sha256'] != hashlib.sha256(executable.read_bytes()).hexdigest() or manifest['helper_sha256'] != record['helpers']['mitmproxy-linux-redirector']['sha256']:
            raise RuntimeError('Linux runtime differs from its verified build manifest')
    return record
