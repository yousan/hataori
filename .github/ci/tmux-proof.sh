#!/usr/bin/env bash
set -euo pipefail
# Build the tmux version under test
set -eu
if ! pkg-config --exists libevent ncurses || ! command -v bison >/dev/null; then
  sudo apt-get -o Dir::Etc::sourcelist="sources.list.d/ubuntu.sources" -o Dir::Etc::sourceparts="-" update
  sudo apt-get install -y libevent-dev libncurses-dev bison
fi
curl -fsSL "https://github.com/tmux/tmux/releases/download/$TMUX_VERSION/tmux-$TMUX_VERSION.tar.gz" -o "$RUNNER_TEMP/tmux.tar.gz"
expected="$(awk -v archive="tmux-$TMUX_VERSION.tar.gz" '$2 == archive {print $1}' .github/ci/tmux-sha256.txt)"
test "${#expected}" = 64
printf '%s  %s\n' "$expected" "$RUNNER_TEMP/tmux.tar.gz" | sha256sum --check --status
tar -xzf "$RUNNER_TEMP/tmux.tar.gz" -C "$RUNNER_TEMP"
cd "$RUNNER_TEMP/tmux-$TMUX_VERSION"
./configure --prefix="$RUNNER_TEMP/tmux-install"
make -j2
make install
export PATH="$RUNNER_TEMP/tmux-install/bin:$PATH"

cd "$GITHUB_WORKSPACE"

# Test the holder
set -eu
git rev-parse HEAD
tmux -V
go test -timeout 150m -count="$TEST_COUNT" -run "$TEST_NAMES" ./internal/holder/tmux

# Test closed-terminal ownership through the runner
if [ "$LIMITED" = false ]; then
go test -timeout 5m -count="$TEST_COUNT" -run '^TestClosedTerminal' ./internal/runner
fi

# Test host terminal inputs
if [ "$LIMITED" = false ]; then
set -eu
skip='^$'
# HostKey documents the complete named-key set of tmux 3.4.
# Older releases drop C-Tab or type C-M-Tab literally. Keep all
# other input tests, including real text/paste and fallback.
case "$TMUX_VERSION" in
  3.2|3.2a) skip='^TestEveryValidKeyIsEncodedByTmux$' ;;
esac
go test -timeout 150m -count="$TEST_COUNT" -run "$TEST_NAMES" -skip "$skip" ./internal/hostterm
fi

# Check the split test rejects inherited remain-on-exit
if [ "$TMUX_VERSION" = 3.8 ]; then
set -eu
source=internal/holder/tmux/started.go
cp "$source" "$RUNNER_TEMP/started-before-mutant.go"
trap 'cp "$RUNNER_TEMP/started-before-mutant.go" "$source"' EXIT
python3 - <<'PYTHON'
from pathlib import Path
p = Path("internal/holder/tmux/started.go")
s = p.read_text()
before = '"set-option -w -u -t " + r.Pane + " remain-on-exit"'
after = '"set-option -w -t " + r.Pane + " remain-on-exit on"'
assert s.count(before) == 1
p.write_text(s.replace(before, after))
PYTHON
if go test -timeout 2m -run '^TestStartInASessionWithAPersonsWindow$' ./internal/holder/tmux > "$RUNNER_TEMP/split-mutant.log" 2>&1; then
  cat "$RUNNER_TEMP/split-mutant.log"
  echo "Split test accepted inherited remain-on-exit"
  exit 1
fi
cat "$RUNNER_TEMP/split-mutant.log"
grep -q 'panes after the second stop' "$RUNNER_TEMP/split-mutant.log"
cp "$RUNNER_TEMP/started-before-mutant.go" "$source"
trap - EXIT
fi

# Repeat the split test with two concurrent copies
if [ "$LIMITED" = false ]; then
set -eu
pids=""
for copy in 1 2 3; do
  go test -timeout 10m -count=30 -run '^TestStartInASessionWithAPersonsWindow$' ./internal/holder/tmux > "$RUNNER_TEMP/split-$copy.log" 2>&1 &
  pids="$pids $!"
done
failed=0
for pid in $pids; do
  wait "$pid" || failed=1
done
for copy in 1 2 3; do
  cat "$RUNNER_TEMP/split-$copy.log"
done
exit "$failed"
fi
