"""A disposable pipe peer: record requests, emit only supplied real captures."""
import json
from pathlib import Path
import sys

requests = Path(sys.argv[1])
for line in sys.stdin:
    message = json.loads(line)
    if "fixture_response" in message:
        print(message["fixture_response"], flush=True)
    else:
        with requests.open("a") as output:
            output.write(line)
