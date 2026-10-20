#!/usr/bin/env python3
"""Require exactly the two tested native image digests in a published index."""
import json
from pathlib import Path
import sys


def verify(document, amd64, arm64):
    expected = {'amd64': amd64.split('@')[-1], 'arm64': arm64.split('@')[-1]}
    entries = document.get('manifests', [])
    actual = {entry['platform']['architecture']: entry['digest'] for entry in entries if entry['platform'].get('os') == 'linux'}
    if len(entries) != 2 or actual != expected:
        raise ValueError('Published image platforms or digests differ from verified candidates')


if __name__ == '__main__':
    verify(json.loads(Path(sys.argv[1]).read_text()), sys.argv[2], sys.argv[3])
    print('Published linux/amd64 and linux/arm64 digests verified')
