#!/usr/bin/env python3
"""Fetch pinned official native dependencies; never install host components."""
import argparse
import hashlib
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tarfile
import urllib.request
import zipfile

ASSETS = {
    'darwin-arm64': ('mitmproxy-12.2.3-macos-arm64.tar.gz', '0a09ee3b82569e8985aff8186e4792618b8e5d0c766098db093d09a87d4b013a'),
    'darwin-amd64': ('mitmproxy-12.2.3-macos-x86_64.tar.gz', '7998187f5a0d399ab796af4523d3ad830ebe690726a41bc3e1df47a8e477a641'),
    'windows-amd64': ('mitmproxy-12.2.3-windows-x86_64.zip', '04a01ea95ae96df75058a893e774957d294e69012dab1f4e256ce2b0c6725483'),
}
PCRE_SHA = 'c08ae2388ef333e8403e670ad70c0a11f1eed021fd88308d7e02f596fcd9dc16'


def fetch(url, destination, expected):
    if not destination.exists():
        with urllib.request.urlopen(url, timeout=120) as source, destination.open('wb') as target:
            shutil.copyfileobj(source, target)
    if hashlib.sha256(destination.read_bytes()).hexdigest() != expected:
        raise RuntimeError(f'Checksum mismatch: {destination.name}')


def extract(archive, destination):
    destination.mkdir(parents=True, exist_ok=True)
    if archive.suffix == '.zip':
        with zipfile.ZipFile(archive) as source:
            for entry in source.infolist():
                if not (destination/entry.filename).resolve().is_relative_to(destination.resolve()):
                    raise RuntimeError('Archive path escapes destination')
            source.extractall(destination)
    else:
        with tarfile.open(archive) as source:
            source.extractall(destination, filter='data')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=True)
    system = platform.system().lower()
    arch = {'arm64': 'arm64', 'x86_64': 'amd64', 'AMD64': 'amd64'}[platform.machine()]
    name, digest = ASSETS[f'{system}-{arch}']
    archive = root/name
    fetch('https://downloads.mitmproxy.org/12.2.3/'+name, archive, digest)
    extract(archive, root/'runtime')
    if system == 'darwin':
        source = root/'pcre2-10.47.tar.gz'
        fetch('https://github.com/PCRE2Project/pcre2/releases/download/pcre2-10.47/pcre2-10.47.tar.gz', source, PCRE_SHA)
        extract(source, root/'source')
        prefix = root/'pcre'
        subprocess.run(['cmake', '-S', str(root/'source/pcre2-10.47'), '-B', str(root/'pcre-build'),
                        '-DCMAKE_BUILD_TYPE=Release', '-DCMAKE_INSTALL_PREFIX='+str(prefix), '-DCMAKE_INSTALL_LIBDIR=lib',
                        '-DCMAKE_OSX_DEPLOYMENT_TARGET=14.0', '-DCMAKE_OSX_ARCHITECTURES='+platform.machine(),
                        '-DCMAKE_POSITION_INDEPENDENT_CODE=ON', '-DBUILD_SHARED_LIBS=OFF',
                        '-DPCRE2_BUILD_PCRE2GREP=OFF', '-DPCRE2_BUILD_TESTS=OFF', '-DPCRE2_SUPPORT_JIT=OFF',
                        '-DPCRE2_BUILD_PCRE2_8=ON', '-DPCRE2_BUILD_PCRE2_16=OFF', '-DPCRE2_BUILD_PCRE2_32=OFF'], check=True)
        subprocess.run(['cmake', '--build', str(root/'pcre-build'), '--parallel', '3'], check=True)
        subprocess.run(['cmake', '--install', str(root/'pcre-build')], check=True)
        if os.environ.get('GITHUB_ENV'):
            with open(os.environ['GITHUB_ENV'], 'a', encoding='utf-8') as target:
                target.write('PKG_CONFIG_PATH='+str(prefix/'lib/pkgconfig')+'\n')


if __name__ == '__main__':
    main()
