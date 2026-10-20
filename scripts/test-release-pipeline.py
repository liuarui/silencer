#!/usr/bin/env python3
"""Release gates reject incomplete or tampered native/image artifacts."""
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


def module(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name+'.py'))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


assembly = module('assemble-release')
images = module('check-image-index')


class ReleaseTests(unittest.TestCase):
    def test_native_missing_and_tampered(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for platform in assembly.PLATFORMS:
                path = root/('native-'+platform)
                path.mkdir()
                name = 'silencer-local-'+platform+('.zip' if platform.startswith('windows-') else '.tar.gz')
                (path/name).write_bytes(b'fixture')
                (path/'manifest.json').write_text(json.dumps({'protocol': 1, 'platforms': {platform: {'file': name, 'sha256': hashlib.sha256(b'fixture').hexdigest()}}}))
            assembly.assemble(root, root/'release')
            self.assertEqual(set(json.loads((root/'release/manifest.json').read_text())['platforms']), assembly.PLATFORMS)
            archive = next((root/'native-windows-amd64').glob('*.zip'))
            archive.write_bytes(b'tampered')
            with self.assertRaises(ValueError): assembly.assemble(root, root/'tampered')
            (root/'native-windows-amd64/manifest.json').unlink()
            with self.assertRaises(ValueError): assembly.assemble(root, root/'missing')

    def test_image_digest_and_platform(self):
        document = {'manifests': [{'platform': {'os': 'linux', 'architecture': arch}, 'digest': digest} for arch, digest in [('amd64', 'sha256:a'), ('arm64', 'sha256:b')]]}
        images.verify(document, 'repo@sha256:a', 'repo@sha256:b')
        with self.assertRaises(ValueError): images.verify(document, 'repo@sha256:c', 'repo@sha256:b')
        document['manifests'].append(document['manifests'][0])
        with self.assertRaises(ValueError): images.verify(document, 'repo@sha256:a', 'repo@sha256:b')


if __name__ == '__main__': unittest.main()
