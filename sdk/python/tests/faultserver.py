"""Fault-injection HTTP server for the Python SDK's O11 tests.

Python twin of sdk/testing/faultserver.mjs (Node) and the Go fault server
in sdk/go/o11_transport_test.go — see sdk/testing/README.md for the shared
matrix. Modes: ok, status, failN, partial, slow, reset.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, List, Tuple


class FaultServer:
    def __init__(self) -> None:
        self.mode: Dict[str, Any] = {"kind": "ok"}
        self.posts: List[Tuple[str, Dict[str, Any]]] = []
        self._lock = threading.Lock()
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                length = int(self.headers.get("Content-Length", "0"))
                raw = self.rfile.read(length)
                try:
                    body = json.loads(raw.decode("utf-8"))
                except ValueError:
                    body = {}
                with outer._lock:
                    outer.posts.append((self.path, body))
                    served = len(
                        [p for p in outer.posts if p[0] == self.path]
                    )
                    mode = dict(outer.mode)
                kind = mode.get("kind", "ok")

                if kind == "ok":
                    n = len(body.get("logs", [])) or 1
                    self._json(200, {"accepted": n, "rejected": 0})
                elif kind == "status":
                    self._json(mode["code"], {"error": "injected"})
                elif kind == "failN":
                    if served <= mode["n"]:
                        self._json(500, {"error": "injected 500"})
                    else:
                        n = len(body.get("logs", [])) or 1
                        self._json(200, {"accepted": n, "rejected": 0})
                elif kind == "partial":
                    self._json(200, mode["ack"])
                elif kind == "slow":
                    import time

                    time.sleep(mode["delay"])
                    n = len(body.get("logs", [])) or 1
                    self._json(200, {"accepted": n, "rejected": 0})
                elif kind == "reset":
                    # Destroy the connection before any response bytes.
                    try:
                        self.connection.close()
                    except OSError:
                        pass
                else:
                    self._json(500, {"error": f"unknown mode {kind}"})

            def _json(self, code: int, payload: Dict[str, Any]) -> None:
                data = json.dumps(payload).encode("utf-8")
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def log_message(self, *args: Any, **kwargs: Any) -> None:
                pass

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}"

    def set_mode(self, mode: Dict[str, Any]) -> None:
        with self._lock:
            self.mode = mode

    def posts_for(self, path: str) -> int:
        with self._lock:
            return len([p for p in self.posts if p[0] == path])

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)
