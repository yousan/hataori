#!/bin/sh
# busy-test.sh <run-dir>: one push that starts a 25-second command, two more while it runs.
set -eu
R=$1; B=$(dirname "$0"); S=/tmp/hspk-$(basename "$R").sock
push() { /usr/bin/python3 "$B/push.py" "$S" message "$@"; }
T0=$(date +%s.%N)
push 10 "Run this exact command in the foreground, not in the background: python3 -c \"import time; time.sleep(25)\". When it has finished, reply over the channel with the word slept."
sleep 8
push 11 "Second message: reply over the channel with the word two."
sleep 2
push 12 "Third message: reply over the channel with the word three."
sleep 55
/usr/bin/python3 "$B/timeline.py" "$R" "$T0"
