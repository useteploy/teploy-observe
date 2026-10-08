"""Owned, cancellable HTTP operations with an absolute lifetime deadline."""
from __future__ import annotations

import http.client
import socket
import threading
import time
from urllib import request


class DeliveryDeadline(TimeoutError):
    """An ambiguous delivery remains queued without spending its retry budget."""


class Operation:
    def __init__(self, deadline: float):
        self.deadline = deadline
        self.done = threading.Event()
        self.cancelled = threading.Event()
        self.lock = threading.Lock()
        self.sockets = []
        self.result = None
        self.error = None

    def remaining(self):
        left = self.deadline - time.monotonic()
        if self.cancelled.is_set() or left <= 0:
            raise DeliveryDeadline("observe_sdk: transport deadline reached")
        return left

    def own(self, sock):
        with self.lock:
            if self.cancelled.is_set():
                sock.close()
                raise DeliveryDeadline("observe_sdk: transport cancelled")
            self.sockets.append(sock)
        return sock

    def cancel(self, *, interrupt=True):
        # shutdown interrupts a progressing header/body read, unlike resetting
        # the timeout on each read. No buffered file close waits on its read lock.
        with self.lock:
            if interrupt:
                self.cancelled.set()
            for sock in self.sockets:
                try:
                    sock.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
                try:
                    sock.close()
                except OSError:
                    pass

    def connect(self, address, timeout=None, source_address=None):
        host, port = address
        # DNS may be uncancellable in libc. It stays inside this single owned
        # operation; cancelled resolution can never subsequently connect/send.
        addresses = socket.getaddrinfo(host, port, 0, socket.SOCK_STREAM)
        error = None
        for family, kind, proto, _, target in addresses:
            self.remaining()
            sock = self.own(socket.socket(family, kind, proto))
            try:
                sock.settimeout(self.remaining())
                if source_address:
                    sock.bind(source_address)
                sock.connect(target)
                self.remaining()
                return sock
            except OSError as exc:
                error = exc
                sock.close()
        if error:
            raise error
        raise OSError("no endpoint addresses")

    def opener(self, redirect_handler):
        operation = self

        class HTTP(http.client.HTTPConnection):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, **kwargs)
                self._create_connection = operation.connect

        class HTTPS(http.client.HTTPSConnection):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, **kwargs)
                self._create_connection = operation.connect

            def connect(self):
                # Split wrapping and handshake so the TLS socket is owned before
                # any blocking handshake, including HTTPS proxy tunnels.
                http.client.HTTPConnection.connect(self)
                host = self._tunnel_host or self.host
                self.sock = operation.own(self._context.wrap_socket(
                    self.sock, server_hostname=host, do_handshake_on_connect=False))
                self.sock.settimeout(operation.remaining())
                self.sock.do_handshake()
                operation.remaining()

        class HTTPHandler(request.HTTPHandler):
            def http_open(self, req):
                return self.do_open(HTTP, req)

        class HTTPSHandler(request.HTTPSHandler):
            def https_open(self, req):
                return self.do_open(HTTPS, req, context=self._context)

        return request.build_opener(redirect_handler, HTTPHandler, HTTPSHandler)

    def own_response(self, response):
        # Also permits a real stdlib HTTPResponse over socketpair in regressions.
        sock = getattr(getattr(getattr(response, "fp", None), "raw", None), "_sock", None)
        if sock is not None:
            self.own(sock)
        self.remaining()
