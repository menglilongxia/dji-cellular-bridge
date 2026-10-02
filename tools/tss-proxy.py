#!/usr/bin/env python3
"""
Minimal selective HTTP proxy that works around the /etc/hosts entries this
Hackintosh uses to block Apple update/TSS servers ("macOS OS 屏蔽更新 by hkmac").

Only hostnames matching APPLE_TSS_PATTERNS are re-resolved via DNS-over-HTTPS
(getting the real public IP and so bypassing /etc/hosts). Every other host is
connected exactly as the system would have, so the router's fake-ip proxy keeps
working for normal traffic.

Used to let pymobiledevice3 fetch the personalized DeveloperDiskImage for an
iOS 26 device so Xcode stops reporting "Device is busy (Waiting to reconnect)".

    python3 tss-proxy.py &
    HTTP_PROXY=http://127.0.0.1:8899 HTTPS_PROXY=http://127.0.0.1:8899 \
        pymobiledevice3 mounter auto-mount
"""

import json
import socket
import socketserver
import sys
import threading
import urllib.parse
import urllib.request

LISTEN = ("127.0.0.1", 8899)
DOH_SERVERS = [
    "https://223.5.5.5/resolve?name={host}&type=A",
    "https://doh.pub/dns-query?name={host}&type=A",
]
# Hostnames that /etc/hosts blackholes to 127.0.0.1 but that we need to reach.
APPLE_TSS_PATTERNS = (
    "gs.apple.com",
    "appldnld.apple.com",
    "gg.apple.com",
    "ig.apple.com",
    "skl.apple.com",
)

_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
_dns_cache = {}
_dns_lock = threading.Lock()


def resolve_real(host):
    with _dns_lock:
        if host in _dns_cache:
            return _dns_cache[host]
    for tmpl in DOH_SERVERS:
        try:
            req = urllib.request.Request(tmpl.format(host=host),
                                         headers={"accept": "application/dns-json"})
            with _opener.open(req, timeout=8) as resp:
                data = json.load(resp)
            for ans in data.get("Answer", []):
                if ans.get("type") == 1:
                    ip = ans["data"]
                    with _dns_lock:
                        _dns_cache[host] = ip
                    print("[tss-proxy] %s -> %s (DoH)" % (host, ip), flush=True)
                    return ip
        except Exception as exc:
            print("[tss-proxy] DoH via %s failed: %s" % (tmpl.split("/")[2], exc),
                  flush=True)
    return None


def needs_bypass(host):
    return any(host == p or host.endswith("." + p) for p in APPLE_TSS_PATTERNS)


def connect(host, port):
    ip = resolve_real(host) if needs_bypass(host) else None
    if ip:
        return socket.create_connection((ip, port), timeout=15)
    return socket.create_connection((host, port), timeout=15)


def pump(a, b):
    try:
        while True:
            data = a.recv(65536)
            if not data:
                break
            b.sendall(data)
    except OSError:
        pass
    finally:
        for s in (a, b):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


class Proxy(socketserver.BaseRequestHandler):
    def handle(self):
        header = b""
        while b"\r\n\r\n" not in header:
            chunk = self.request.recv(4096)
            if not chunk:
                return
            header += chunk
            if len(header) > 65536:
                return

        try:
            first = header.split(b"\r\n", 1)[0].decode("latin-1")
            method, target, _version = first.split(" ", 2)
        except ValueError:
            return

        if method.upper() == "CONNECT":
            host, _, port = target.partition(":")
            try:
                upstream = connect(host, int(port or 443))
            except OSError as exc:
                self.request.sendall(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
                print("[tss-proxy] CONNECT %s failed: %s" % (target, exc), flush=True)
                return
            self.request.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            threading.Thread(target=pump, args=(self.request, upstream),
                             daemon=True).start()
            pump(upstream, self.request)
            upstream.close()
            return

        # Plain HTTP: the proxy receives an absolute URI.
        parsed = urllib.parse.urlsplit(target if "://" in target else "http://" + target)
        host = parsed.hostname
        port = parsed.port or 80
        if not host:
            return
        path = parsed.path or "/"
        if parsed.query:
            path += "?" + parsed.query
        rest = header.split(b"\r\n", 1)[1]
        try:
            upstream = connect(host, port)
        except OSError as exc:
            self.request.sendall(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
            print("[tss-proxy] GET %s failed: %s" % (host, exc), flush=True)
            return
        upstream.sendall(("%s %s HTTP/1.1\r\n" % (method, path)).encode() + rest)
        pump(upstream, self.request)
        upstream.close()


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


if __name__ == "__main__":
    print("[tss-proxy] listening on %s:%d" % LISTEN, flush=True)
    try:
        Server(LISTEN, Proxy).serve_forever()
    except KeyboardInterrupt:
        sys.exit(0)
