// Assemble the external Observe app in a disposable, pinned Neutron checkout.
// The patch adds only this importer and its missing dependency to the lockfile.
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { execFileSync } from 'node:child_process';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
if (!process.argv[2]) throw new Error('usage: node .ci/prepare-ui-workspace.mjs <isolated-neutron-checkout>');
const workspace = resolve(process.argv[2], 'typescript');
const app = resolve(workspace, 'apps/observe');
if (existsSync(app)) throw new Error(`Refusing to overwrite an existing app: ${app}`);
execFileSync('patch', ['--dry-run', '-p1', '-i', resolve(root, '.ci/observe-workspace.patch')], { cwd: workspace, stdio: 'inherit' });
execFileSync('patch', ['-p1', '-i', resolve(root, '.ci/observe-workspace.patch')], { cwd: workspace, stdio: 'inherit' });
mkdirSync(app, { recursive: true });
const pkg = JSON.parse(readFileSync(resolve(root, 'ui/package.json'), 'utf8'));
for (const [oldName, newName] of [['neutron', '@neutron-build/core'], ['neutron-cli', '@neutron-build/cli']]) {
  pkg.dependencies[newName] = pkg.dependencies[oldName];
  delete pkg.dependencies[oldName];
}
writeFileSync(resolve(app, 'package.json'), JSON.stringify(pkg, null, 2) + '\n');
writeFileSync(resolve(app, 'neutron.config.ts'), readFileSync(resolve(root, 'ui/neutron.config.ts'), 'utf8').replace('"neutron"', '"@neutron-build/core"'));
