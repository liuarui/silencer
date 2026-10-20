#!/usr/bin/env python3
"""Build and test the capture gateway natively without changing host settings."""
import os
from pathlib import Path
import platform
import shlex
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
system = platform.system()
with tempfile.TemporaryDirectory(prefix='silencer-native-check-') as directory:
    build = Path(directory)
    flags = shlex.split(subprocess.check_output(['pkg-config', '--cflags', '--libs', 'libpcre2-8'], text=True))
    name = {'Darwin': 'libsilencer_engine.dylib', 'Linux': 'libsilencer_engine.so', 'Windows': 'silencer_engine.dll'}[system]
    compiler = os.environ.get('CXX', 'clang++' if system == 'Darwin' else 'g++')
    link = ['-dynamiclib'] if system == 'Darwin' else ['-shared']
    if system == 'Windows':
        link += ['-Wl,--export-all-symbols', '-Wl,--out-implib,'+str(build/'libsilencer_engine.dll.a'), '-lws2_32']
    else:
        link.append('-fPIC')
    subprocess.run([compiler, '-std=c++17', '-O2', '-Wall', '-Wextra', '-Werror', str(root/'engine/cpp/engine.cpp'), *flags, *link, '-o', str(build/name)], check=True)
    env = os.environ.copy()
    env['CGO_ENABLED'] = '1'
    env['CGO_LDFLAGS'] = '-L'+str(build)
    if system != 'Windows': env['CGO_LDFLAGS'] += ' -Wl,-rpath,'+str(build)
    env['PATH'] = str(build)+os.pathsep+env['PATH']
    env['LD_LIBRARY_PATH'] = str(build)
    env['DYLD_LIBRARY_PATH'] = str(build)
    subprocess.run(['go', 'build', '-tags', 'silencer_engine', '-o', str(build/('silencer-gateway.exe' if system == 'Windows' else 'silencer-gateway')), './cmd/gateway'], cwd=root/'gateway', env=env, check=True)
    subprocess.run(['go', 'test', '-tags', 'silencer_engine', './...', '-count=1'], cwd=root/'gateway', env=env, check=True)
