#!/usr/bin/env python3
"""Scrub ONE private run into a NEW directory; never edit raw recordings.

python3 scrub.py PRIVATE_RAW NEW_OUTPUT
All Slack IDs, UUIDs, names and URLs are replaced consistently. Message times
are shifted together, keeping timestamp ordering and deltas. Review before git.
"""

import decimal
import json
from pathlib import Path
import re
import sys

ID = re.compile(r"(?<![A-Za-z0-9])(?:[UTCAFBGDWE][A-Z0-9]{8,}|Ev[A-Za-z0-9]{8,})(?![A-Za-z0-9])")
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", re.I)
TOKEN = re.compile(r"(?:xox[bpe]-|xapp-)[A-Za-z0-9-]+")
URL = re.compile(r"(?:https?|wss)://[^\s<>|\"\\]+")
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
HOME = re.compile(r"(?:/home/|/Users/)[^\s<>\"\\]+")
TIMESTAMP = re.compile(r"(?<![0-9])[0-9]{10}\.[0-9]{6}(?![0-9])")
NAMES = {"name", "username", "real_name", "display_name", "team", "user",
         "first_name", "last_name", "real_name_normalized", "display_name_normalized",
         "channel_name", "domain"}
OPAQUE = {"block_id", "trigger_id", "cursor", "next_cursor", "event_context", "host"}


def strings(v):
    if isinstance(v, dict):
        for k, val in v.items():
            yield k
            yield from strings(val)
    elif isinstance(v, list):
        for val in v:
            yield from strings(val)
    elif isinstance(v, str):
        yield v


def scrub(raw, out):
    docs = [(p.name, json.loads(p.read_text())) for p in sorted(raw.glob("*.json"))]
    if not docs:
        raise ValueError("no recordings")
    all_strings = [s for _, d in docs for s in strings(d)]
    ids = sorted({m.group() for s in all_strings for m in ID.finditer(s)})
    idmap = {v: ("Ev" if v.startswith("Ev") else v[0])+f"{i+1:010d}" for i, v in enumerate(ids)}
    uuids, urls, names, opaques, emails = {}, {}, {}, {}, {}
    times = [decimal.Decimal(m.group()) for s in all_strings for m in TIMESTAMP.finditer(s)]
    offset = min(times).to_integral_value(rounding=decimal.ROUND_FLOOR)-decimal.Decimal(1000000000) if times else 0

    # Collect names first so their occurrences in other fields are removed too.
    def collect(v):
        if isinstance(v, dict):
            for k, x in v.items():
                if k in NAMES and isinstance(x, str) and x and not ID.fullmatch(x):
                    names.setdefault(x, "name-"+str(len(names)+1))
                collect(x)
        elif isinstance(v, list):
            for x in v:
                collect(x)
    for _, d in docs:
        collect(d)

    def text(s):
        s = TOKEN.sub("redacted-credential", s)
        s = URL.sub(lambda m: urls.setdefault(m.group(), "https://example.invalid/redacted-url-"+str(len(urls)+1)), s)
        s = EMAIL.sub(lambda m: emails.setdefault(m.group(), "user"+str(len(emails)+1)+"@example.com"), s)
        s = HOME.sub("REDACTED-PATH", s)
        s = ID.sub(lambda m: idmap[m.group()], s)
        s = UUID.sub(lambda m: uuids.setdefault(m.group(), f"00000000-0000-0000-0000-{len(uuids)+1:012d}"), s)
        s = TIMESTAMP.sub(lambda m: f"{decimal.Decimal(m.group())-offset:.6f}", s)
        # Replace whole name occurrences, including the auth.test team/user names.
        for real in sorted(names, key=len, reverse=True):
            s = re.sub(r"(?<![\w-])"+re.escape(real)+r"(?![\w-])", lambda _: names[real], s)
        return s

    def fix(v, key=""):
        if isinstance(v, dict):
            return {text(k): fix(x, k) for k, x in v.items()}
        if isinstance(v, list):
            return [fix(x, key) for x in v]
        if isinstance(v, str):
            if key in {"token", "response_url", "upload_url"}:
                return "redacted-credential"
            if key in OPAQUE and v:
                return opaques.setdefault(v, "opaque-"+str(len(opaques)+1))
            return text(v)
        if key in {"event_time", "date_created", "date_updated", "created", "updated", "timestamp"} and isinstance(v, (int, float)) and v >= 1000000000:
            return float(decimal.Decimal(str(v))-offset) if isinstance(v, float) else v-int(offset)
        return v

    cleaned = [(name, fix(d)) for name, d in docs]
    out.mkdir(mode=0o700, parents=False, exist_ok=False)
    for name, doc in cleaned:
        (out/name).write_text(json.dumps(doc, indent=2, ensure_ascii=False)+"\n")
    print(f"Scrubbed {len(docs)} records; review output before committing")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    scrub(Path(sys.argv[1]), Path(sys.argv[2]))
