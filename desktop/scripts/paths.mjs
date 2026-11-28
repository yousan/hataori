import { readFileSync } from 'node:fs';
// git diff --name-only -z --no-renames includes both sides of a rename and the
// complete PR/push range, so a final docs commit cannot hide desktop inputs.
const files = readFileSync(0, 'utf8').split('\0').filter(Boolean);
const relevant = files.some(file => {
  // This is a proven non-executable package document. Do not turn a blanket
  // Markdown exemption into a bypass for policy, embedded prompts or fixtures.
  if (file === 'desktop/README.md') return false;
  if (/(?:^|\/)(?:AGENTS|CLAUDE)\.md$/u.test(file) || file.startsWith('.github/')) return true;
  if (['docs/testing.md', 'docs/design/ci-policy.md', 'docs/protocol.md', 'docs/plugins.md', 'docs/configuration.md'].includes(file)) return true;
  return /^(?:desktop\/|web\/|cmd\/|internal\/|go\.(?:mod|sum)$)/u.test(file);
});
console.log(relevant ? 'true' : 'false');
