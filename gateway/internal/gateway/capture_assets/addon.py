"""Silencer's pinned mitmproxy 12.2.3 adapter. No bodies are retained here."""
import json
import os
import re
import threading
import ipaddress
import subprocess
import sys
import socket
from importlib.metadata import version
import urllib.request
from urllib.parse import urlsplit
from mitmproxy import ctx, http
from mitmproxy.addons.next_layer import NeedsMoreData
from mitmproxy.net.http.http1.read import read_request_head
from mitmproxy.net.http import url as http_url
from mitmproxy.proxy import commands, events, layer
from mitmproxy.proxy.layers import HttpLayer, TCPLayer
from mitmproxy.proxy.layers.http import HTTPMode
from mitmproxy.proxy.mode_servers import LocalRedirectorInstance


def check_runtime_packages():
    if sys.platform == 'linux':
        # Earlier helpers match thread IDs and can recapture Go worker traffic.
        if version('mitmproxy-rs') != '0.12.11' or version('mitmproxy-linux') != '0.12.11':
            raise RuntimeError('Linux capture requires mitmproxy-rs and mitmproxy-linux 0.12.11')


class RejectConnection(layer.Layer):
    def _handle_event(self, event):
        if isinstance(event, events.Start):
            yield commands.CloseConnection(self.context.client)


def authority(host, port):
    host = host.lower().rstrip('.')
    return f"[{host}]:{port}" if ':' in host else f"{host}:{port}"


def original_capture_url(flow, targets, tunnel=None):
    request = flow.request
    port = request.port
    wire_host = request.host.lower().rstrip('.')
    sni = (flow.client_conn.sni or '').lower().rstrip('.')
    declarations = request.headers.get_all('Host')
    if request.authority:
        declarations = [request.authority, *declarations]
    parsed = []
    malformed = False
    for value in declarations:
        try:
            host, declared_port = http_url.parse_authority(value, check=True)
            parsed.append((host.lower().rstrip('.'), declared_port))
        except ValueError:
            malformed = True
    candidates = {authority(wire_host, port)}
    if tunnel:
        candidates.add(tunnel['authority'])
    if sni:
        candidates.add(authority(sni, port))
    candidates.update(authority(host, port) for host, _ in parsed)
    if not candidates.intersection(targets):
        return None
    try:
        ipaddress.ip_address(wire_host)
        wire_is_ip = True
    except ValueError:
        wire_is_ip = False
    if tunnel:
        host = tunnel['host']
    elif request.scheme == 'https':
        # Native transparent HTTP replaces Request.host with the destination IP.
        # Recover a domain only from TLS SNI, never from a shared-IP lookup.
        host = sni or wire_host
    else:
        host = parsed[0][0] if wire_is_ip and parsed else wire_host
    if (malformed or len(request.headers.get_all('Host')) > 1
            or authority(host, port) not in targets
            or (not wire_is_ip and wire_host != host)
            or (sni and request.scheme == 'https' and sni != host)
            or any(declared_host != host or (declared_port is not None and declared_port != port)
                   for declared_host, declared_port in parsed)
            or (tunnel and authority(host, port) != tunnel['authority'])):
        raise ValueError('Capture target identity mismatch')
    return http_url.unparse(request.scheme, host, port, request.path)


class SilencerCapture:
    def load(self, loader):
        try:
            check_runtime_packages()
        except Exception:
            os._exit(78)
        with open(os.environ["SILENCER_CAPTURE_BRIDGE_CONFIG"], encoding="utf-8") as source:
            self.config = json.load(source)
        self.targets = set(self.config["targets"])
        self.tunnels = {}
        self.excluded_proxy_pids = set()
        self.proxy_listener_pids = {}
        self.stop_event = threading.Event()
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def configure(self, updated):
        if hasattr(self, "targets"):
            # This decision happens before TLS decryption, not in request().
            desired = ["^(?:" + "|".join(re.escape(x) for x in sorted(self.targets)) + ")$"] if self.targets else ["(?!)"]
            if ctx.options.allow_hosts != desired:
                ctx.options.update(allow_hosts=desired)

    def running(self):
        try:
            for proxy in self.config.get('proxies', []):
                endpoint = urlsplit(proxy)
                if endpoint.hostname:
                    self.exclude_proxy_listener((endpoint.hostname, endpoint.port or (443 if endpoint.scheme == 'https' else 1080 if endpoint.scheme.startswith('socks') else 80)))
        except Exception:
            # mitmproxy logs hook exceptions and can continue without an addon.
            # Never leave its default plaintext-forwarding path active here.
            os._exit(1)
        # Pinned internal API: wrap the actual decision function so a fragmented
        # CONNECT can raise NeedsMoreData *inside* the built-in buffering layer.
        # A script's next_layer hook returning None would not defer that layer.
        self.next_layer_addon = ctx.master.addons.get("nextlayer")
        self.original_next_layer = self.next_layer_addon._next_layer
        self.next_layer_addon._next_layer = self.select_layer
        def pulse():
            failures = 0
            while not self.stop_event.is_set():
                try:
                    req = urllib.request.Request(self.config["bridge_url"] + "/heartbeat", data=json.dumps({'protected': bool(self.targets), 'targets': sorted(self.targets)}).encode(), headers={"X-Silencer-Capture-Bridge": self.config["bridge_token"]})
                    with self.opener.open(req, timeout=2) as response:
                        state = json.load(response)
                        desired = state.get('targets', [])
                        proxies = state.get('proxies', self.config.get('proxies', []))
                        failures = 0
                        if set(desired) != self.targets or proxies != self.config.get('proxies', []):
                            ctx.master.event_loop.call_soon_threadsafe(self.set_configuration, desired, proxies)
                except Exception:
                    failures += 1
                    if failures >= 5:
                        os._exit(1)
                self.stop_event.wait(2)
        self.thread = threading.Thread(target=pulse, daemon=True)
        self.thread.start()

    def set_targets(self, targets):
        self.targets = set(targets)
        self.configure(set())

    def set_configuration(self, targets, proxies):
        try:
            for proxy in proxies:
                endpoint = urlsplit(proxy)
                if endpoint.hostname:
                    self.exclude_proxy_listener((endpoint.hostname, endpoint.port or (443 if endpoint.scheme == 'https' else 1080 if endpoint.scheme.startswith('socks') else 80)))
            self.config['proxies'] = proxies
            self.set_targets(targets)
        except Exception:
            os._exit(1)

    def select_layer(self, context, client_data, server_data):
        tunnel = self.tunnels.get(context.client.id)
        if tunnel:
            if context.client.sni and context.client.sni.lower().rstrip('.') != tunnel['host']:
                return RejectConnection(context)
            hello = self.next_layer_addon._get_client_hello(context, client_data)
            if hello and hello.sni and hello.sni.lower().rstrip('.') != tunnel['host']:
                return RejectConnection(context)
        elif context.client.transport_protocol == 'tcp' and not context.client.tls:
            prefix = b'CONNECT '
            if client_data and (prefix.startswith(client_data) or client_data.startswith(prefix)):
                if len(client_data) > 65536:
                    return RejectConnection(context)
                if b'\r\n\r\n' not in client_data:
                    raise NeedsMoreData
                try:
                    head = read_request_head(client_data.split(b'\r\n\r\n', 1)[0].split(b'\r\n'))
                except ValueError:
                    return RejectConnection(context)
                if authority(head.host, head.port) not in self.targets:
                    # Preserve the original proxy destination and the complete
                    # CONNECT bytes for unknown targets, without TLS inspection.
                    return TCPLayer(context, ignore=True)
                original_proxy = context.server.address
                if not original_proxy:
                    return self.original_next_layer(context, client_data, server_data)
                proxy_auth = head.headers.get('Proxy-Authorization', '')
                if proxy_auth:
                    # Do not consume an authenticated proxy handshake that we
                    # cannot preserve. Its original proxy remains authoritative.
                    return RejectConnection(context)
                try:
                    self.exclude_proxy_listener(original_proxy)
                except RuntimeError:
                    return RejectConnection(context)
                self.tunnels[context.client.id] = {'host': head.host.lower().rstrip('.'), 'authority': authority(head.host, head.port), 'proxy': 'http://' + authority(*original_proxy)}
                return HttpLayer(context, HTTPMode.regular)
        return self.original_next_layer(context, client_data, server_data)

    def exclude_proxy_listener(self, address):
        # Update only our redirector's process filter. No system proxy, routing,
        # DNS or third-party application configuration is changed.
        server = LocalRedirectorInstance._server
        instance = LocalRedirectorInstance._instance
        if server is None or instance is None:
            return
        try:
            addresses = socket.getaddrinfo(address[0], address[1], type=socket.SOCK_STREAM)
            local = False
            for family, _, _, _, endpoint in addresses:
                if ipaddress.ip_address(endpoint[0]).is_loopback:
                    local = True
                    break
                try:
                    with socket.socket(family, socket.SOCK_STREAM) as probe:
                        probe.bind((endpoint[0], 0))
                    local = True
                    break
                except OSError:
                    pass
            if not local:
                return
            port = int(address[1])
            command = ['powershell.exe', '-NoProfile', '-Command', f'Get-NetTCPConnection -LocalPort {port} -State Listen | Select-Object -ExpandProperty OwningProcess'] if sys.platform == 'win32' else ['lsof', '-t', '-nP', f'-iTCP:{port}', '-sTCP:LISTEN']
            result = subprocess.run(command, capture_output=True, text=True, timeout=2)
            pids = {int(line.strip()) for line in result.stdout.splitlines() if line.strip().isdigit()}
            if result.returncode != 0 or not pids:
                raise RuntimeError('Cannot identify local proxy process; capture remains disabled')
            self.proxy_listener_pids[address] = pids
            self.excluded_proxy_pids = set().union(*self.proxy_listener_pids.values())
            base = instance.mode.data
            exclusions = ','.join('!' + str(pid) for pid in sorted(self.excluded_proxy_pids | {os.getpid()}))
            spec = base + (',' if base else '') + exclusions
            if sys.platform == 'linux' and len(spec.split(',')) > 20:
                raise RuntimeError('Linux redirector process filter capacity exceeded')
            server.set_intercept(spec)
        except (OSError, ValueError, subprocess.TimeoutExpired):
            raise RuntimeError('Cannot exclude local proxy process; capture remains disabled') from None

    def http_connect(self, flow: http.HTTPFlow):
        tunnel = self.tunnels.get(flow.client_conn.id)
        if tunnel and authority(flow.request.host, flow.request.port) != tunnel['authority']:
            flow.response = http.Response.make(403, b'CONNECT target mismatch')

    def client_disconnected(self, client):
        self.tunnels.pop(client.id, None)

    def requestheaders(self, flow: http.HTTPFlow):
        tunnel = self.tunnels.get(flow.client_conn.id)
        try:
            original = original_capture_url(flow, self.targets, tunnel)
        except ValueError:
            flow.response = http.Response.make(403, b'Capture target identity mismatch')
            return
        if original is None:
            return
        try:
            # Re-check before egress: a core can restart on the same port with a
            # new PID while this adapter is running.
            for proxy in self.config.get('proxies', []):
                endpoint = urlsplit(proxy)
                if endpoint.hostname:
                    self.exclude_proxy_listener((endpoint.hostname, endpoint.port or (443 if endpoint.scheme == 'https' else 1080 if endpoint.scheme.startswith('socks') else 80)))
            if tunnel:
                endpoint = urlsplit(tunnel['proxy'])
                self.exclude_proxy_listener((endpoint.hostname, endpoint.port))
        except RuntimeError:
            flow.response = http.Response.make(503, b'Cannot safely preserve local proxy egress')
            return
        endpoint = urlsplit(self.config["bridge_url"])
        for header in list(flow.request.headers):
            if header.lower().startswith("x-silencer-"):
                del flow.request.headers[header]
        flow.request.headers["X-Silencer-Capture-Target"] = original
        flow.request.headers["X-Silencer-Capture-Bridge"] = self.config["bridge_token"]
        if tunnel:
            flow.request.headers["X-Silencer-Capture-Egress"] = tunnel['proxy']
        flow.request.scheme = "http"
        flow.request.host = endpoint.hostname
        flow.request.port = endpoint.port
        flow.request.path = "/ingress"
        flow.request.stream = True

    def responseheaders(self, flow: http.HTTPFlow):
        flow.response.stream = True

    def done(self):
        self.stop_event.set()
        if hasattr(self, 'original_next_layer'):
            self.next_layer_addon._next_layer = self.original_next_layer


addons = [SilencerCapture()]
