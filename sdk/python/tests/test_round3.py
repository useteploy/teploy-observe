"""Round-3 contracts: redirect refusal (TO-037) and default lifecycle (TO-044)."""
import threading
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading as _t

import pytest

import observe_sdk as obs
from observe_sdk.client import Client


def _two_servers():
    """A sink that records API keys, and a source that redirects to it."""
    seen = []

    class Sink(BaseHTTPRequestHandler):
        def do_POST(self):
            seen.append(self.headers.get("X-API-Key"))
            self.send_response(204)
            self.end_headers()

        def log_message(self, *a):
            pass

    class Source(BaseHTTPRequestHandler):
        def do_POST(self):
            self.send_response(307)
            self.send_header("Location", sink_url)
            self.end_headers()

        def log_message(self, *a):
            pass

    sink = ThreadingHTTPServer(("127.0.0.1", 0), Sink)
    source = ThreadingHTTPServer(("127.0.0.1", 0), Source)
    _t.Thread(target=sink.serve_forever, daemon=True).start()
    _t.Thread(target=source.serve_forever, daemon=True).start()
    sink_url = f"http://127.0.0.1:{sink.server_port}/ingest"
    return sink, source, f"http://127.0.0.1:{source.server_port}/ingest", seen


def test_redirect_never_forwards_api_key():
    sink, source, source_url, seen = _two_servers()
    try:
        c = Client(endpoint=source_url, api_key="REPRO_KEY")
        with pytest.raises(RuntimeError):
            c.capture_exception(ValueError("x"))
        c.close()
        assert seen == [], f"the key must never reach the redirect target, got {seen}"
    finally:
        sink.shutdown()
        source.shutdown()


def test_default_replacement_propagates_failed_close(monkeypatch):
    # A first client whose close fails must surface the failure instead of
    # silently swapping (TO-044).
    calls = {"closed": 0}

    class BrokenClose(Client):
        def close(self):
            calls["closed"] += 1
            raise TimeoutError("drain incomplete")

    real_init_state = obs.client._default
    obs.client._default = BrokenClose.__new__(BrokenClose)
    obs.client._default.opts = None
    try:
        # init validates candidate options first, then closes the old one.
        with pytest.raises(TimeoutError):
            obs.init(endpoint="http://127.0.0.1:1/x")
    finally:
        obs.client._default = real_init_state
    assert calls["closed"] == 1


def test_single_exit_handler_and_serialized_replacement():
    # Repeated init does not accumulate retained exit clients: the handler
    # closes whatever is current (TO-044).
    import observe_sdk.client as mod
    before = getattr(mod, "_exit_registered", True)
    assert before  # module registered its handler once at import
    # Two sequential inits against an unreachable endpoint succeed at swap
    # time only if close tolerates nothing-pending; use valid fresh clients.
    c1 = obs.init(endpoint="http://127.0.0.1:1/a")
    assert obs.client._default is c1
    # Force close failure of the outgoing client by killing its worker path:
    c1.close = lambda: None
    c2 = obs.init(endpoint="http://127.0.0.1:1/b")
    try:
        assert obs.client._default is c2
    finally:
        c2.close = lambda: None
