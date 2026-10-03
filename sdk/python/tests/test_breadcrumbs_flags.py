"""Breadcrumb and feature-flag tests for the Observe Python SDK."""

from __future__ import annotations

import json
import logging
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any, Dict, List

import pytest

import observe_sdk
from observe_sdk.breadcrumbs import BreadcrumbBuffer


class _Handler(BaseHTTPRequestHandler):
    posts: List[Any] = []
    flag_response: Any = (200, {"enabled": True, "variant": "b", "reason": "evaluated"})
    flag_delay = 0.0

    def do_POST(self):  # noqa: N802
        n = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(n).decode("utf-8"))
        type(self).posts.append((self.path, body, dict(self.headers)))
        if self.path == "/api/v1/flags/evaluate":
            if type(self).flag_delay:
                time.sleep(type(self).flag_delay)
            status, payload = type(self).flag_response
            raw = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        else:
            status, raw = 200, b"{}"
        self.send_response(status)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *a, **k):
        pass


@pytest.fixture
def srv():
    _Handler.posts = []
    _Handler.flag_response = (200, {"enabled": True, "variant": "b", "reason": "evaluated"})
    _Handler.flag_delay = 0.0
    httpd = HTTPServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    host, port = httpd.server_address
    yield f"http://{host}:{port}", _Handler.posts
    httpd.shutdown()


def _client(url: str, **kw: Any) -> observe_sdk.Client:
    kw.setdefault("log_flush_interval", 3600.0)
    return observe_sdk.Client(endpoint=url, api_key="k", site_id="s1", **kw)


def _errors(posts: List[Any]) -> List[Dict[str, Any]]:
    return [b for p, b, _ in posts if p == "/api/v1/errors"]


def test_buffer_is_bounded_and_ordered():
    b = BreadcrumbBuffer(3)
    for i in range(5):
        b.add(f"m{i}")
    assert [c["message"] for c in b.snapshot()] == ["m2", "m3", "m4"]
    assert len(b) == 3


def test_buffer_thread_safe_under_contention():
    b = BreadcrumbBuffer(50)

    def work(n: int) -> None:
        for i in range(500):
            b.add(f"{n}-{i}")
            b.snapshot()

    ts = [threading.Thread(target=work, args=(n,)) for n in range(8)]
    [t.start() for t in ts]
    [t.join() for t in ts]
    assert len(b) == 50


def test_wire_shape_and_data_handling():
    b = BreadcrumbBuffer()
    b.add("hi", category="checkout", type="user", level="warning", data={"n": 1})
    b.add("bad level", level="loud")
    b.add("big", data={"x": "y" * 5000})
    b.add("odd", data={"obj": object()})
    s = b.snapshot()
    assert set(s[0]) == {"type", "category", "message", "data", "timestamp", "level"}
    assert isinstance(s[0]["timestamp"], int) and s[0]["timestamp"] > 1_600_000_000_000
    assert s[0]["level"] == "warning" and s[0]["data"] == {"n": 1}
    assert s[1]["level"] == "info"
    assert s[2]["data"]["_truncated"] is True
    assert "object object" in s[3]["data"]["obj"]  # default=str, no crash
    # a snapshot is a copy
    s[0]["data"]["n"] = 99
    assert b.snapshot()[0]["data"]["n"] == 1


def test_before_breadcrumb_edit_drop_and_throw_fail_closed():
    errs: List[BaseException] = []

    def hook(c: Dict[str, Any]):
        if c["message"] == "drop":
            return None
        if c["message"] == "throw":
            raise RuntimeError("hook bug")
        c["message"] = c["message"].upper()
        return c

    b = BreadcrumbBuffer(10, hook, errs.append)
    b.add("keep")
    b.add("drop")
    b.add("throw")
    assert [c["message"] for c in b.snapshot()] == ["KEEP"]
    assert len(errs) == 1


def test_add_never_raises_on_garbage():
    b = BreadcrumbBuffer()
    b.add(None)  # type: ignore[arg-type]
    b.add("x", level=object())  # type: ignore[arg-type]
    b.add("x", timestamp="nope")  # type: ignore[arg-type]
    assert len(b) == 3


def test_exception_and_message_carry_breadcrumbs(srv):
    url, posts = srv
    c = _client(url, max_breadcrumbs=2)
    c.add_breadcrumb("one")
    c.add_breadcrumb("two", category="db", level="error", data={"rows": 3})
    c.add_breadcrumb("three")
    try:
        raise ValueError("boom")
    except ValueError as exc:
        c.capture_exception(exc)
    c.capture_message("heads up", level="warning")
    e1, e2 = _errors(posts)
    assert [x["message"] for x in e1["breadcrumbs"]] == ["two", "three"]
    assert e1["level"] == "error" and e1["error_type"] == "ValueError"
    assert e2["level"] == "warning" and e2["error_type"] == "Message" and e2["error_value"] == "heads up"
    assert len(e2["breadcrumbs"]) == 2
    c.clear_breadcrumbs()
    c.capture_message("clean")
    assert "breadcrumbs" not in _errors(posts)[-1]
    c.close()


def test_capture_message_rejects_bad_level(srv):
    c = _client(srv[0])
    with pytest.raises(ValueError):
        c.capture_message("x", level="fatal")
    c.close()


def test_logging_breadcrumbs_opt_in_level_and_cleanup(srv):
    url, posts = srv
    root = logging.getLogger()
    before = list(root.handlers)
    old_level = root.level
    root.setLevel(logging.DEBUG)
    try:
        # Off by default: no handler, nothing recorded.
        c0 = _client(url)
        logging.getLogger("app").warning("ignored")
        assert len(c0._crumbs) == 0
        assert root.handlers == before
        c0.close()

        c = _client(url, logging_breadcrumbs=True)
        assert len(root.handlers) == len(before) + 1
        logging.getLogger("app.db").info("below threshold")
        logging.getLogger("app.db").warning("slow query %s", "q1")
        logging.getLogger("app.api").error("failed")
        logging.getLogger("observe_sdk").error("sdk own noise")
        s = c._crumbs.snapshot()
        assert [(x["category"], x["level"], x["message"]) for x in s] == [
            ("app.db", "warning", "slow query q1"),
            ("app.api", "error", "failed"),
        ]
        assert s[0]["type"] == "log"
        c.capture_message("m")
        assert len(_errors(posts)[-1]["breadcrumbs"]) == 2
        c.close()
        assert root.handlers == before, "close() removes the handler"
    finally:
        root.setLevel(old_level)


def test_logging_breadcrumb_level_is_configurable(srv):
    logger = logging.getLogger("custom.scope")
    logger.setLevel(logging.DEBUG)
    c = _client(srv[0])
    c.install_logging_breadcrumbs(logging.INFO, logger)
    logger.debug("no")
    logger.info("yes")
    assert [x["message"] for x in c._crumbs.snapshot()] == ["yes"]
    c.remove_logging_breadcrumbs()
    logger.info("after removal")
    assert len(c._crumbs) == 1
    c.close()


def test_logging_handler_does_not_recurse_when_hook_logs(srv):
    logger = logging.getLogger("rec.test")
    logger.setLevel(logging.DEBUG)

    def hook(crumb):
        logger.warning("hook logging")
        return crumb

    c = _client(srv[0], before_breadcrumb=hook)
    c.install_logging_breadcrumbs(logging.WARNING, logger)
    logger.warning("trigger")
    assert len(c._crumbs) == 1  # the nested record was suppressed
    c.close()


def test_option_validation(srv):
    with pytest.raises(ValueError):
        _client(srv[0], max_breadcrumbs=0)
    with pytest.raises(ValueError):
        _client(srv[0], before_breadcrumb="nope")


def test_module_helpers_noop_before_init_and_work_after(srv):
    url, posts = srv
    observe_sdk.close()
    observe_sdk.client._default = None  # type: ignore[attr-defined]
    observe_sdk.add_breadcrumb("ignored")  # no-op, no raise
    assert observe_sdk.evaluate_flag("f", default={"enabled": True})["enabled"] is True
    observe_sdk.init(endpoint=url, api_key="k", log_flush_interval=3600.0)
    observe_sdk.add_breadcrumb("mod level")
    observe_sdk.capture_message("via module")
    assert _errors(posts)[-1]["breadcrumbs"][0]["message"] == "mod level"
    observe_sdk.close()


# --- flags ---------------------------------------------------------------


def test_evaluate_flag_request_and_response(srv):
    url, posts = srv
    c = _client(url)
    r = c.evaluate_flag("new-checkout", user_id="u1", attributes={"plan": "pro", "n": 3, "ok": True, "o": {}})
    assert r == {"key": "new-checkout", "enabled": True, "variant": "b", "reason": "evaluated", "source": "server"}
    path, body, headers = posts[-1]
    assert path == "/api/v1/flags/evaluate"
    assert body == {"site_id": "s1", "flag_key": "new-checkout", "user_id": "u1",
                    "context": {"plan": "pro", "n": "3", "ok": "true"}}
    assert {k.lower(): v for k, v in headers.items()}.get("x-api-key") == "k"
    c.close()


def test_evaluate_flag_defaults_user_to_identity(srv):
    url, posts = srv
    c = _client(url)
    c.identify("user-9")
    c.evaluate_flag("f")
    assert posts[-1][1]["user_id"] == "user-9"
    c.close()


@pytest.mark.parametrize("response", [
    (500, {"error": "x"}),
    (200, b"garbage"),
    (200, {"enabled": "yes"}),
    (200, {"enabled": False, "reason": "unavailable"}),
    (200, {"enabled": False, "reason": "invalid"}),
    (302, {}),
])
def test_evaluate_flag_failure_returns_caller_default(srv, response):
    url, _ = srv
    _Handler.flag_response = response
    c = _client(url)
    r = c.evaluate_flag("f", default={"enabled": True, "variant": "safe"})
    assert (r["enabled"], r["variant"], r["source"]) == (True, "safe", "default")
    assert r["error"]
    assert c.evaluate_flag("f")["enabled"] is False  # fail-safe default
    c.close()


def test_evaluate_flag_timeout(srv):
    url, _ = srv
    _Handler.flag_delay = 1.5
    c = _client(url)
    t0 = time.time()
    r = c.evaluate_flag("f", timeout=0.2, default={"enabled": True})
    assert time.time() - t0 < 1.2
    assert r["source"] == "default" and r["enabled"] is True
    c.close()


def test_evaluate_flag_unreachable_endpoint_never_raises():
    c = observe_sdk.Client(endpoint="http://127.0.0.1:1", log_flush_interval=3600.0)
    r = c.evaluate_flag("f", timeout=0.5)
    assert r["source"] == "default" and r["enabled"] is False
    c.close()


def test_evaluate_flag_sends_no_other_telemetry(srv):
    url, posts = srv
    c = _client(url)
    c.evaluate_flag("f")
    assert [p for p, _, _ in posts] == ["/api/v1/flags/evaluate"]
    c.close()
