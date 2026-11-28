import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtemp, mkdir, writeFile, symlink, readlink, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { stageDmgSource } from '../scripts/dmg-source.mjs';

test('DMG root contains the app bundle with portable framework links', async t => {
  const scratch = await mkdtemp(join(tmpdir(), 'hataori-dmg-test-'));
  t.after(() => rm(scratch, { recursive: true, force: true }));
  const app = join(scratch, 'hataori.app');
  const versions = join(app, 'Contents/Frameworks/Fixture.framework/Versions');
  await mkdir(join(versions, 'A'), { recursive: true });
  await writeFile(join(versions, 'A/Fixture'), 'inert framework');
  await symlink('A', join(versions, 'Current'));
  await symlink('Versions/Current/Fixture', join(versions, '../Fixture'));
  const staged = await stageDmgSource(app, scratch);
  assert.equal(staged.app, join(staged.directory, 'hataori.app'));
  const copied = join(staged.app, 'Contents/Frameworks/Fixture.framework');
  assert.equal(await readlink(join(copied, 'Versions/Current')), 'A');
  assert.equal(await readlink(join(copied, 'Fixture')), 'Versions/Current/Fixture');
  // The mounted app must still work after the source build tree disappears.
  await rm(app, { recursive: true });
  assert.equal(await readFile(join(copied, 'Fixture'), 'utf8'), 'inert framework');
});
