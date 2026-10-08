import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { parsePhase } from './args.mjs';
test('published measurement commands select distinct phases', () => {
  const pkg = JSON.parse(readFileSync(new URL('../package.json', import.meta.url)));
  for (const phase of ['lcp', 'session']) {
    assert.equal(parsePhase(pkg.scripts[`measure:${phase}`].split(' ').slice(2)), phase);
    assert.equal(parsePhase([`--phase=${phase}`]), phase);
  }
  assert.equal(parsePhase([]), 'lcp');
});
test('invalid, missing, extra and duplicate arguments are rejected', () => {
  for (const args of [['--phase'], ['--phase','typo'], ['--unknown'], ['--phase=lcp','--phase=session'], ['--phase=session','extra']]) assert.throws(() => parsePhase(args));
});
