// Shared fault-injection HTTP server for Node-run SDK tests (O11).
// Implements the fault matrix documented in sdk/testing/README.md so the
// browser SDK and sentry-shim tests exercise identical adverse transports.
// Language twins: sdk/go/*_test.go servers and sdk/python/tests/faultserver.py.

import http from "node:http";

export class FaultServer {
  constructor() {
    this.requests = []; // { path, headers, bodyText }
    this.mode = { kind: "ok" }; // ok | status | failN | partial | slow | reset | redirect
    this.server = http.createServer((req, res) => {
      let bodyText = "";
      req.on("data", (c) => (bodyText += c));
      req.on("end", () => {
        this.requests.push({ path: req.url, headers: req.headers, bodyText });
        const m = this.mode;
        switch (m.kind) {
          case "ok":
            res.writeHead(200, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ ok: true }));
            break;
          case "status":
            res.writeHead(m.code, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ error: `injected ${m.code}` }));
            break;
          case "failN":
            if (this.requests.length <= m.n) {
              res.writeHead(500, { "Content-Type": "application/json" });
              res.end(JSON.stringify({ error: "injected 500" }));
            } else {
              res.writeHead(200, { "Content-Type": "application/json" });
              res.end(JSON.stringify({ ok: true }));
            }
            break;
          case "partial":
            res.writeHead(200, { "Content-Type": "application/json" });
            res.end(JSON.stringify(m.ack));
            break;
          case "slow":
            setTimeout(() => {
              res.writeHead(200, { "Content-Type": "application/json" });
              res.end(JSON.stringify({ ok: true }));
            }, m.delayMs);
            break;
          case "reset":
            res.socket.destroy();
            break;
          case "redirect":
            res.writeHead(302, { Location: m.location });
            res.end();
            break;
          default:
            throw new Error(`unknown fault mode ${m.kind}`);
        }
      });
    });
  }

  url(path) {
    const addr = this.server.address();
    return `http://127.0.0.1:${addr.port}${path ?? ""}`;
  }

  start() {
    return new Promise((resolve) => this.server.listen(0, "127.0.0.1", resolve));
  }

  close() {
    return new Promise((resolve) => this.server.close(resolve));
  }
}
