"""Protocol tests: no OS interception, certificate installation or host changes."""
import importlib.util
import pathlib
import unittest
from unittest.mock import patch
from types import SimpleNamespace
from mitmproxy import connection, ctx, http, options
from mitmproxy.addons.next_layer import NextLayer, NeedsMoreData
from mitmproxy.proxy.context import Context
from mitmproxy.proxy.layers import HttpLayer, TCPLayer
from mitmproxy.proxy.layers.modes import TransparentProxy

path = pathlib.Path(__file__).resolve().parents[1] / 'internal/gateway/capture_assets/addon.py'
spec = importlib.util.spec_from_file_location('silencer_capture_addon', path)
addon = importlib.util.module_from_spec(spec)
spec.loader.exec_module(addon)


class AddonProtocolTests(unittest.TestCase):
    def setUp(self):
        self.options = options.Options()
        self.patch = patch.object(ctx, 'options', self.options, create=True)
        self.patch.start()
        self.addon = addon.SilencerCapture()
        self.addon.targets = {'api.example.test:443'}
        self.addon.tunnels = {}
        self.addon.excluded_proxy_pids = set()
        self.addon.proxy_listener_pids = {}
        self.addon.checked_proxy_endpoints = set()
        self.addon.config = {'bridge_url': 'http://127.0.0.1:12345', 'bridge_token': 'private-bridge-token'}
        self.native = NextLayer()
        self.addon.next_layer_addon = self.native
        self.addon.original_next_layer = self.native._next_layer
        self.client = connection.Client(peername=('127.0.0.1', 4444), sockname=('127.0.0.1', 8080))
        self.context = Context(self.client, self.options)
        self.context.server.address = ('127.0.0.1', 7890)
        TransparentProxy(self.context)

    def tearDown(self):
        self.patch.stop()

    def test_fragmented_connect_waits_for_complete_head(self):
        for fragment in (b'C', b'CON', b'CONNECT ', b'CONNECT api.example.test:443 HTTP/1.1\r\n'):
            with self.assertRaises(NeedsMoreData):
                self.addon.select_layer(self.context, fragment, b'')

    def test_known_connect_uses_authority_not_host_and_preserves_proxy(self):
        layer = self.addon.select_layer(self.context, b'CONNECT api.example.test:443 HTTP/1.1\r\nHost: misleading.test\r\n\r\n', b'')
        self.assertIsInstance(layer, HttpLayer)
        self.assertEqual(self.addon.tunnels[self.client.id]['proxy'], 'http://127.0.0.1:7890')
        self.assertEqual(self.context.server.address, ('127.0.0.1', 7890))

    def test_unknown_connect_is_opaque_and_keeps_original_destination(self):
        layer = self.addon.select_layer(self.context, b'CONNECT unrelated.test:443 HTTP/1.1\r\nHost: unrelated.test\r\n\r\n', b'')
        self.assertIsInstance(layer, TCPLayer)
        self.assertEqual(self.context.server.address, ('127.0.0.1', 7890))
        self.assertNotIn(self.client.id, self.addon.tunnels)

    def test_forwarding_keeps_provider_key_and_carries_authenticated_egress(self):
        self.test_known_connect_uses_authority_not_host_and_preserves_proxy()
        flow = http.HTTPFlow(self.client, self.context.server)
        flow.request = http.Request.make('POST', 'https://api.example.test/v1/chat?x=2&x=1', b'{"secret":"original"}', {'Authorization': 'Bearer provider-key', 'X-Silencer-Capture-Egress': 'http://attacker.test'})
        self.addon.requestheaders(flow)
        self.assertEqual(flow.request.url, 'http://127.0.0.1:12345/ingress')
        self.assertEqual(flow.request.headers['Authorization'], 'Bearer provider-key')
        self.assertEqual(flow.request.headers['X-Silencer-Capture-Egress'], 'http://127.0.0.1:7890')
        self.assertEqual(flow.request.headers['X-Silencer-Capture-Target'], 'https://api.example.test/v1/chat?x=2&x=1')
        self.assertTrue(flow.request.stream)

    def test_tunnel_cannot_change_target(self):
        self.test_known_connect_uses_authority_not_host_and_preserves_proxy()
        flow = http.HTTPFlow(self.client, self.context.server)
        flow.request = http.Request.make('GET', 'https://different.test/')
        self.addon.requestheaders(flow)
        self.assertEqual(flow.response.status_code, 403)

    def test_native_tls_ip_uses_verified_sni_and_keeps_path(self):
        self.client.sni = 'API.EXAMPLE.TEST'
        flow = http.HTTPFlow(self.client, self.context.server)
        flow.request = http.Request.make('POST', 'https://203.0.113.8/v1/messages?x=%2F&x=2', b'{}', {'Host': 'api.example.test', 'Authorization': 'Bearer provider-key'})
        flow.request.headers['Host'] = 'api.example.test'
        self.addon.requestheaders(flow)
        self.assertEqual(flow.request.headers['X-Silencer-Capture-Target'], 'https://api.example.test/v1/messages?x=%2F&x=2')
        self.assertEqual(flow.request.headers['Authorization'], 'Bearer provider-key')
        self.assertEqual(flow.request.url, 'http://127.0.0.1:12345/ingress')

    def test_native_tls_host_cannot_bypass_known_sni(self):
        for header in ('unrelated.test', 'api.example.test:8443', 'invalid host'):
            self.client.sni = 'api.example.test'
            flow = http.HTTPFlow(self.client, self.context.server)
            flow.request = http.Request.make('GET', 'https://203.0.113.8/', headers={'Host': header})
            flow.request.headers['Host'] = header
            self.addon.requestheaders(flow)
            self.assertEqual(flow.response.status_code, 403)

    def test_native_http_keeps_actual_nondefault_port(self):
        self.addon.targets = {'api.example.test:8080'}
        flow = http.HTTPFlow(self.client, self.context.server)
        flow.request = http.Request.make('POST', 'http://203.0.113.8:8080/v1/chat?x=2', headers={'Host': 'api.example.test'})
        flow.request.headers['Host'] = 'api.example.test'
        self.addon.requestheaders(flow)
        self.assertEqual(flow.request.headers['X-Silencer-Capture-Target'], 'http://api.example.test:8080/v1/chat?x=2')

    def test_tls_without_sni_does_not_infer_domain_from_host(self):
        flow = http.HTTPFlow(self.client, self.context.server)
        flow.request = http.Request.make('GET', 'https://203.0.113.8/', headers={'Host': 'api.example.test'})
        flow.request.headers['Host'] = 'api.example.test'
        self.addon.requestheaders(flow)
        self.assertEqual(flow.response.status_code, 403)

    def test_conflicting_http2_authority_or_duplicate_host_is_blocked(self):
        self.client.sni = 'api.example.test'
        for conflict in ('duplicate', 'authority'):
            flow = http.HTTPFlow(self.client, self.context.server)
            flow.request = http.Request.make('GET', 'https://203.0.113.8/', headers={'Host': 'api.example.test'})
            flow.request.headers['Host'] = 'api.example.test'
            if conflict == 'duplicate':
                flow.request.headers.add('Host', 'api.example.test')
            else:
                flow.request.http_version = 'HTTP/2.0'
                flow.request.authority = 'unrelated.test'
            self.addon.requestheaders(flow)
            self.assertEqual(flow.response.status_code, 403)

    def test_mismatched_tls_sni_is_rejected(self):
        self.test_known_connect_uses_authority_not_host_and_preserves_proxy()
        self.client.sni = 'different.test'
        self.assertIsInstance(self.addon.select_layer(self.context, b'', b''), addon.RejectConnection)

    def test_custom_local_proxy_process_is_excluded(self):
        server = SimpleNamespace(set_intercept=lambda spec: setattr(self, 'intercept_spec', spec))
        instance = SimpleNamespace(mode=SimpleNamespace(data='!123,!clash'))
        with patch.object(addon.LocalRedirectorInstance, '_server', server), patch.object(addon.LocalRedirectorInstance, '_instance', instance), patch.object(addon.subprocess, 'run', return_value=SimpleNamespace(stdout='654\n', returncode=0)):
            self.addon.exclude_proxy_listener(('127.0.0.1', 7890))
        self.assertIn('!654', self.intercept_spec)
        self.assertTrue(self.intercept_spec.startswith('!123,!clash,'))

    def test_unidentifiable_proxy_blocks_known_connect(self):
        with patch.object(self.addon, 'exclude_proxy_listener', side_effect=RuntimeError('unknown process')):
            selected = self.addon.select_layer(self.context, b'CONNECT api.example.test:443 HTTP/1.1\r\n\r\n', b'')
        self.assertIsInstance(selected, addon.RejectConnection)

    def test_linux_rejects_thread_id_filter_runtime(self):
        with patch.object(addon.sys, 'platform', 'linux'), patch.object(addon, 'version', return_value='0.12.9'):
            with self.assertRaises(RuntimeError): addon.check_runtime_packages()
        with patch.object(addon.sys, 'platform', 'linux'), patch.object(addon, 'version', return_value='0.12.11'):
            addon.check_runtime_packages()

    def test_restarted_proxy_replaces_stale_pid(self):
        server = SimpleNamespace(set_intercept=lambda spec: setattr(self, 'intercept_spec', spec))
        instance = SimpleNamespace(mode=SimpleNamespace(data='!123,!clash'))
        results = [SimpleNamespace(stdout='654\n', returncode=0), SimpleNamespace(stdout='987\n', returncode=0)]
        with patch.object(addon.LocalRedirectorInstance, '_server', server), patch.object(addon.LocalRedirectorInstance, '_instance', instance), patch.object(addon.subprocess, 'run', side_effect=results):
            self.addon.exclude_proxy_listener(('127.0.0.1', 7890))
            self.addon.exclude_proxy_listener(('127.0.0.1', 7890))
        self.assertIn('!987', self.intercept_spec)
        self.assertNotIn('!654', self.intercept_spec)


if __name__ == '__main__':
    unittest.main()
