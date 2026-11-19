#!/usr/bin/env python3
"""Turn raw captures from capture.py into committable testdata.

usage: scrub.py OUT_DIR RAW_DIR...

Each RAW_DIR becomes OUT_DIR/<basename of RAW_DIR>. OUT_DIR must not exist yet.
The same value is replaced the same way in every file:

  snowflakes (17-20 digits, anywhere) -> 1000000000000000001 and up, in the order of the
                                          real ids, so that comparing ids still orders them
  32-digit hex values (hashes, session ids, rate-limit buckets) -> 000...0N
  interaction tokens (also in paths)  -> REDACTED-TOKEN-N (in /interactions/ and /webhooks/ paths)
  signed URL parameters (ex, is, hm)  -> ex=00000000&is=00000000&hm=REDACTED
  user, guild, channel and role names -> name-N (thread names, test texts and emoji are kept)
  e-mail addresses                    -> userN@example.com
  READY's _trace and resume URL       -> fixed values
  profile tags, discriminators        -> TAG, 0000

GUILD_CREATE and GUILD_ROLE_UPDATE are dropped: the driver does not read them, and they
hold the server's member and role lists. created.json is dropped.
"""

import json, os, re, sys

SNOWFLAKE = re.compile(r"(?<![0-9])[0-9]{17,20}(?![0-9])")
HEX32 = re.compile(r"(?<![0-9a-f])(?:a_)?[0-9a-f]{32}(?![0-9a-f])")
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
SIGNED = re.compile(r"\?ex=[0-9a-f]+&is=[0-9a-f]+&hm=[0-9a-f]+&?")
NAME_KEYS = {"username", "global_name", "nick", "display_name"}
DROP = {"GUILD_CREATE", "GUILD_ROLE_UPDATE"}
BASE = 1000000000000000000


def walk(v, f):
    if isinstance(v, dict):
        return {k: f(k, walk(x, f)) for k, x in v.items()}
    if isinstance(v, list):
        return [walk(x, f) for x in v]
    return v


def strings(v):
    if isinstance(v, dict):
        for x in v.values():
            yield from strings(x)
    elif isinstance(v, list):
        for x in v:
            yield from strings(x)
    elif isinstance(v, str):
        yield v
    elif isinstance(v, int) and not isinstance(v, bool):
        yield str(v)


def main():
    out, raws = sys.argv[1], sys.argv[2:]
    if os.path.exists(out):
        sys.exit(f"{out} exists")
    docs = {}
    for r in raws:
        for n in sorted(os.listdir(r)):
            if not n.endswith(".json") or n == "created.json":
                continue
            d = json.load(open(os.path.join(r, n)))
            if d.get("source") == "gateway" and d.get("t") in DROP:
                continue
            docs[(os.path.basename(r.rstrip("/")), n)] = d

    # Ids: every snowflake anywhere, mapped in numeric order.
    ids = set()
    for d in docs.values():
        for s in strings(d):
            ids.update(SNOWFLAKE.findall(s))
    idmap = {real: str(BASE + i + 1) for i, real in enumerate(sorted(ids, key=int))}
    hexmap, tokmap, namemap, mailmap = {}, {}, {}, {}

    def hexsub(m):
        return hexmap.setdefault(m.group(0), "%032x" % (len(hexmap) + 1))

    def name(v):
        if not isinstance(v, str) or v.startswith(("[hataori test]", "hataori-test")) or v == "":
            return v
        if not any(ch.isalnum() for ch in v):  # an emoji, such as a reaction's
            return v
        return namemap.setdefault(v, "name-%d" % (len(namemap) + 1))

    def fix(k, v):
        if k in ("token",) and isinstance(v, str):
            return tokmap.setdefault(v, "REDACTED-TOKEN-%d" % (len(tokmap) + 1))
        if k in NAME_KEYS:
            return name(v)
        if k == "path" and isinstance(v, str):
            return re.sub(r"(/(?:interactions|webhooks)/[0-9]+/)([^/]+)",
                          lambda m: m.group(1) + tokmap.setdefault(m.group(2), "REDACTED-TOKEN-%d" % (len(tokmap) + 1)), v)
        if k == "tag" and isinstance(v, str):
            return "TAG"
        if k == "discriminator" and isinstance(v, str) and v != "0":
            return "0000"
        if k == "_trace":
            return ["REDACTED"]
        if k == "resume_gateway_url":
            return "wss://gateway.discord.gg"
        if k == "name" and isinstance(v, str):
            return name(v)
        return v

    def text(s):
        s = SNOWFLAKE.sub(lambda m: idmap[m.group(0)], s)
        s = SIGNED.sub("?ex=00000000&is=00000000&hm=REDACTED&", s)
        s = HEX32.sub(hexsub, s)
        s = EMAIL.sub(lambda m: mailmap.setdefault(m.group(0), "user%d@example.com" % (len(mailmap) + 1)), s)
        return s

    def scalars(v):
        if isinstance(v, dict):
            return {text(k): scalars(x) for k, x in v.items()}
        if isinstance(v, list):
            return [scalars(x) for x in v]
        if isinstance(v, str):
            return text(v)
        return v

    for (sub, n), d in docs.items():
        d = scalars(walk(d, fix))
        os.makedirs(os.path.join(out, sub), exist_ok=True)
        with open(os.path.join(out, sub, n), "w") as f:
            json.dump(d, f, indent=1, ensure_ascii=False, sort_keys=True)
            f.write("\n")
    print(f"{len(docs)} files, {len(idmap)} ids, {len(hexmap)} hashes, {len(tokmap)} tokens, "
          f"{len(namemap)} names", file=sys.stderr)


if __name__ == "__main__":
    main()
