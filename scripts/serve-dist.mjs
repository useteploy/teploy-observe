/* Tiny SPA static server: serves ./dist files, falls back to index.html for
 * app routes — the same contract the observe binary's static adapter expects
 * (allowAppRoutes). Usage: node serve-dist.mjs <port> */
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { extname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

// fileURLToPath, not .pathname: the workspace path contains a space, which the
// URL encodes as %20 and .pathname would keep literally.
const root = fileURLToPath(new URL("../cmd/observe/ui/dist", import.meta.url));
const port = Number(process.argv[2] ?? 8931);

const types = {
  ".html": "text/html; charset=utf-8", ".js": "text/javascript", ".css": "text/css",
  ".svg": "image/svg+xml", ".json": "application/json", ".br": "application/octet-stream",
  ".gz": "application/octet-stream", ".txt": "text/plain; charset=utf-8",
};

createServer(async (req, res) => {
  try {
    const path = normalize(decodeURIComponent(new URL(req.url, "http://x").pathname));
    const file = join(root, path);
    if (file.startsWith(root)) {
      const data = await readFile(file).catch(() => null);
      if (data) {
        res.writeHead(200, { "content-type": types[extname(file)] ?? "application/octet-stream" });
        res.end(data);
        return;
      }
    }
    const shell = await readFile(join(root, "index.html"));
    res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
    res.end(shell);
  } catch (e) {
    res.writeHead(500);
    res.end("err: " + (e?.code ?? e?.message ?? e));
  }
}).listen(port, "127.0.0.1", () => console.log(`spa server on :${port}`));
