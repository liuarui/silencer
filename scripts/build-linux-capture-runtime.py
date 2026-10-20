#!/usr/bin/env python3
"""Freeze a hash-pinned Linux x64 runtime without capture or host installation.

Run with CPython 3.13 on Linux x64 (glibc 2.39 or older for Ubuntu 24.04).
The upstream PyInstaller hooks preserve the real redirector executable. Package
metadata comes from the installed wheels, never from hand-written version files.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tempfile
import venv

ROOT = Path(__file__).resolve().parents[1]
VERSIONS = {'mitmproxy': '12.2.3', 'mitmproxy-rs': '0.12.11',
            'mitmproxy-linux': '0.12.11', 'pyinstaller': '6.19.0'}


def run(*args, **kwargs):
    return subprocess.run(*args, check=True, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True,
                        help='new output directory; existing output is never replaced')
    args = parser.parse_args()
    if platform.system() != 'Linux' or platform.machine() not in ('x86_64', 'AMD64'):
        parser.error('build natively on Linux x64')
    if sys.implementation.name != 'cpython' or sys.version_info[:2] != (3, 13):
        parser.error('the wheel lock targets CPython 3.13')
    libc, libc_version = platform.libc_ver()
    if libc != 'glibc' or tuple(int(x) for x in libc_version.split('.')[:2]) > (2, 39):
        parser.error('build on glibc 2.39 or older for Ubuntu 24.04 compatibility')
    if shutil.which('objdump') is None:
        parser.error('PyInstaller requires objdump (binutils)')
    output = args.out.resolve()
    if output.exists():
        parser.error('--out must be a new directory')
    output.parent.mkdir(parents=True, exist_ok=True)
    lock = ROOT/'scripts/linux-capture-runtime.lock'
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(('PYTHON', 'PIP_', 'SILENCER_'))
           and key not in ('LD_LIBRARY_PATH', 'DYLD_LIBRARY_PATH')}
    with tempfile.TemporaryDirectory(prefix='silencer-linux-runtime-') as directory:
        work = Path(directory)
        venv.EnvBuilder(with_pip=True).create(work/'venv')
        python = work/'venv/bin/python'
        run([str(python), '-m', 'pip', '--isolated', 'install', '--no-cache-dir',
             '--require-hashes', '--only-binary=:all:', '--index-url', 'https://pypi.org/simple',
             '--report', str(work/'install-report.json'), '-r', str(lock)], env=env)
        probe_source = (
            'import hashlib,importlib.util,json,os\n'
            'from importlib.metadata import version\n'
            'import mitmproxy_linux\n'
            'def load(loader):\n'
            '    helper=mitmproxy_linux.executable_path()\n'
            '    assert helper.is_file() and os.access(helper,os.X_OK)\n'
            f'    spec=importlib.util.spec_from_file_location("silencer_probe",{str(ROOT/"gateway/internal/gateway/capture_assets/addon.py")!r})\n'
            '    addon=importlib.util.module_from_spec(spec); spec.loader.exec_module(addon)\n'
            '    addon.check_runtime_packages()\n'
            '    print("SILENCER_RUNTIME_PROBE="+json.dumps({"versions":'
            '{name:version(name) for name in ["mitmproxy","mitmproxy-rs","mitmproxy-linux"]},'
            '"helper_sha256":hashlib.sha256(helper.read_bytes()).hexdigest()}),flush=True)\n'
            '    raise SystemExit(0)\n'
        )
        inspection = run([str(python), '-c',
            'import hashlib,json\nfrom importlib.metadata import version\n'
            'import mitmproxy_linux\n'
            f'assert {{name:version(name) for name in {list(VERSIONS)!r}}} == {VERSIONS!r}\n'
            'print(hashlib.sha256(mitmproxy_linux.executable_path().read_bytes()).hexdigest())'],
            env=env, capture_output=True, text=True).stdout.strip()
        entry = work/'entrypoint.py'
        entry.write_text('from mitmproxy.tools.main import mitmdump\nmitmdump()\n', encoding='utf-8')
        command = [str(python), '-m', 'PyInstaller', '--clean', '--noconfirm',
                   '--onefile', '--console', '--name', 'mitmdump', '--python-option', 'u',
                   '--exclude-module', 'mitmproxy.tools.web',
                   '--exclude-module', 'mitmproxy.tools.console',
                   '--distpath', str(work/'dist'), '--workpath', str(work/'build'),
                   '--specpath', str(work)]
        for name in ('mitmproxy', 'mitmproxy-rs', 'mitmproxy-linux'):
            command += ['--copy-metadata', name]
        run([*command, str(entry)], env=env)
        executable = work/'dist/mitmdump'
        probe = work/'probe.py'
        probe.write_text(probe_source, encoding='utf-8')
        result = run([str(executable), '-q', '--mode', 'regular@127.0.0.1:0',
                      '--set', 'confdir='+str(work/'probe-state'), '-s', str(probe)],
                     env=env, capture_output=True, text=True, timeout=45)
        prefix = 'SILENCER_RUNTIME_PROBE='
        records = [json.loads(line[len(prefix):]) for line in result.stdout.splitlines()
                   if line.startswith(prefix)]
        expected = {name: VERSIONS[name] for name in VERSIONS if name != 'pyinstaller'}
        if len(records) != 1 or records[0]['versions'] != expected or records[0]['helper_sha256'] != inspection:
            raise RuntimeError('Frozen runtime metadata/helper verification failed')
        # Collect installed dependency notices, including Python's own license.
        notices = work/'dist/licenses'
        run([str(python), '-c',
            'from importlib.metadata import distributions\nfrom pathlib import Path\n'
            'import re,shutil,sys\n'
            'dest=Path(sys.argv[1]); dest.mkdir()\n'
            'for dist in distributions():\n'
            '    name=re.sub(r"[^a-zA-Z0-9_.-]","_",dist.metadata["Name"])\n'
            '    for item in dist.files or []:\n'
            '        if item.name.lower().startswith(("license","licence","copying","notice")):\n'
            '            source=Path(dist.locate_file(item))\n'
            '            if source.is_file():\n'
            '                if item.is_absolute() or ".." in item.parts: raise RuntimeError("Unsafe license path")\n'
            '                target=dest/name/str(item); target.parent.mkdir(parents=True,exist_ok=True)\n'
            '                shutil.copy2(source,target)\n'
            'for source in (Path(sys.base_prefix)/"LICENSE.txt",Path(sys.base_prefix)/"lib/python3.13/LICENSE.txt"):\n'
            '    if source.is_file(): shutil.copy2(source,dest/"CPython-LICENSE.txt"); break\n'
            'else: raise RuntimeError("CPython license missing")\n', str(notices)], env=env)
        shutil.copy2(lock, work/'dist/requirements.lock')
        shutil.copy2(work/'install-report.json', work/'dist/install-report.json')
        manifest = {'platform': 'linux-amd64', 'python': platform.python_version(),
                    'glibc': libc_version, **records[0],
                    'mitmdump_sha256': hashlib.sha256(executable.read_bytes()).hexdigest(),
                    'requirements_sha256': hashlib.sha256(lock.read_bytes()).hexdigest()}
        (work/'dist/build-manifest.json').write_text(json.dumps(manifest, indent=2)+'\n', encoding='utf-8')
        shutil.copytree(work/'dist', output)
        print(output)


if __name__ == '__main__':
    main()
