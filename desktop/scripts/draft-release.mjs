// Trusted-tag delivery only. No asset is uploaded to Actions or to a published
// release. A maintainer publishes/signs only through the later #1074 decision.
import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createHash } from 'node:crypto';
import { verifyManifest } from '../src/manifest.mjs';
import { requireDraft, uploadDraftFiles } from './draft-api.mjs';

const tag = process.env.GITHUB_REF_NAME, repository = process.env.GITHUB_REPOSITORY, commit = process.env.DESKTOP_HEAD;
if (process.env.GITHUB_ACTIONS !== 'true' || process.env.RUNNER_ENVIRONMENT !== 'github-hosted' || process.env.GITHUB_EVENT_NAME !== 'push' || process.env.GITHUB_REF_TYPE !== 'tag' || process.platform !== 'darwin' || repository !== 'yousan/hataori' || !/^v\d+\.\d+\.\d+$/u.test(tag) || !/^[0-9a-f]{40}$/u.test(commit)) throw new Error('Trusted hosted stable tag required');
const directory = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const version = tag.slice(1), arch = process.arch;
const manifest = await verifyManifest(path.join(directory, 'resources'), version, arch);
if (manifest.commit !== commit) throw new Error('Release manifest source mismatch');
const dmg = `hataori-${version}-${arch}.dmg`;
const names = [dmg, dmg + '.sha256'];
const present = await readdir(path.join(directory, 'out'));
for (const name of names) if (!present.includes(name)) throw new Error('Missing checked DMG or checksum');
const bytes = await readFile(path.join(directory, 'out', dmg));
const expected = createHash('sha256').update(bytes).digest('hex') + '  ' + dmg + '\n';
if (await readFile(path.join(directory, 'out', dmg + '.sha256'), 'utf8') !== expected) throw new Error('DMG checksum mismatch');
const api = 'https://api.github.com/repos/' + repository;
const headers = { Authorization: 'Bearer ' + process.env.GH_TOKEN, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' };
if (!process.env.GH_TOKEN) throw new Error('Trusted tag release authority unavailable');
async function request(url, options = {}, allowed = []) {
  const response = await fetch(url, { ...options, headers: { ...headers, ...options.headers }, redirect: 'error' });
  if (!response.ok && !allowed.includes(response.status)) throw new Error('Draft release API refused status ' + response.status);
  return { status: response.status, body: await response.json() };
}
const draftOnly = release => requireDraft(release, tag, commit);
// Draft releases are omitted from the public tag lookup. Authenticated release
// listing includes drafts for repository writers; enumerate only this tag.
async function findDraft() {
  for (let page = 1; page <= 10; page++) {
    const { body } = await request(api + `/releases?per_page=100&page=${page}`);
    if (!Array.isArray(body)) throw new Error('Invalid draft release response');
    const matches = body.filter(item => item.tag_name === tag);
    if (matches.length > 1) throw new Error('Ambiguous release tag');
    if (matches.length === 1) return draftOnly(matches[0]);
    if (body.length < 100) return null;
  }
  throw new Error('Release listing limit; maintainer must identify the draft');
}
let release = await findDraft();
if (!release) {
  const created = await request(api + '/releases', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ tag_name: tag, target_commitish: commit, name: `hataori ${version} (unsigned maintainer acceptance)`, draft: true, prerelease: true,
      body: 'Unsigned macOS acceptance files only. Do not publish these assets before the signing and distribution decision in #1074. Hosted checks do not replace real-Mac acceptance.' }) }, [422]);
  // The other architecture may have created the same draft concurrently. This
  // single identity reconciliation never reruns a build or overwrites assets.
  release = created.status === 422 ? await findDraft() : draftOnly(created.body);
  if (!release) throw new Error('Maintainer draft could not be established');
}
const manifestName = `hataori-${version}-${arch}-manifest.json`;
const files = [...names.map(name => [name, path.join(directory, 'out', name)]), [manifestName, path.join(directory, 'resources', 'desktop-manifest.json')]];
await uploadDraftFiles({ repository, release, tag, commit, files: await Promise.all(files.map(async ([name, file]) => [name, await readFile(file)])), request });
console.log(`Delivered ${arch} ${commit} only to unpublished draft release ${release.id}`);
