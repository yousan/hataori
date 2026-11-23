#!/usr/bin/env python3
"""Capture GitHub REST responses for adapter tests."""

import ipaddress
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.request


HEADERS = {
    "Accept": "application/vnd.github+json",
    "X-GitHub-Api-Version": "2022-11-28",
    "User-Agent": "hataori-capture",
}
KEEP_HEADERS = {
    "content-type", "link", "retry-after", "x-ratelimit-limit",
    "x-ratelimit-remaining", "x-ratelimit-reset", "x-ratelimit-resource",
    "x-ratelimit-used", "x-github-api-version-selected",
}
REQUESTS = [
    ("issues-page-1.json", "/repos/urfave/cli/issues?direction=desc&page=1&per_page=5&sort=updated&state=all"),
    ("issues-page-2.json", "/repos/urfave/cli/issues?direction=desc&page=2&per_page=5&sort=updated&state=all"),
    ("issue.json", "/repos/cli/cli/issues/15104"),
    ("issue-comments.json", "/repos/cli/cli/issues/15104/comments?page=1&per_page=100"),
    ("pulls.json", "/repos/urfave/cli/pulls?direction=desc&page=1&per_page=5&sort=updated&state=all"),
    ("pull.json", "/repos/cli/cli/pulls/14910"),
    ("pull-reviews.json", "/repos/cli/cli/pulls/14910/reviews?page=1&per_page=100"),
    ("pull-review-comments.json", "/repos/cli/cli/pulls/14910/comments?page=1&per_page=100"),
    ("pull-comments.json", "/repos/cli/cli/issues/14910/comments?page=1&per_page=100"),
    ("pull-files.json", "/repos/cli/cli/pulls/14910/files?page=1&per_page=100"),
    ("pull-check-runs.json", "/repos/cli/cli/commits/{sha}/check-runs?page=1&per_page=100"),
    ("pull-status.json", "/repos/cli/cli/commits/{sha}/status?page=1&per_page=100"),
    ("ref-check-runs.json", "/repos/urfave/cli/commits/d5c06cb04479eeacbbb9378ea26261b6be516320/check-runs?page=1&per_page=100"),
    ("ref-status.json", "/repos/urfave/cli/commits/d5c06cb04479eeacbbb9378ea26261b6be516320/status?page=1&per_page=100"),
    ("not-found.json", "/repos/cli/cli/issues/999999999"),
]


class CaptureError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def fetch(opener, path, headers):
    request = urllib.request.Request("https://api.github.com" + path, headers=headers, method="GET")
    try:
        response = opener.open(request)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        status = response.status
        if 300 <= status < 400:
            raise CaptureError(f"Unexpected redirect: HTTP {status}")
        kept = {key.lower(): value for key, value in response.headers.items()
                if key.lower() in KEEP_HEADERS}
        body = json.load(response)
    return {"request": "GET " + path, "status": status, "headers": kept, "body": body}


def save(directory, name, answer):
    with (directory / name).open("w", encoding="utf-8") as output:
        json.dump(answer, output, indent=1, sort_keys=False)
        output.write("\n")
    print(f"{name} {answer['status']} remaining={answer['headers'].get('x-ratelimit-remaining', 'unknown')}")


def scrub_message(message):
    # Match address-shaped runs, then validate them before replacing them.
    pattern = r"(?<![\w:])(?:[0-9A-Fa-f:]*:[0-9A-Fa-f:]+|\d+(?:\.\d+){3})(?![\w:])"
    count = 0

    def replace(match):
        nonlocal count
        try:
            address = ipaddress.ip_address(match.group())
        except ValueError:
            return match.group()
        count += 1
        return "192.0.2.1" if address.version == 4 else "2001:db8::1"

    result = re.sub(pattern, replace, message)
    return result, count


def main():
    if len(sys.argv) != 3 or sys.argv[1] not in ("answers", "ratelimit"):
        raise CaptureError("Usage: python3 -I capture.py <answers|ratelimit> <out-dir>")
    step, directory = sys.argv[1], Path(sys.argv[2])
    token_file = os.environ.get("HATAORI_GITHUB_TOKEN_FILE")
    if step == "ratelimit" and token_file is not None:
        raise CaptureError("ratelimit requires HATAORI_GITHUB_TOKEN_FILE to be unset")
    headers = HEADERS.copy()
    if token_file is not None:
        token = Path(token_file).read_text(encoding="utf-8").strip()
        if not token:
            raise CaptureError("Token file is empty")
        headers["Authorization"] = "Bearer " + token
    directory.mkdir(parents=True, exist_ok=True)
    opener = urllib.request.build_opener(NoRedirect())
    if step == "answers":
        sha = None
        for name, path in REQUESTS:
            if "{sha}" in path:
                if sha is None:
                    raise CaptureError("Pull response did not contain a valid head SHA")
                path = path.format(sha=sha)
            answer = fetch(opener, path, headers)
            save(directory, name, answer)
            if name == "pull.json":
                try:
                    sha = answer["body"]["head"]["sha"]
                    if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-fA-F]{40}", sha):
                        sha = None
                except (KeyError, TypeError):
                    sha = None
    else:
        for _ in range(70):
            answer = fetch(opener, "/repos/cli/cli", headers)
            if answer["headers"].get("x-ratelimit-remaining") == "0":
                break
        else:
            raise CaptureError("Rate limit did not reach zero after 70 requests")
        answer = fetch(opener, "/repos/cli/cli/issues/15104", headers)
        if answer["status"] not in (403, 429):
            raise CaptureError(f"Expected rate-limit response, got HTTP {answer['status']}")
        if not isinstance(answer["body"], dict) or not isinstance(answer["body"].get("message"), str):
            raise CaptureError("Rate-limit response has no message")
        answer["body"]["message"], count = scrub_message(answer["body"]["message"])
        save(directory, "rate-limited.json", answer)
        print(f"IP addresses replaced: {count}")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Exceptions from libraries or the filesystem can contain credentials or paths.
        if isinstance(error, CaptureError):
            print(str(error), file=sys.stderr)
        else:
            print("Capture failed", file=sys.stderr)
        sys.exit(1)
