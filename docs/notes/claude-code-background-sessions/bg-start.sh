#!/bin/sh
# bg-start.sh <run-dir> <session-uuid> <prompt> [claude args...]: claude --bg from a script,
# with no terminal (stdin from /dev/null), in <run-dir>/work, with the run's hook settings.
# Make <run-dir> with ../claude-code-channels/mkrun.sh.
set -eu
run=$1 sid=$2 prompt=$3; shift 3
CL=${CLAUDE_BIN:-$HOME/.local/bin/claude}
cd "$run/work"
env -i HOME="$HOME" PATH="$PATH" LANG=C.UTF-8 \
  "$CL" --bg --model sonnet --session-id "$sid" --settings "$run/settings.json" "$@" "$prompt" </dev/null
