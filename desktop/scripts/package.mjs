// Run only on a disposable standard hosted macOS runner. Build outputs stay
// local to that runner; this script has no publication or credential API.
import packager from '@electron/packager';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFile, writeFile, mkdir, mkdtemp, cp } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { verifyManifest } from '../src/manifest.mjs';

if (process.platform !== 'darwin' || process.env.CI !== 'true') throw new Error('Disposable hosted macOS CI required');
const desktop = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const repository = path.dirname(desktop);
const commit = execFileSync('git', ['rev-parse', 'HEAD'], { cwd: repository, encoding: 'utf8' }).trim();
if (!/^[0-9a-f]{40}$/u.test(commit)) throw new Error('Invalid source SHA');
const tag = process.env.GITHUB_REF_TYPE === 'tag' ? process.env.GITHUB_REF_NAME : '';
if (tag && !/^v\d+\.\d+\.\d+$/u.test(tag)) throw new Error('Expected one stable release tag');
const version = tag ? tag.slice(1) : `0.0.0-dev.${commit}`;
const arch = process.arch;
if (!['x64', 'arm64'].includes(arch)) throw new Error('Unsupported runner architecture');
const resources = path.join(desktop, 'resources');
await mkdir(resources, { recursive: true, mode: 0o700 });
const manifest = { format: 1, version, commit, platform: 'darwin', arch, files: {} };
for (const [name, command] of [['hataori', './cmd/hataori'], ['hataori-desktop-transport', './cmd/hataori-desktop-transport']]) {
  const target = path.join(resources, name);
  execFileSync('go', ['build', '-trimpath', '-ldflags', `-X github.com/yousan/hataori/internal/version.Version=${version}`, '-o', target, command], {
    cwd: repository, env: { ...process.env, CGO_ENABLED: '1' }, stdio: 'inherit',
  });
  manifest.files[name] = createHash('sha256').update(await readFile(target)).digest('hex');
}
const metadata = JSON.parse(execFileSync(path.join(resources, 'hataori'), ['desktop', 'metadata'], { encoding: 'utf8' }));
if (metadata.version !== version || metadata.commit !== commit) throw new Error('Daemon build identity mismatch');
Object.assign(manifest, { schema: metadata.schema, control_protocol: metadata.control_protocol, host_capability: metadata.host_capability });
await writeFile(path.join(resources, 'desktop-manifest.json'), JSON.stringify(manifest, null, 2) + '\n', { mode: 0o600, flag: 'wx' });
await verifyManifest(resources, version, arch);
const pkg = JSON.parse(await readFile(path.join(desktop, 'package.json'), 'utf8'));
// Electron reads package.json.version at runtime. appVersion alone only stamps
// platform metadata, so stamp a private package copy from the same value too.
await mkdir(path.join(desktop, 'out'), { recursive: true });
const staged = await mkdtemp(path.join(desktop, 'out', 'source-'));
await cp(path.join(desktop, 'src'), path.join(staged, 'src'), { recursive: true });
await mkdir(path.join(staged, 'node_modules'), { recursive: true });
await cp(path.join(desktop, 'node_modules/ws'), path.join(staged, 'node_modules/ws'), { recursive: true });
const stagedPackage = { ...pkg, version };
delete stagedPackage.devDependencies;
delete stagedPackage.scripts;
await writeFile(path.join(staged, 'package.json'), JSON.stringify(stagedPackage, null, 2) + '\n', { flag: 'wx' });
const output = await packager({ dir: staged, name: 'hataori', platform: 'darwin', arch,
  out: path.join(desktop, 'out'), appVersion: version, buildVersion: tag ? version : '0.0.0',
  electronVersion: pkg.devDependencies.electron, appBundleId: 'io.hataori.desktop',
  asar: true, prune: false, osxSign: false, osxNotarize: false,
  extraResource: ['hataori', 'hataori-desktop-transport', 'desktop-manifest.json'].map(name => path.join(resources, name)),
  ignore: [/^\/out(?:\/|$)/u, /^\/resources(?:\/|$)/u, /^\/test(?:\/|$)/u, /^\/scripts(?:\/|$)/u],
});
if (output.length !== 1) throw new Error('Expected one architecture-specific app');
const app = path.join(output[0], 'hataori.app');
await verifyManifest(path.join(app, 'Contents/Resources'), version, arch);
const runtimeVersion = execFileSync(path.join(app, 'Contents/MacOS/hataori'), ['--version'], { encoding: 'utf8' }).trim();
if (runtimeVersion !== version) throw new Error('Packaged runtime/package/manifest version mismatch');
const dmg = path.join(desktop, 'out', `hataori-${version}-${arch}.dmg`);
execFileSync('/usr/bin/hdiutil', ['create', '-volname', 'hataori', '-srcfolder', app, '-format', 'UDZO', dmg], { stdio: 'inherit' });
const digest = createHash('sha256').update(await readFile(dmg)).digest('hex');
await writeFile(dmg + '.sha256', `${digest}  ${path.basename(dmg)}\n`, { flag: 'wx' });
console.log(`Built ${path.basename(dmg)} from ${commit}; unsigned, unpublished, no artifact upload`);
