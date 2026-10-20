#!/usr/bin/env python3
"""Check reviewed public source paths and the exact sponsor footer."""
import argparse
from pathlib import Path, PurePosixPath
import subprocess

ROOT = Path(__file__).resolve().parents[1]
SPONSORS = '## Sponsors\n\nSponsored by [ForZTN](https://sponsorship.forztn.com/github.com/liuarui/silencer).\n'
PRIVATE = ('extension/', '.private/', 'frontend/src/private/', 'docs/verification/', '.claude/', '.codex/', '.pi/')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--staged', action='store_true', help='Inspect index contents before committing')
    args = parser.parse_args()
    def contents(path):
        if args.staged:
            return subprocess.check_output(['git', 'show', ':'+path], cwd=ROOT).decode()
        return (ROOT/path).read_text()
    paths = [line for line in contents('scripts/public-release-files.txt').splitlines() if line and not line.startswith('#')]
    if len(paths) != len(set(paths)):
        raise SystemExit('Duplicate reviewed public path')
    tracked = set(subprocess.check_output(['git', 'ls-files', '-z'], cwd=ROOT).decode().split('\0'))-{''}
    for path in paths:
        parsed = PurePosixPath(path)
        if parsed.is_absolute() or '..' in parsed.parts or path.startswith(PRIVATE) or 'BROWSER_EXTENSION' in path:
            raise SystemExit('Invalid public path: '+path)
        if not (ROOT/path).is_file() or (ROOT/path).is_symlink():
            raise SystemExit('Missing or symlinked public file: '+path)
    unexpected = tracked-set(paths)
    if unexpected:
        raise SystemExit('Unreviewed tracked paths: '+', '.join(sorted(unexpected)))
    for path in ('README.md', 'README_EN.md'):
        if not contents(path).endswith(SPONSORS):
            raise SystemExit('Sponsor footer changed: '+path)
    print(f'Public source boundary OK: {len(paths)} reviewed files; sponsor footers intact')


if __name__ == '__main__':
    main()
