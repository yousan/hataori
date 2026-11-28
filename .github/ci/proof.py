#!/usr/bin/env python3
"""Validate only GitHub-provided event/environment data; no token or network."""
import hashlib
import json
import os
import re
import sys


def validate(event, env):
    expected = {"intent", "from", "target", "control", "digest", "authority", "scope", "title", "purpose"}
    inputs = event.get("inputs", {})
    if set(inputs) != expected or env.get("GITHUB_EVENT_NAME") != "workflow_dispatch":
        raise ValueError("dispatch contract")
    if env.get("GITHUB_RUN_ATTEMPT") != "1":
        raise ValueError("reruns are not proof")
    for name in ("from", "target", "control"):
        if not re.fullmatch(r"(?:[0-9a-f]{40}|[0-9a-f]{64})", inputs[name]):
            raise ValueError("immutable revision required")
    for name in ("digest", "authority"):
        if not re.fullmatch(r"[0-9a-f]{64}", inputs[name]):
            raise ValueError("approved digest required")
    if not inputs["intent"] or inputs["scope"] not in ("true", "false") or inputs["purpose"] not in ("sweep", "development"):
        raise ValueError("scope or purpose")
    sweep = dict(ID=inputs["intent"], From=inputs["from"], Target=inputs["target"], Control=inputs["control"], Digest=inputs["digest"], Authority=inputs["authority"], CompatibilityRequired=inputs["scope"] == "true")
    title = "integration-sweep " + hashlib.sha256(json.dumps(sweep, separators=(",", ":"), ensure_ascii=False).encode()).hexdigest()
    ref = "refs/heads/ci-sweeps/" + hashlib.sha256(inputs["intent"].encode()).hexdigest()
    if inputs["title"] != title or env.get("GITHUB_SHA") != inputs["target"] or env.get("GITHUB_REF") != ref:
        raise ValueError("frozen tuple mismatch")
    if inputs["purpose"] == "development" and inputs["scope"] != "false":
        raise ValueError("development cannot claim compatibility")
    return sweep


if __name__ == "__main__":
    try:
        with open(os.environ["GITHUB_EVENT_PATH"], encoding="utf-8") as stream:
            validate(json.load(stream), os.environ)
    except (ValueError, KeyError, OSError, TypeError):
        print("invalid immutable dispatch contract", file=sys.stderr)
        sys.exit(1)
