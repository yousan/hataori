import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtemp, writeFile, rm, symlink } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { verifyManifest } from '../src/manifest.mjs';

test('bundled version, architecture, file set and checksums are checked before execution', async t => {
  const dir = await mkdtemp(path.join(tmpdir(), 'desktop-manifest-'));
  t.after(() => rm(dir, { recursive: true }));
  const manifest = { format: 1, version: '1.0.0', commit: '1'.repeat(40), platform: 'darwin', arch: 'arm64', files: {} };
  for (const name of ['hataori', 'hataori-desktop-transport']) {
    await writeFile(path.join(dir, name), name, { mode: 0o700 });
    manifest.files[name] = createHash('sha256').update(name).digest('hex');
  }
  await writeFile(path.join(dir, 'desktop-manifest.json'), JSON.stringify(manifest));
  await verifyManifest(dir, '1.0.0', 'arm64');
  await assert.rejects(verifyManifest(dir, '1.0.1', 'arm64'));
  await assert.rejects(verifyManifest(dir, '1.0.0', 'x64'));
  await writeFile(path.join(dir, 'hataori'), 'changed');
  await assert.rejects(verifyManifest(dir, '1.0.0', 'arm64'));
  await rm(path.join(dir, 'hataori'));
  await symlink('hataori-desktop-transport', path.join(dir, 'hataori'));
  await assert.rejects(verifyManifest(dir, '1.0.0', 'arm64'));
});
