#!/usr/bin/env python3
"""Exercise the real Unix installer without host service/network/trust changes."""
import hashlib
import base64
import io
import os
from pathlib import Path
import plistlib
import signal
import shlex
import subprocess
import tarfile
import tempfile
import time
import unittest

INSTALLER = Path(__file__).resolve().parents[1]/'gateway/internal/gateway/capture_assets/install.sh'
UNINSTALLER = INSTALLER.with_name('uninstall.sh')


class InstallerLifecycleTests(unittest.TestCase):
    def test_linux_uninstall_requires_positive_stop_confirmation(self):
        for failure in ['query', 'active', 'process']:
            with self.subTest(failure=failure):
                root = self.sandbox('Linux')
                self.release(root)
                self.run_installer(root)
                if failure == 'query':
                    (root/'fail-service').write_text('-p ActiveState --value')
                elif failure == 'active':
                    (root/'service-active-state').write_text('deactivating')
                else:
                    (root/'service-processes').write_text(str(root/'data/versions/owned/runtime/mitmdump')+'\n')
                self.run_installer(root, '--uninstall', success=False)
                for name in ['.silencer-local-install', 'launch.sh', 'current']:
                    self.assertTrue((root/'data'/name).exists(), name)
                self.assertTrue((root/'services/silencer-local.service').exists())
                (root/'service-active-state').unlink(missing_ok=True)
                (root/'service-processes').unlink(missing_ok=True)
                self.run_installer(root, '--uninstall')

    def test_linux_loaded_service_ownership_is_checked_without_a_unit_file(self):
        # Exercise the production guard with a read-only systemctl fixture. It
        # cannot call a host service or download/install anything.
        source = INSTALLER.read_text()
        guard = source[source.index('capture_check_service_owner() {'):source.index('\ncapture_uninstall_stage()')]
        root = self.sandbox('Linux')
        data = root/'data'
        unit = root/'services/silencer-local.service'
        unit.parent.mkdir(exist_ok=True)
        loaded = '{ path=/bin/bash ; argv[]=/bin/bash '+str(data/'launch.sh')+' ; ignore_errors=no ; }'
        program = guard + r'''
capture_root="$1"; capture_service_file="$2"; capture_fixture="$3"
capture_os=linux; capture_sandbox=''
systemctl() {
  [[ "$1 $2 $3 $4" == '--user show silencer-local.service -p' && "$6" == --value ]] || exit 97
  case "$5" in
    LoadState) cat "$capture_fixture/load-state";;
    ExecStart) cat "$capture_fixture/exec-start";;
    *) exit 97;;
  esac
}
capture_check_service_owner
'''
        for state, command, valid in [
            ('not-found', '', True), ('loaded', loaded, True),
            ('loaded', loaded.replace('/launch.sh', '-other/launch.sh'), False),
            ('error', '', False),
        ]:
            with self.subTest(state=state, valid=valid):
                (root/'load-state').write_text(state)
                (root/'exec-start').write_text(command)
                result = subprocess.run(['/bin/bash', '-c', program, 'fixture', str(data), str(unit), str(root)], capture_output=True, text=True)
                self.assertEqual(result.returncode == 0, valid, result.stderr)
        unit.write_text('ExecStart=/bin/bash "/another/installation/launch.sh"\n')
        (root/'load-state').write_text('not-found')
        result = subprocess.run(['/bin/bash', '-c', program, 'fixture', str(data), str(unit), str(root)], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)

    def sandbox(self, platform):
        temporary = tempfile.TemporaryDirectory(prefix='silencer-installer-%-')
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name).resolve()
        (root/'.silencer-installer-sandbox').write_text('silencer-installer-test-v1')
        (root/'platform').write_text(platform)
        (root/'release').mkdir()
        (root/'blocked-bin').mkdir()
        for command in ['launchctl', 'systemctl', 'open', 'xdg-open', 'curl', 'sudo', 'security', 'osascript']:
            sentinel = root/'blocked-bin'/command
            sentinel.write_text('#!/bin/sh\nprintf "forbidden host command\\n" > "'+str(root/'host-command-called')+'"\nexit 97\n')
            sentinel.chmod(0o755)
        return root

    def release(self, root, tag='one', missing=False, traversal=False):
        system = 'darwin' if (root/'platform').read_text() == 'Darwin' else 'linux'
        name = f'silencer-local-{system}-amd64.tar.gz'
        archive = root/'release'/name
        members = {'silencer-gateway': '#!/bin/sh\nexit 0\n', 'runtime/mitmdump': '#!/bin/sh\nexit 0\n', 'web_dist/index.html': tag}
        if missing: members.pop('runtime/mitmdump')
        if traversal: members['../escaped'] = 'must never extract'
        with tarfile.open(archive, 'w:gz') as target:
            for path, value in members.items():
                entry = tarfile.TarInfo(path)
                entry.mode = 0o755 if path != 'web_dist/index.html' else 0o644
                data = value.encode()
                entry.size = len(data)
                target.addfile(entry, io.BytesIO(data))
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        (root/'release/SHA256SUMS').write_text(f'{digest}  {name}\n')
        return digest, archive

    def environment(self, root):
        env = {k:v for k,v in os.environ.items() if not k.startswith('SILENCER_')}
        env['PATH'] = str(root/'blocked-bin')+os.pathsep+env['PATH']
        # HOME and the user's service/trust/proxy variables are never changed.
        return env

    def run_installer(self, root, *args, success=True):
        result = subprocess.run(['/bin/bash', str(INSTALLER), '--sandbox', str(root), *args], env=self.environment(root), capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode == 0, success, result.stdout+result.stderr)
        self.assertFalse((root/'host-command-called').exists())
        self.assertEqual(list((root/'data/versions').glob('.download.*')), [])
        return result

    def test_install_repeat_update_and_uninstall_preserve_data(self):
        for platform in ['Darwin', 'Linux']:
            with self.subTest(platform=platform):
                root = self.sandbox(platform)
                first, _ = self.release(root)
                self.run_installer(root)
                data = root/'data'
                for path in ['control.json', 'silencer.db', 'control.json.capture/mitmproxy-ca.pem']:
                    file = data/path
                    file.parent.mkdir(exist_ok=True)
                    file.write_text('private sentinel')
                self.run_installer(root)
                self.assertEqual((data/'current').read_text().strip(), first)
                self.assertEqual(len(list((data/'versions').iterdir())), 1)
                if platform == 'Darwin':
                    plist = plistlib.loads((root/'services/com.silencer.local.plist').read_bytes())
                    self.assertEqual(plist['ProgramArguments'][1], str(data/'launch.sh'))
                else:
                    unit = (root/'services/silencer-local.service').read_text()
                    self.assertIn('%%', unit)
                    self.assertIn('KillMode=process', unit)
                second, _ = self.release(root, 'two')
                self.run_installer(root)
                self.assertNotEqual(first, second)
                self.assertEqual((data/'current').read_text().strip(), second)
                self.assertTrue((data/'versions'/first).is_dir())
                self.run_installer(root, '--uninstall')
                self.assertFalse((data/'current').exists())
                self.assertEqual(list((root/'services').iterdir()), [])
                for path in ['control.json', 'silencer.db', 'control.json.capture/mitmproxy-ca.pem']:
                    self.assertEqual((data/path).read_text(), 'private sentinel')

    def uninstaller(self, root, keep=False, managed=True, fingerprint='', verifier='/bin/false'):
        platform = 'darwin' if (root/'platform').read_text() == 'Darwin' else 'linux'
        values = {
            '__SILENCER_DATA_ROOT__': shlex.quote(str(root/'data')),
            '__SILENCER_CAPTURE_ROOT__': shlex.quote(str(root/'data/control.json.capture')),
            '__SILENCER_PLATFORM__': shlex.quote(platform),
            '__SILENCER_VERIFIER__': shlex.quote(str(verifier)),
            '__SILENCER_MANAGED__': str(managed).lower(),
            '__SILENCER_KEEP_CA__': str(keep).lower(),
            '__SILENCER_CA_SHA256__': shlex.quote(fingerprint),
            '__SILENCER_LINUX_CA_ID__': "'0000000000000000'",
            '__SILENCER_CA_BASE64__': '',
            '__SILENCER_INSTALLER_BASE64__': base64.b64encode(INSTALLER.read_bytes()).decode(),
        }
        source = UNINSTALLER.read_text()
        for key, value in values.items(): source = source.replace(key, value)
        path = root/'uninstall.sh'
        path.write_text(source)
        return path

    def run_uninstaller(self, root, path, success=True):
        result = subprocess.run(['/bin/bash', str(path), '--sandbox', str(root)], env=self.environment(root), capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode == 0, success, result.stdout+result.stderr)
        self.assertFalse((root/'host-command-called').exists())
        report = (root/'data/control.json.capture/uninstall-report.html').read_text()
        return result, report

    def test_visual_uninstall_report_and_repeat_preserve_data(self):
        for platform in ['Darwin', 'Linux']:
            for keep in [False, True]:
                with self.subTest(platform=platform, keep=keep):
                    root = self.sandbox(platform)
                    self.release(root)
                    self.run_installer(root)
                    for name in ['control.json', 'silencer.db', 'control.json.capture/mitmproxy-ca.pem']:
                        file = root/'data'/name
                        file.parent.mkdir(exist_ok=True)
                        file.write_text('private sentinel')
                    script = self.uninstaller(root, keep=keep)
                    for _ in range(2):
                        _, report = self.run_uninstaller(root, script)
                        self.assertIn('<li data-service="removed"', report)
                        self.assertIn('<li data-certificate="'+('retained' if keep else 'not-present')+'"', report)
                        self.assertFalse((root/'data/.silencer-local-install').exists())
                        for name in ['control.json', 'silencer.db', 'control.json.capture/mitmproxy-ca.pem']:
                            self.assertEqual((root/'data'/name).read_text(), 'private sentinel')

    def test_visual_uninstall_failure_and_stale_certificate_are_not_success(self):
        for failure in ['service', 'new-certificate', 'unmanaged']:
            with self.subTest(failure=failure):
                root = self.sandbox('Darwin')
                self.release(root)
                self.run_installer(root)
                script = self.uninstaller(root, managed=failure != 'unmanaged')
                if failure == 'service': (root/'fail-service').write_text('bootout')
                if failure == 'new-certificate':
                    (root/'data/control.json.capture').mkdir()
                    (root/'data/control.json.capture/mitmproxy-ca-cert.pem').write_text('CA generated after script download')
                _, report = self.run_uninstaller(root, script, success=False)
                self.assertNotIn('<li data-service="removed"', report)
                self.assertIn('<li data-certificate="pending"', report)
                self.assertTrue((root/'data/.silencer-local-install').exists())
                if failure == 'new-certificate': self.assertIn('CA changed', report)

    def test_interrupted_uninstall_replaces_old_success_report(self):
        root = self.sandbox('Darwin')
        self.release(root)
        self.run_installer(root)
        state = root/'data/control.json.capture'
        state.mkdir()
        (state/'mitmproxy-ca-cert.pem').write_text('isolated test identity')
        (state/'uninstall-report.html').write_text('previous-success')
        verifier = root/'interrupt-verifier'
        verifier.write_text('#!/bin/sh\nkill -TERM "$PPID"\nexit 0\n')
        verifier.chmod(0o700)
        script = self.uninstaller(root, fingerprint='A'*64, verifier=verifier)
        _, report = self.run_uninstaller(root, script, success=False)
        self.assertIn('Interrupted', report)
        self.assertNotIn('previous-success', report)
        self.assertTrue((root/'data/.silencer-local-install').exists())

    def test_partial_uninstall_resumes_same_script_at_every_stage(self):
        phases = ['service-stopped', 'service-file-removed', 'launcher-removed', 'current-removed', 'marker-removed']
        for platform in ['Darwin', 'Linux']:
            for phase in phases:
                with self.subTest(platform=platform, phase=phase):
                    root = self.sandbox(platform)
                    self.release(root)
                    self.run_installer(root)
                    preserved = {}
                    for name in ['control.json', 'silencer.db', 'control.json.capture/mitmproxy-ca.pem']:
                        file = root/'data'/name
                        file.parent.mkdir(exist_ok=True)
                        file.write_bytes(('private sentinel: '+name).encode())
                        preserved[name] = file.read_bytes()
                    script = self.uninstaller(root)
                    (root/'stop-at').write_text(phase)
                    _, interrupted = self.run_uninstaller(root, script, success=False)
                    self.assertNotIn('<li data-service="removed"', interrupted)
                    self.assertEqual((root/'data/.silencer-local-install').exists(), phase != 'marker-removed')
                    _, recovered = self.run_uninstaller(root, script)
                    self.assertIn('<li data-service="removed"', recovered)
                    self.assertIn('<li data-certificate="not-present"', recovered)
                    self.assertFalse((root/'data/.silencer-local-install').exists())
                    for name, contents in preserved.items(): self.assertEqual((root/'data'/name).read_bytes(), contents)

    def test_invalid_release_never_replaces_current_or_restarts_service(self):
        for failure in ['checksum', 'truncated', 'incomplete', 'traversal']:
            with self.subTest(failure=failure):
                root = self.sandbox('Darwin')
                previous, _ = self.release(root)
                self.run_installer(root)
                operations = (root/'operations').read_text()
                _, archive = self.release(root, 'invalid', missing=failure == 'incomplete', traversal=failure == 'traversal')
                if failure == 'checksum': archive.write_bytes(b'incorrect checksum')
                if failure == 'truncated':
                    archive.write_bytes(b'not a tarball')
                    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
                    (root/'release/SHA256SUMS').write_text(f'{digest}  {archive.name}\n')
                self.run_installer(root, success=False)
                self.assertEqual((root/'data/current').read_text().strip(), previous)
                self.assertEqual((root/'operations').read_text(), operations)
                self.assertFalse((root/'data/versions/escaped').exists())

    def test_service_and_readiness_failure_roll_back(self):
        for platform in ['Darwin', 'Linux']:
            for failure in ['service', 'readiness']:
                with self.subTest(platform=platform, failure=failure):
                    root = self.sandbox(platform)
                    previous, _ = self.release(root)
                    self.run_installer(root)
                    launcher = (root/'data/launch.sh').read_bytes()
                    service = next((root/'services').iterdir())
                    contents = service.read_bytes()
                    self.release(root, 'update')
                    if failure == 'service': (root/'fail-service').write_text('bootstrap' if platform == 'Darwin' else 'restart')
                    else: (root/'fail-ready').touch()
                    self.run_installer(root, success=False)
                    self.assertEqual((root/'data/current').read_text().strip(), previous)
                    self.assertEqual((root/'data/launch.sh').read_bytes(), launcher)
                    self.assertEqual(service.read_bytes(), contents)

    def test_existing_damaged_version_is_repaired(self):
        for platform in ['Darwin', 'Linux']:
            with self.subTest(platform=platform):
                root = self.sandbox(platform)
                digest, _ = self.release(root)
                version = root/'data/versions'/digest
                version.mkdir(parents=True)
                self.run_installer(root)
                self.assertTrue(os.access(version/'runtime/mitmdump', os.X_OK))
                (version/'silencer-gateway').write_text('corrupt but still executable')
                self.run_installer(root)
                self.assertEqual((version/'silencer-gateway').read_text(), '#!/bin/sh\nexit 0\n')

    def test_same_version_readiness_failure_restores_previous_files(self):
        for platform in ['Darwin', 'Linux']:
            with self.subTest(platform=platform):
                root = self.sandbox(platform)
                digest, _ = self.release(root)
                self.run_installer(root)
                version = root/'data/versions'/digest
                (version/'silencer-gateway').write_text('previous file identity')
                (root/'fail-ready').write_text('1')
                self.run_installer(root, success=False)
                self.assertEqual((version/'silencer-gateway').read_text(), 'previous file identity')
                self.assertEqual((root/'data/current').read_text().strip(), digest)

    def test_failed_rollback_preserves_recovery_files(self):
        root = self.sandbox('Linux')
        previous, _ = self.release(root)
        self.run_installer(root)
        self.release(root, 'next')
        (root/'fail-ready').write_text('1')
        copy = root/'blocked-bin/cp'
        copy.write_text('#!/bin/sh\nif [ "$1" = -p ]; then case "$2" in */rollback/current) exit 1;; esac; fi\nexec /bin/cp "$@"\n')
        copy.chmod(0o755)
        result = subprocess.run(['/bin/bash', str(INSTALLER), '--sandbox', str(root)], env=self.environment(root), capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('recovery files retained', result.stderr)
        saved = list((root/'data/versions').glob('.download.*/rollback/current'))
        self.assertEqual(len(saved), 1)
        self.assertEqual(saved[0].read_text().strip(), previous)
        self.assertTrue((root/'data/versions'/previous/'silencer-gateway').exists())
        self.assertFalse((root/'host-command-called').exists())

    def test_interrupted_download_preserves_previous_installation(self):
        root = self.sandbox('Darwin')
        previous, _ = self.release(root)
        self.run_installer(root)
        _, archive = self.release(root, 'next')
        archive.unlink()
        os.mkfifo(archive)
        archive_started = root/'archive-copy-started'
        copy = root/'blocked-bin/cp'
        copy.write_text('#!/bin/sh\nif [ "$1" = "$SILENCER_TEST_ARCHIVE_SOURCE" ]; then : > "$SILENCER_TEST_ARCHIVE_STARTED"; exec /bin/cat "$1" > "$2"; fi\nexec /bin/cp "$@"\n')
        copy.chmod(0o755)
        env = self.environment(root)
        env['SILENCER_TEST_ARCHIVE_SOURCE'] = str(archive)
        env['SILENCER_TEST_ARCHIVE_STARTED'] = str(archive_started)
        process = subprocess.Popen(['/bin/bash', str(INSTALLER), '--sandbox', str(root)], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        try:
            download_started = root/'data/versions'
            deadline = time.monotonic()+15
            while time.monotonic() < deadline:
                if archive_started.exists(): break
                if process.poll() is not None: break
                time.sleep(0.01)
            self.assertTrue(archive_started.exists(), 'installer did not begin downloading the release archive')
            self.assertIsNone(process.poll(), 'installer exited before the archive download was interrupted')
            os.killpg(process.pid, signal.SIGTERM)
            process.wait(timeout=15)
        finally:
            # Remove any remaining download helper even when the installer
            # shell has already exited.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            if process.poll() is None: process.wait()
        self.assertNotEqual(process.returncode, 0)
        self.assertEqual((root/'data/current').read_text().strip(), previous)
        self.assertEqual(list((root/'data/versions').glob('.download.*')), [])
        self.assertFalse((root/'host-command-called').exists())

    def test_unmarked_sandbox_is_rejected_before_writes(self):
        root = self.sandbox('Linux')
        (root/'.silencer-installer-sandbox').unlink()
        result = subprocess.run(['/bin/bash', str(INSTALLER), '--sandbox', str(root)], env=self.environment(root), capture_output=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((root/'data').exists())
        self.assertFalse((root/'host-command-called').exists())

    def test_symlinked_sandbox_destination_is_rejected(self):
        root = self.sandbox('Linux')
        with tempfile.TemporaryDirectory(prefix='silencer-sandbox-external-') as directory:
            external = Path(directory)
            (root/'services').symlink_to(external, target_is_directory=True)
            result = subprocess.run(['/bin/bash', str(INSTALLER), '--sandbox', str(root)], env=self.environment(root), capture_output=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(list(external.iterdir()), [])
            self.assertFalse((root/'data').exists())

    def test_first_install_readiness_failure_removes_own_service(self):
        root = self.sandbox('Linux')
        self.release(root)
        (root/'fail-ready').touch()
        self.run_installer(root, success=False)
        self.assertFalse((root/'data/current').exists())
        self.assertFalse((root/'services/silencer-local.service').exists())
        self.assertIn('disable --now', (root/'operations').read_text())


if __name__ == '__main__':
    unittest.main()
