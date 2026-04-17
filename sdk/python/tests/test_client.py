"""Tests for the Observe Python SDK."""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import List

import pytest

import observe_sdk


class _RecordingServer(BaseHTTPRequestHandler):
    posts: List[tuple] = []

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        body = json.loads(raw.decode("utf-8"))
        self.__class__.posts.append((self.path, body, dict(self.headers)))
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *args, **kwargs):  # silence stderr noise
        pass


@pytest.fixture
def server():
    _RecordingServer.posts = []
    httpd = HTTPServer(("127.0.0.1", 0), _RecordingServer)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    host, port = httpd.server_address
    yield f"http://{host}:{port}", _RecordingServer.posts
    httpd.shutdown()


def test_capture_exception(server):
    endpoint, posts = server
    client = observe_sdk.Client(endpoint=endpoint, api_key="k")
    try:
        raise ValueError("boom")
    except ValueError as exc:
        client.capture_exception(exc, release="v1")
    client.close()

    error_posts = [p for p in posts if p[0] == "/api/v1/errors"]
    assert len(error_posts) == 1
    body = error_posts[0][1]
    assert body["error_type"] == "ValueError"
    assert body["error_value"] == "boom"
    assert body["release_tag"] == "v1"
    assert body["level"] == "error"
    assert len(body["stack_trace"]) >= 1
    assert error_posts[0][2].get("X-Api-Key") == "k"


def test_log_batching_flushes_on_close(server):
    endpoint, posts = server
    client = observe_sdk.Client(
        endpoint=endpoint,
        log_batch_size=100,
        log_flush_interval=3600.0,
    )
    for i in range(5):
        client.info("hello", i=i)
    client.close()

    log_posts = [p for p in posts if p[0] == "/api/v1/logs"]
    assert len(log_posts) == 5


def test_auto_flush_at_batch_size(server):
    endpoint, posts = server
    client = observe_sdk.Client(
        endpoint=endpoint,
        log_batch_size=3,
        log_flush_interval=3600.0,
    )
    for i in range(3):
        client.warn("warn", n=i)
    # Flush is synchronous when the buffer fills.
    time.sleep(0.1)
    log_posts = [p for p in posts if p[0] == "/api/v1/logs"]
    assert len(log_posts) == 3
    client.close()
