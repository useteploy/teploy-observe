"""Actual stdlib HTTPResponse/socketpair regressions; no listening service."""
import http.client
import json
import socket
import threading
import time
import unittest
from unittest.mock import patch

from observe_sdk import Client
from observe_sdk.transport import DeliveryDeadline, Operation


class AbsoluteDeadlineTests(unittest.TestCase):
    def make_client(self):
        client = Client(endpoint="http://fixture.invalid", log_flush_interval=3600)
        client.info("deadline fixture")
        self.addCleanup(lambda: self.cleanup(client))
        return client

    def cleanup(self, client):
        client._stop.set()
        client._thread.join(0.2)
        operation = getattr(client, "_batch_operation", None)
        if operation:
            operation.cancel()
            self.assertTrue(operation.done.wait(0.3))

    def trickle(self, client, headers=False):
        reader, writer = socket.socketpair()
        self.addCleanup(reader.close)
        self.addCleanup(writer.close)
        body = b'{"accepted":1,"rejected":0}'
        wire = b"HTTP/1.1 200 OK\r\nContent-Length: " + str(len(body)).encode() + b"\r\n\r\n"
        if headers:
            class PairSocket:
                def __getattr__(self, name): return getattr(reader, name)
                def setsockopt(self, level, option, value):
                    if level == socket.IPPROTO_TCP and option == socket.TCP_NODELAY: return
                    reader.setsockopt(level, option, value)
            def connect(operation, *args, **kwargs):
                return operation.own(PairSocket())
            transport_patch = patch.object(Operation, "connect", connect)
            transport_patch.start()
            self.addCleanup(transport_patch.stop)
        else:
            writer.sendall(wire)
            response = http.client.HTTPResponse(reader)
            response.begin()
            class Opener:
                def open(self, req, timeout=None):
                    return response
            client._opener = Opener()
        def send():
            try:
                if headers:
                    writer.recv(4096)
                for offset in range(0, len(wire if headers else body), 2):
                    time.sleep(0.008)
                    writer.sendall((wire if headers else body)[offset:offset + 2])
            except OSError:
                pass
            finally:
                writer.close()
        thread = threading.Thread(target=send)
        thread.start()
        self.addCleanup(lambda: thread.join(0.3))

    def assert_retained(self, client, call):
        started = time.monotonic()
        with self.assertRaises(TimeoutError):
            call(timeout=0.025)
        self.assertLess(time.monotonic() - started, 0.12)
        self.assertEqual(client.stats()["queued"], 1)
        self.assertEqual(client.stats()["delivered"], 0)
        self.assertEqual(client.stats()["dropped"], {})
        self.assertEqual(client._send_attempts, 0)

    def test_progressing_body_flush(self):
        client = self.make_client()
        self.trickle(client)
        self.assert_retained(client, client.flush)

    def test_progressing_headers_flush(self):
        client = self.make_client()
        self.trickle(client, headers=True)
        self.assert_retained(client, client.flush)

    def test_progressing_body_close_final_drain(self):
        client = self.make_client()
        self.trickle(client)
        self.assert_retained(client, client.close)
        self.assertEqual(client._state, "closing")

    def test_blocked_resolution_stays_owned_and_cannot_send_after_cancel(self):
        client = self.make_client()
        entered, release = threading.Event(), threading.Event()
        def resolve(*args):
            entered.set()
            release.wait(0.3)
            return [(socket.AF_INET, socket.SOCK_STREAM, 0, "", ("127.0.0.1", 1))]
        with patch("observe_sdk.transport.socket.getaddrinfo", resolve), patch("observe_sdk.transport.socket.socket") as create:
            try:
                self.assert_retained(client, client.flush)
                self.assertTrue(entered.is_set())
                first = client._batch_operation
                self.assert_retained(client, client.flush)
                self.assertIs(client._batch_operation, first)
                release.set()
                self.assertTrue(first.done.wait(0.2))
                create.assert_not_called()
            finally:
                release.set()

    def test_stalled_connect_is_owned_and_cancelled(self):
        client = self.make_client()
        reader, writer = socket.socketpair()
        self.addCleanup(reader.close)
        self.addCleanup(writer.close)
        entered = threading.Event()
        class ConnectingSocket:
            def __getattr__(self, name): return getattr(reader, name)
            def connect(self, target):
                entered.set()
                reader.recv(1)  # actual blocking socket I/O at the connect seam
        addresses = [(socket.AF_INET, socket.SOCK_STREAM, 0, "", ("fixture", 80))]
        with patch("observe_sdk.transport.socket.getaddrinfo", return_value=addresses), patch("observe_sdk.transport.socket.socket", return_value=ConnectingSocket()):
            self.assert_retained(client, client.flush)
            self.assertTrue(entered.is_set())
            self.assertTrue(client._batch_operation.done.wait(0.2))
            self.assertTrue(client._batch_operation.cancelled.is_set())

    def test_ambiguous_and_oversized_ack_stay_queued(self):
        for body in [b'{"accepted":true,"rejected":0}', b'{"accepted":2,"rejected":0}', b'bad json', b' ' * (64 * 1024 + 1)]:
            client = self.make_client()
            class Response:
                status = 200
                def __enter__(self): return self
                def __exit__(self, *args): pass
                def read(self, size): return body[:size]
            class Opener:
                def open(self, *args, **kwargs): return Response()
            client._opener = Opener()
            with self.assertRaises((ValueError, RuntimeError)):
                client.flush(timeout=0.1)
            self.assertEqual(client.stats()["queued"], 1)
            self.assertEqual(client.stats()["delivered"], 0)


if __name__ == "__main__":
    unittest.main()
