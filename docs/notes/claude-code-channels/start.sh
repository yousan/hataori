#!/bin/sh
# start.sh <run-dir> <session-uuid> [claude args...]: start the real interactive CLI in a
# tmux pane (private tmux socket "hspike"), in <run-dir>/work, with a clean environment.
set -eu
run=$1 sid=$2; shift 2
name=$(basename "$run")
CL=${CLAUDE_BIN:-$HOME/.local/bin/claude}
tmux -f /dev/null -L hspike new-session -d -s "$name" -x 200 -y 50 -c "$run/work" \
  env -i HOME="$HOME" PATH="$PATH" TERM=xterm-256color LANG=C.UTF-8 \
  "$CL" --model sonnet --session-id "$sid" --settings "$run/settings.json" "$@"
echo "$name"
