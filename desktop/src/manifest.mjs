import { createHash } from 'node:crypto';
import { readFile, lstat } from 'node:fs/promises';
import path from 'node:path';

const names = ['hataori', 'hataori-desktop-transport'];
export async function verifyManifest(resources, version, arch = process.arch) {
  const manifest = JSON.parse(await readFile(path.join(resources, 'desktop-manifest.json'), 'utf8'));
  if (manifest.format !== 1 || manifest.platform !== 'darwin' || manifest.arch !== arch ||
      manifest.version !== version || !/^[0-9a-f]{40}$/u.test(manifest.commit) ||
      !manifest.files || Object.keys(manifest.files).sort().join() !== [...names].sort().join()) throw new Error('Incompatible desktop manifest');
  for (const name of names) {
    const file = path.join(resources, name);
    const info = await lstat(file);
    if (!info.isFile() || info.nlink !== 1 || (info.mode & 0o022) !== 0 || (info.mode & 0o111) === 0) throw new Error('Untrusted bundled executable');
    const digest = createHash('sha256').update(await readFile(file)).digest('hex');
    if (manifest.files[name] !== digest) throw new Error('Bundled executable checksum mismatch');
  }
  return manifest;
}
