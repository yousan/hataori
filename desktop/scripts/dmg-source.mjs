import { cp, mkdtemp } from 'node:fs/promises';
import { basename, join } from 'node:path';

// hdiutil consumes a directory containing the app bundle, not the bundle's
// Contents directory. Electron's framework symlinks must stay relative to the
// copied app; fs.cp's default link rewriting would bind them to the build tree.
export async function stageDmgSource(app, output) {
  const directory = await mkdtemp(join(output, 'dmg-source-'));
  const copiedApp = join(directory, basename(app));
  await cp(app, copiedApp, { recursive: true, verbatimSymlinks: true });
  return { directory, app: copiedApp };
}
