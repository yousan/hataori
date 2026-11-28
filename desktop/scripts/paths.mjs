import { readFileSync } from 'node:fs';
// git diff --name-only -z --no-renames includes both sides of a rename and the
// complete PR/push range, so a final docs commit cannot hide desktop inputs.
const files = readFileSync(0, 'utf8').split('\0').filter(Boolean);
const relevant = files.some(file => /^(?:desktop\/|web\/|cmd\/|internal\/|go\.(?:mod|sum)$|\.github\/workflows\/desktop\.yml$)/u.test(file));
console.log(relevant ? 'true' : 'false');
