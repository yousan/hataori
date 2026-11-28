import assert from 'node:assert/strict';
import { test } from 'node:test';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { requireDraft, uploadDraftFiles } from '../scripts/draft-api.mjs';

const tag = 'v1.0.0', commit = '1'.repeat(40);
const draft = { id: 41, tag_name: tag, target_commitish: commit, draft: true, assets: [] };
test('unsigned delivery refuses published, mismatched or existing assets before upload', async () => {
  for (const release of [{ ...draft, draft: false }, { ...draft, target_commitish: 'main' }, { ...draft, tag_name: 'v0.9.0' }]) assert.throws(() => requireDraft(release, tag, commit));
  for (const current of [{ ...draft, draft: false }, { ...draft, assets: [{ name: 'app.dmg' }] }]) {
    let uploads = 0;
    const request = async (_url, options = {}) => { if (options.method) uploads++; return { body: current }; };
    await assert.rejects(uploadDraftFiles({ repository: 'fixture/repository', release: draft, tag, commit, files: [['app.dmg', Buffer.from('inert')]], request }));
    assert.equal(uploads, 0);
  }
  const calls = [];
  const request = async (url, options) => { calls.push({ url, method: options?.method }); return { body: draft }; };
  await uploadDraftFiles({ repository: 'fixture/repository', release: draft, tag, commit, files: [['app.dmg', Buffer.from('inert')]], request });
  assert.deepEqual(calls.map(c => c.method), [undefined, 'POST']);
  assert.ok(calls[1].url.startsWith('https://uploads.github.com/repos/fixture/repository/releases/41/assets?'));
});

test('desktop path gate includes cumulative source/build inputs and both rename sides but excludes docs', () => {
  const classify = files => {
    const result = spawnSync(process.execPath, [fileURLToPath(new URL('../scripts/paths.mjs', import.meta.url))], { input: files.join('\0') + '\0', encoding: 'utf8' });
    assert.equal(result.status, 0); return result.stdout.trim();
  };
  assert.equal(classify(['docs/notes.md', 'README.md', 'desktop/README.md']), 'false');
  assert.equal(classify(['desktop/src/main.mjs', 'docs/last-commit.md']), 'true');
  assert.equal(classify(['desktop/README.md', 'desktop/src/main.mjs']), 'true');
  for (const policy of ['AGENTS.md', 'web/AGENTS.md', '.github/filters.yml', 'docs/design/ci-policy.md', 'docs/protocol.md']) assert.equal(classify([policy]), 'true');
  for (const executableMarkdown of ['internal/supervisor/layer.md', 'internal/store/testdata/README.md', 'cmd/hataori/testdata/supervisor-layers/none.md']) assert.equal(classify([executableMarkdown]), 'true');
  assert.equal(classify(['docs/moved.md', 'internal/dashboard/page/old.js']), 'true');
  assert.equal(classify(['.github/workflows/desktop.yml']), 'true');
  assert.equal(classify(['internal/core/core.go']), 'true');
});
