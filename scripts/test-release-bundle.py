#!/usr/bin/env python3
"""Verify release architecture and dependency checks without executing fixtures."""
import importlib.util
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('local_release', Path(__file__).with_name('build-local-release.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


def pe(path, machine=0x8664):
    header = bytearray(64)
    header[:2] = b'MZ'
    struct.pack_into('<I', header, 60, 64)
    path.write_bytes(header + b'PE\0\0' + struct.pack('<H', machine))
    return path


class ReleaseBundleTests(unittest.TestCase):
    def test_helper_probe_rejects_missing_invalid_and_nonexecutable_helpers(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            helper = root/'redirector.exe'
            with self.assertRaises(RuntimeError): release._probe.helper_info(helper, 'windows')
            pe(helper)
            self.assertEqual(release._probe.helper_info(helper, 'windows')['size'], helper.stat().st_size)
            pe(helper, 0xaa64)
            with self.assertRaises(RuntimeError): release._probe.helper_info(helper, 'windows')
            linux = root/'linux-helper'
            linux.write_bytes(b'not an executable')
            with self.assertRaises(RuntimeError): release._probe.helper_info(linux, 'linux')

    def test_metadata_alone_cannot_replace_a_helper_probe(self):
        with patch.object(release._probe.subprocess, 'run') as run:
            run.return_value.returncode = 0
            run.return_value.stdout = 'Mitmproxy: 12.2.3\n'
            with self.assertRaisesRegex(RuntimeError, 'helper verification failed'):
                release._probe.inspect_runtime('/unused/mitmdump', 'linux', '/unused')

    def test_rejects_mislabeled_elf_and_pe(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            windows = pe(root/'gateway.exe')
            release.check_binary_architecture(windows, 'windows', 'amd64')
            with self.assertRaises(RuntimeError):
                release.check_binary_architecture(windows, 'windows', 'arm64')
            linux = root/'gateway'
            header = bytearray(64)
            header[:6] = b'\x7fELF\x02\x01'
            struct.pack_into('<H', header, 18, 62)
            linux.write_bytes(header)
            release.check_binary_architecture(linux, 'linux', 'amd64')
            with self.assertRaises(RuntimeError):
                release.check_binary_architecture(linux, 'linux', 'arm64')
            with self.assertRaises(RuntimeError):
                release.check_binary_architecture(linux, 'windows', 'amd64')

    def test_copies_transitive_runtime_dlls_and_rejects_missing_dependency(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            lib, toolchain, system = [root/name for name in ['lib', 'toolchain', 'system']]
            for directory in [lib, toolchain, system]:
                directory.mkdir()
            gateway = pe(root/'gateway.exe')
            pe(toolchain/'pcre.dll')
            pe(toolchain/'runtime.dll')
            pe(system/'KERNEL32.dll')
            graph = {'gateway.exe': ['KERNEL32.dll', 'pcre.dll'], 'pcre.dll': ['runtime.dll'], 'runtime.dll': []}
            release.copy_windows_dependencies([gateway], lib, [toolchain], system, lambda path: graph[path.name])
            self.assertEqual({path.name for path in lib.iterdir()}, {'pcre.dll', 'runtime.dll'})
            with self.assertRaisesRegex(RuntimeError, 'Missing required runtime DLL'):
                release.copy_windows_dependencies([gateway], lib, [toolchain], system, lambda path: ['missing.dll'])
            with self.assertRaisesRegex(RuntimeError, 'Invalid DLL import name'):
                release.copy_windows_dependencies([gateway], lib, [toolchain], system, lambda path: ['../escaped.dll'])


if __name__ == '__main__':
    unittest.main()
