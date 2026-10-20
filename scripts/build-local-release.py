#!/usr/bin/env python3
"""Build a native, checksummed local bundle; never exports data or credentials.

Run natively on each target with its C++/PCRE2 toolchain. --runtime is a complete
mitmproxy 12.2.3 standalone distribution, including helpers. Linux uses the
hash-pinned build from build-linux-capture-runtime.py with actual metadata.
This does not upload, sign, install, or change host trust/network settings.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import struct
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
_probe_spec = importlib.util.spec_from_file_location('capture_runtime_check', Path(__file__).with_name('capture-runtime-check.py'))
_probe = importlib.util.module_from_spec(_probe_spec)
_probe_spec.loader.exec_module(_probe)


def run(args, **kwargs):
    subprocess.run([str(x) for x in args], check=True, **kwargs)


def check_macos_minimum(path):
    for command in subprocess.check_output(['otool', '-l', str(path)], text=True).split('Load command'):
        pattern = r'\bminos\s+(\d+(?:\.\d+)+)' if 'LC_BUILD_VERSION' in command else r'\bversion\s+(\d+(?:\.\d+)+)' if 'LC_VERSION_MIN_MACOSX' in command else None
        if pattern and (match := re.search(pattern, command)):
            minimum = tuple(int(part) for part in match[1].split('.')[:2])
            if minimum > (14, 0): raise RuntimeError(f'{path.name} requires macOS {match[1]}; build dependencies for macOS 14.0 or earlier')


def check_binary_architecture(path, system, arch):
    if system == 'darwin':
        architectures = subprocess.check_output(['lipo', '-archs', str(path)], text=True).split()
        if {'amd64': 'x86_64', 'arm64': 'arm64'}[arch] not in architectures:
            raise RuntimeError(f'{path.name} does not match the native architecture')
        return
    with Path(path).open('rb') as source:
        header = source.read(64)
        if system == 'linux':
            valid = len(header) >= 20 and header[:4] == b'\x7fELF' and header[4:6] == b'\x02\x01'
            machine = struct.unpack_from('<H', header, 18)[0] if valid else None
            expected = {'amd64': 62, 'arm64': 183}[arch]
        else:
            valid = len(header) == 64 and header[:2] == b'MZ'
            if valid:
                source.seek(struct.unpack_from('<I', header, 60)[0])
                pe = source.read(6)
                valid = len(pe) == 6 and pe[:4] == b'PE\0\0'
            machine = struct.unpack_from('<H', pe, 4)[0] if valid else None
            expected = {'amd64': 0x8664, 'arm64': 0xaa64}[arch]
    if not valid or machine != expected:
        raise RuntimeError(f'{Path(path).name} does not match {system}/{arch}')


def windows_imports(path):
    output = subprocess.check_output([os.environ.get('OBJDUMP', 'objdump'), '-p', str(path)], text=True)
    return re.findall(r'DLL Name:\s*(\S+)', output)


def copy_windows_dependencies(binaries, library_dir, search_dirs, system_dir, imports=windows_imports):
    pending, inspected = list(binaries), set()
    while pending:
        binary = pending.pop()
        if binary.name.lower() in inspected:
            continue
        inspected.add(binary.name.lower())
        check_binary_architecture(binary, 'windows', 'amd64')
        for name in imports(binary):
            if Path(name).name != name or '\\' in name:
                raise RuntimeError('Invalid DLL import name')
            if name.lower().startswith(('api-ms-win-', 'ext-ms-win-')) or (system_dir/name).is_file():
                continue
            candidates = [entry for directory in [library_dir, *search_dirs] if directory.is_dir()
                          for entry in directory.iterdir() if entry.name.lower() == name.lower() and entry.is_file()]
            if not candidates:
                raise RuntimeError(f'Missing required runtime DLL: {name}')
            destination = library_dir/name
            if candidates[0].resolve() != destination.resolve():
                shutil.copy2(candidates[0], destination)
            pending.append(destination)


def copy_linux_pcre_dependency(binary, library_dir):
    output = subprocess.check_output(['ldd', str(binary)], text=True)
    if 'not found' in output:
        raise RuntimeError('Native binary has unresolved shared libraries')
    for name, source in re.findall(r'^\s*(libpcre2-8\.so\S*)\s+=>\s+(\S+)', output, re.M):
        shutil.copy2(source, library_dir/name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--dependency', type=Path, action='append', default=[])
    parser.add_argument('--pcre2-license', type=Path)
    parser.add_argument('--license', type=Path, action='append', default=[])
    args = parser.parse_args()
    system = {'Darwin': 'darwin', 'Linux': 'linux', 'Windows': 'windows'}[platform.system()]
    arch = {'aarch64': 'arm64', 'arm64': 'arm64', 'x86_64': 'amd64', 'AMD64': 'amd64'}[platform.machine()]
    if system != 'darwin' and arch != 'amd64':
        parser.error('Windows/Linux capture releases currently target x64')
    if subprocess.check_output(['go', 'env', 'GOOS', 'GOARCH'], text=True).split() != [system, arch]:
        parser.error('Go target must match the native build host; clear cross-compilation settings')
    executable = 'mitmdump.exe' if system == 'windows' else 'mitmdump'
    runtime_root = args.runtime.resolve()
    mac_app = runtime_root if runtime_root.suffix == '.app' else runtime_root/'mitmproxy.app'
    runtime_executable = mac_app/'Contents/MacOS/mitmdump' if system == 'darwin' else runtime_root/executable
    if not runtime_executable.is_file():
        parser.error(f'--runtime must contain {executable} and all vendor helper files')
    check_binary_architecture(runtime_executable, system, arch)
    version = subprocess.check_output([str(runtime_executable), '--version'], text=True)
    if 'Mitmproxy: 12.2.3' not in version:
        parser.error('mitmproxy 12.2.3 is required')
    if system == 'darwin': check_macos_minimum(runtime_executable)
    runtime_manifest = _probe.inspect_runtime(runtime_executable, system, runtime_root) if system in ('linux', 'windows') else None
    args.out.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='silencer-local-build-') as temporary:
        bundle = Path(temporary)
        if runtime_manifest:
            (bundle/'capture-runtime-manifest.json').write_text(json.dumps(runtime_manifest, indent=2)+'\n', encoding='utf-8')
        lib = bundle / 'lib'
        lib.mkdir()
        flags = shlex.split(subprocess.check_output(['pkg-config', '--cflags', '--libs', 'libpcre2-8'], text=True))
        prefix = Path(subprocess.check_output(['pkg-config', '--variable=prefix', 'libpcre2-8'], text=True).strip())
        license_candidates = [args.pcre2_license] if args.pcre2_license else [prefix/'share/licenses/pcre2/LICENCE', prefix/'share/licenses/pcre2/LICENSE', prefix/'share/doc/pcre2/LICENCE.md', prefix/'share/doc/pcre2/LICENCE', prefix/'LICENCE']
        pcre_license = next((path for path in license_candidates if path and path.is_file()), None)
        if pcre_license is None: parser.error('provide --pcre2-license with the PCRE2 LICENCE file')
        (bundle/'licenses').mkdir()
        shutil.copy2(pcre_license, bundle/'licenses/PCRE2-LICENCE')
        for notice in args.license: shutil.copy2(notice, bundle/'licenses'/notice.name)
        for notice in (ROOT/'licenses').iterdir():
            if notice.is_file(): shutil.copy2(notice, bundle/'licenses'/notice.name)
        font_notice = ROOT/'frontend/src/assets/fonts/OFL-Inter.txt'
        if font_notice.is_file(): shutil.copy2(font_notice, bundle/'licenses'/font_notice.name)
        for notice in ['LICENSE', 'THIRD_PARTY_NOTICES.md']: shutil.copy2(ROOT/notice, bundle/notice)
        library = {'darwin': 'libsilencer_engine.dylib', 'linux': 'libsilencer_engine.so', 'windows': 'silencer_engine.dll'}[system]
        compiler = os.environ.get('CXX', 'clang++' if system == 'darwin' else 'g++')
        link = ['-dynamiclib', '-Wl,-install_name,@rpath/'+library] if system == 'darwin' else ['-shared']
        if system == 'darwin': link.append('-mmacosx-version-min=14.0')
        if system != 'windows':
            link.append('-fPIC')
        else:
            link.extend(['-Wl,--export-all-symbols', '-lws2_32'])
        run([compiler, '-std=c++17', '-O2', ROOT/'engine/cpp/engine.cpp', *flags, *link, '-o', lib/library])
        if system == 'windows':
            # MinGW needs an import library as well as the runtime DLL.
            run([compiler, '-std=c++17', '-O2', ROOT/'engine/cpp/engine.cpp', *flags, '-shared', '-Wl,--export-all-symbols', '-Wl,--out-implib,'+str(lib/'libsilencer_engine.dll.a'), '-lws2_32', '-o', lib/library])
        if system == 'linux':
            copy_linux_pcre_dependency(lib/library, lib)
        if system == 'windows':
            prefix = Path(subprocess.check_output(['pkg-config', '--variable=prefix', 'libpcre2-8'], text=True).strip())
            for pattern in ['*pcre2-8*.dll', 'libstdc++-6.dll', 'libgcc_s_*-1.dll', 'libwinpthread-1.dll']:
                for dependency in (prefix/'bin').glob(pattern): shutil.copy2(dependency, lib/dependency.name)
        for dependency in args.dependency:
            shutil.copy2(dependency, lib/dependency.name)
        if system == 'darwin':
            pending = [lib/library]
            inspected = set()
            while pending:
                current = pending.pop()
                if current in inspected: continue
                inspected.add(current)
                dependencies = subprocess.check_output(['otool', '-L', str(current)], text=True).splitlines()[2:]
                for line in dependencies:
                    path = line.strip().split(' (', 1)[0]
                    if path.startswith('/') and not path.startswith(('/usr/lib/', '/System/Library/')):
                        source = Path(path)
                        destination = lib/source.name
                        if not destination.exists(): shutil.copy2(source, destination)
                        run(['install_name_tool', '-change', path, '@loader_path/'+source.name, current])
                        pending.append(destination)
            for dylib in inspected:
                check_macos_minimum(dylib)
                run(['codesign', '--force', '--sign', '-', dylib])
        env = os.environ.copy()
        env['CGO_ENABLED'] = '1'
        if system == 'darwin':
            env['MACOSX_DEPLOYMENT_TARGET'] = '14.0'
            env['CGO_CFLAGS'] = env.get('CGO_CFLAGS', '')+' -mmacosx-version-min=14.0'
        env['CGO_LDFLAGS'] = '-L'+shlex.quote(str(lib))
        if system != 'windows':
            env['CGO_LDFLAGS'] += ' -Wl,-rpath,' + ('@executable_path/lib' if system == 'darwin' else '$ORIGIN/lib')
        if system == 'darwin': env['CGO_LDFLAGS'] += ' -mmacosx-version-min=14.0'
        name = 'silencer-gateway.exe' if system == 'windows' else 'silencer-gateway'
        run(['go', 'build', '-tags', 'silencer_engine', '-trimpath', '-o', bundle/name, './cmd/gateway'], cwd=ROOT/'gateway', env=env)
        check_binary_architecture(lib/library, system, arch)
        check_binary_architecture(bundle/name, system, arch)
        if system == 'windows':
            compiler_path = Path(shutil.which(compiler) or compiler).resolve()
            copy_windows_dependencies([bundle/name, lib/library], lib, [prefix/'bin', compiler_path.parent, *(path.parent for path in args.dependency)], Path(os.environ['SystemRoot'])/'System32')
        if system == 'darwin': check_macos_minimum(bundle/name)
        run([shutil.which('npm') or 'npm', 'run', 'build'], cwd=ROOT/'frontend')
        shutil.copytree(ROOT/'frontend/dist', bundle/'web_dist')
        if system == 'darwin':
            shutil.copytree(mac_app, bundle/'runtime/mitmproxy.app', symlinks=True)
            shim = bundle/'runtime/mitmdump'
            shim.write_text('#!/bin/sh\nset -eu\nunset LD_LIBRARY_PATH DYLD_LIBRARY_PATH\nruntime_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)\nexec "$runtime_dir/mitmproxy.app/Contents/MacOS/mitmdump" "$@"\n', encoding='utf-8')
            shim.chmod(0o755)
        else:
            shutil.copytree(runtime_root, bundle/'runtime', symlinks=True)
        (bundle/'capture-runtime-version').write_text('12.2.3\n', encoding='utf-8')
        suffix = '.zip' if system == 'windows' else '.tar.gz'
        filename = f'silencer-local-{system}-{arch}{suffix}'
        archive = args.out/filename
        if system == 'windows':
            with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as target:
                for file in bundle.rglob('*'):
                    if file.is_file(): target.write(file, file.relative_to(bundle))
        else:
            with tarfile.open(archive, 'w:gz') as target:
                for item in bundle.iterdir(): target.add(item, arcname=item.name)
        manifest_path = args.out/'manifest.json'
        manifest = json.loads(manifest_path.read_text()) if manifest_path.exists() else {'protocol': 1, 'platforms': {}}
        manifest['platforms'][f'{system}-{arch}'] = {'file': filename, 'sha256': hashlib.sha256(archive.read_bytes()).hexdigest()}
        manifest_path.write_text(json.dumps(manifest, indent=2)+'\n', encoding='utf-8')
        (args.out/'SHA256SUMS').write_text(''.join(item['sha256']+'  '+item['file']+'\n' for item in manifest['platforms'].values()), encoding='utf-8')
        print(archive)


if __name__ == '__main__':
    main()
