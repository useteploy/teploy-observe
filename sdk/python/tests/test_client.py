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

    # One batch request carries all five entries (audit F36: batching).
    batch_posts = [p for p in posts if p[0] == "/api/v1/logs/batch"]
    assert len(batch_posts) == 1
    assert len(batch_posts[0][1]["logs"]) == 5


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
    batch_posts = [p for p in posts if p[0] == "/api/v1/logs/batch"]
    assert sum(len(p[1]["logs"]) for p in batch_posts) == 3
    client.close()


# Audit F36: an unserializable attribute used to raise inside the detached
# flush loop AFTER the buffer was detached, silently discarding the valid
# entries behind it. Admission-time validation rejects just the bad entry.
def test_unserializable_entry_rejected_at_admission(server):
    endpoint, posts = server
    client = observe_sdk.Client(
        endpoint=endpoint, log_batch_size=100, log_flush_interval=3600.0
    )
    with pytest.raises(ValueError):
        client.info("bad", blob=object())
    client.info("good")
    client.close()
    batch_posts = [p for p in posts if p[0] == "/api/v1/logs/batch"]
    assert sum(len(p[1]["logs"]) for p in batch_posts) == 1


# Audit F34: shared mutable identity attributed one request's telemetry to
# whichever user identified last. bind_identity scopes it per context.
def test_identity_is_context_scoped(server):
    endpoint, posts = server
    client = observe_sdk.Client(
        endpoint=endpoint, log_flush_interval=3600.0
    )
    import asyncio

    async def user(name: str) -> None:
        with client.bind_identity(name):
            await asyncio.sleep(0.02)
            client.capture_exception(RuntimeError(f"err-{name}"))

    async def main() -> None:
        await asyncio.gather(user("alice"), user("bob"))

    asyncio.run(main())
    error_posts = [p for p in posts if p[0] == "/api/v1/errors"]
    idents = sorted(p[1]["distinct_id"] for p in error_posts)
    assert idents == ["alice", "bob"]
    # Anonymous afterwards.
    client.capture_exception(RuntimeError("anon"))
    error_posts = [p for p in posts if p[0] == "/api/v1/errors"]
    assert error_posts[-1][1].get("distinct_id", "") == ""
    client.close()


# Audit F33: identify emits an analytics $identify event whose ONLY identity
# field is the hashed-on-the-server top-level distinct_id — no raw user_id
# attribute, and no silently-ignored distinct_id on log payloads.
def test_identify_sends_analytics_event_without_raw_traits(server):
    endpoint, posts = server
    client = observe_sdk.Client(endpoint=endpoint, log_flush_interval=3600.0)
    client.identify("u-1", {"plan": "pro", "user_id": "u-1", "email": "a@b.c"})
    client.close()
    ident_posts = [p for p in posts if p[0] == "/api/v1/events"]
    assert len(ident_posts) == 1
    body = ident_posts[0][1]
    assert body["event_type"] == "$identify"
    assert body["distinct_id"] == "u-1"
    assert body["properties"] == {"plan": "pro"}


# AUD-035 (round 2): a failed batch post leaves the entries queued.
def test_failed_flush_retains_queue(server):
    endpoint, posts = server
    client = observe_sdk.Client(
        endpoint=endpoint, log_batch_size=100, log_flush_interval=3600.0
    )
    errors = []
    client.on_error = lambda exc: errors.append(exc)
    # Point the client at a dead port by closing the real server first.
    client.info("doomed")
    # Simulate failure by monkeypatching _post_bytes.
    def boom(path, data, **kw):
        raise RuntimeError("observe: post failed: connection refused")
    orig = client._post_bytes
    client._post_bytes = boom
    with pytest.raises(RuntimeError):
        client.flush()
    client._post_bytes = orig
    assert len(client._buffer) == 1, "failed batch must remain queued"
    client.flush()
    assert len(client._buffer) == 0
    batch_posts = [p for p in posts if p[0] == "/api/v1/logs/batch"]
    assert sum(len(p[1]["logs"]) for p in batch_posts) == 1
    client.close()


# AUD-036 (round 2): entries are frozen at admission — caller mutation
# after log() cannot change the delivered body.
def test_queued_entries_are_immutable_snapshots(server):
    endpoint, posts = server
    client = observe_sdk.Client(
        endpoint=endpoint, log_batch_size=100, log_flush_interval=3600.0
    )
    attrs = {"k": "original"}
    client.info("m", attrs=attrs)
    attrs["k"] = "mutated"
    client.flush()
    batch_posts = [p for p in posts if p[0] == "/api/v1/logs/batch"]
    assert batch_posts[0][1]["logs"][0]["attributes"]["attrs"]["k"] == "original"
    client.close()


# AUD-037 (round 2): invalid options are rejected before the thread starts.
def test_invalid_options_rejected():
    for kwargs in (
        {"log_flush_interval": 0},
        {"log_flush_interval": float("nan")},
        {"log_flush_interval": float("inf")},
        {"timeout": 0},
        {"log_batch_size": 0},
        {"log_batch_size": 500},
    ):
        try:
            observe_sdk.Client(
                endpoint="http://127.0.0.1:1", **kwargs
            )
        except ValueError:
            continue
        pytest.fail(f"{kwargs} must be rejected at construction")


# AUD-037: log() after close refuses admission instead of queueing for a
# dead worker.
def test_log_after_close_raises(server):
    endpoint, _ = server
    client = observe_sdk.Client(
        endpoint=endpoint, log_flush_interval=3600.0
    )
    client.close()
    with pytest.raises(RuntimeError):
        client.info("late")


# AUD-026 (round 2): batches pack by encoded bytes, not entry count.
def test_batch_respects_byte_budget(server):
    endpoint, posts = server
    client = observe_sdk.Client(
        endpoint=endpoint, log_batch_size=200, log_flush_interval=3600.0
    )
    blob = "x" * (30 * 1024)  # ~30 KiB per entry; 40 of them exceed 1 MiB
    for i in range(40):
        client.warn("big", blob=blob)
    client.flush()
    batch_posts = [p for p in posts if p[0] == "/api/v1/logs/batch"]
    assert len(batch_posts) >= 2, "byte budget must split the batch into multiple requests"
    for path, body, _ in batch_posts:
        assert len(json.dumps(body)) < 2 * 1024 * 1024
    client.close()
