// No credentials or network API here: the trusted-tag entry supplies request.
// Keeping the publication boundary separate makes refusal tests scratch-only.
export function requireDraft(release, tag, commit) {
  if (!release || release.draft !== true || release.tag_name !== tag || release.target_commitish !== commit || !Number.isSafeInteger(release.id)) throw new Error('An exact-source unpublished maintainer draft is required');
  return release;
}

export async function uploadDraftFiles({ repository, release, tag, commit, files, request }) {
  for (const [name, body] of files) {
    // Only the maintainer can publish the draft, and #1074 must keep it draft
    // throughout delivery. This client never calls PATCH, publish or delete.
    const current = requireDraft((await request(`https://api.github.com/repos/${repository}/releases/${release.id}`)).body, tag, commit);
    if (current.assets?.some(asset => asset.name === name)) throw new Error('Existing draft asset preserved; no overwrite');
    await request(`https://uploads.github.com/repos/${repository}/releases/${release.id}/assets?name=${encodeURIComponent(name)}`, {
      method: 'POST', headers: { 'Content-Type': name.endsWith('.dmg') ? 'application/octet-stream' : 'text/plain' }, body,
    });
  }
}
