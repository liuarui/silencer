#!/usr/bin/env python3
"""Verify all native artifacts and assemble the public download contract."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil

ROOT = Path(__file__).resolve().parents[1]
PLATFORMS = {'linux-amd64', 'windows-amd64', 'darwin-amd64', 'darwin-arm64'}


def assemble(source, output):
    manifest = {'protocol': 1, 'platforms': {}}
    output.mkdir(parents=True, exist_ok=True)
    for path in sorted(source.glob('native-*/manifest.json')):
        document = json.loads(path.read_text())
        if document.get('protocol') != 1:
            raise ValueError('Unexpected native protocol')
        for platform, entry in document['platforms'].items():
            suffix = '.zip' if platform.startswith('windows-') else '.tar.gz'
            filename = 'silencer-local-'+platform+suffix
            if platform not in PLATFORMS or platform in manifest['platforms'] or entry['file'] != filename:
                raise ValueError('Unexpected or duplicate native platform')
            archive = path.parent/filename
            if hashlib.sha256(archive.read_bytes()).hexdigest() != entry['sha256']:
                raise ValueError('Native artifact checksum mismatch')
            shutil.copy2(archive, output/filename)
            manifest['platforms'][platform] = entry
    if set(manifest['platforms']) != PLATFORMS:
        raise ValueError('All four native packages are required')
    (output/'manifest.json').write_text(json.dumps(manifest, indent=2)+'\n')
    for name in ('install.sh', 'install.ps1', 'uninstall.sh', 'uninstall.ps1'):
        shutil.copy2(ROOT/'gateway/internal/gateway/capture_assets'/name, output/name)
    files = sorted(path for path in output.iterdir() if path.is_file() and path.name != 'SHA256SUMS')
    (output/'SHA256SUMS').write_text(''.join(hashlib.sha256(path.read_bytes()).hexdigest()+'  '+path.name+'\n' for path in files))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source', type=Path)
    parser.add_argument('output', type=Path)
    args = parser.parse_args()
    assemble(args.source, args.output)
