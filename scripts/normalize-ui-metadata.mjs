// Embedding does not need build wall-clock timestamps. Neutron currently emits
// them unconditionally (reported in Teploy/_internal/UPSTREAM_BUGS.md).
// Preserve policy content and accurate compression counts while removing time.
import { readFileSync, writeFileSync, readdirSync } from 'node:fs';
import { resolve } from 'node:path';
import { gzipSync, brotliCompressSync, constants } from 'node:zlib';

if (!process.argv[2]) throw new Error('usage: node scripts/normalize-ui-metadata.mjs <dist>');
const dist = resolve(process.argv[2]);
const policyPath = resolve(dist, '.neutron-static-policy.json');
const metaPath = resolve(dist, '.neutron-adapter-static.json');
const original = readFileSync(policyPath);
const policy = JSON.parse(original);
const meta = JSON.parse(readFileSync(metaPath));
delete policy.generatedAt;
delete meta.generatedAt;
const normalized = Buffer.from(JSON.stringify(policy, null, 2));
for (const [suffix, field, compress] of [
  ['gz', 'gzipBytesSaved', b => gzipSync(b, { level: 9 })],
  ['br', 'brotliBytesSaved', b => brotliCompressSync(b, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } })],
]) {
  const previous = readFileSync(`${policyPath}.${suffix}`);
  const compressed = compress(normalized);
  meta.compression[field] += (normalized.length - compressed.length) - (original.length - previous.length);
  writeFileSync(`${policyPath}.${suffix}`, compressed);
}
writeFileSync(policyPath, normalized);
writeFileSync(metaPath, JSON.stringify(meta, null, 2));

// gzip's OS header differs on macOS/Linux despite identical deflate payloads.
// RFC 1952 value 255 means unknown OS; this byte is outside the payload CRC.
function normalizeGzipHeaders(dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = resolve(dir, entry.name);
    if (entry.isDirectory()) normalizeGzipHeaders(path);
    else if (entry.name.endsWith('.gz')) {
      const bytes = readFileSync(path);
      if (bytes[0] !== 0x1f || bytes[1] !== 0x8b || bytes[3] !== 0) throw new Error(`Unexpected gzip header: ${path}`);
      bytes[9] = 255;
      writeFileSync(path, bytes);
    }
  }
}
normalizeGzipHeaders(dist);
