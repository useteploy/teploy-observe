// Embedding does not need build wall-clock timestamps. Neutron currently emits
// them unconditionally (reported in Teploy/_internal/UPSTREAM_BUGS.md).
// Preserve policy content and accurate compression counts while removing time.
import { readFileSync, writeFileSync } from 'node:fs';
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
