#!/usr/bin/env bash
set -euo pipefail
[[ "${GITHUB_ACTIONS:-}" = true && "${RUNNER_ENVIRONMENT:-}" = github-hosted && "${RUNNER_OS:-}" = macOS ]]
[[ "$DESKTOP_HEAD" =~ ^[0-9a-f]{40}$ ]]
test "$(git rev-parse HEAD)" = "$DESKTOP_HEAD"
printf 'Desktop exact SHA: %s\nRunner: %s / %s\n' "$DESKTOP_HEAD" "$RUNNER_OS" "$(uname -m)" >> "$GITHUB_STEP_SUMMARY"
export DESKTOP_PROOF_VERSION="0.0.0-dev.$DESKTOP_HEAD"
export DESKTOP_PROOF_NEXT_VERSION="0.0.1-dev.$DESKTOP_HEAD"
export DESKTOP_FIXTURE_BINARY="$RUNNER_TEMP/desktop-fixture.test"
export DESKTOP_TRANSPORT_BINARY="$RUNNER_TEMP/hataori-desktop-transport"
export DESKTOP_NEXT_FIXTURE_BINARY="$RUNNER_TEMP/desktop-next-fixture.test"
export DESKTOP_NEXT_TRANSPORT_BINARY="$RUNNER_TEMP/hataori-desktop-next-transport"
export CGO_ENABLED=1
# Ordinary user only. ACL inspection must be available; a skipped required
# Darwin test is a failure, not runtime acceptance. No sudo or local services.
go test -c -o "$RUNNER_TEMP/nativehttp.test" ./internal/nativehttp
"$RUNNER_TEMP/nativehttp.test" -test.run='^(TestDarwinACLGrantRefusesPrivatePaths|TestKernelPeerUID)$' -test.v -test.parallel=1 -test.timeout=30s | tee "$RUNNER_TEMP/desktop-native.log"
! grep -Eq '^[[:space:]]*--- SKIP:' "$RUNNER_TEMP/desktop-native.log"
for name in TestDarwinACLGrantRefusesPrivatePaths TestKernelPeerUID; do
  grep -q "^--- PASS: $name " "$RUNNER_TEMP/desktop-native.log"
done
go test -v -p 1 -parallel 1 -count=1 -timeout 90s ./internal/desktopstate ./internal/desktopsetup ./internal/desktopbridge | tee "$RUNNER_TEMP/desktop-state.log"
grep -q '^--- PASS: TestDarwinCaseInsensitiveScratch ' "$RUNNER_TEMP/desktop-state.log"
printf 'PASS %s %s TestDarwinCaseInsensitiveScratch\n' "$DESKTOP_HEAD" "$RUNNER_OS" >> "$GITHUB_STEP_SUMMARY"
go test -ldflags "-X github.com/yousan/hataori/internal/version.Version=$DESKTOP_PROOF_VERSION" -c -o "$DESKTOP_FIXTURE_BINARY" ./cmd/hataori
go build -trimpath -ldflags "-X github.com/yousan/hataori/internal/version.Version=$DESKTOP_PROOF_VERSION" -o "$DESKTOP_TRANSPORT_BINARY" ./cmd/hataori-desktop-transport
go test -ldflags "-X github.com/yousan/hataori/internal/version.Version=$DESKTOP_PROOF_NEXT_VERSION" -c -o "$DESKTOP_NEXT_FIXTURE_BINARY" ./cmd/hataori
go build -trimpath -ldflags "-X github.com/yousan/hataori/internal/version.Version=$DESKTOP_PROOF_NEXT_VERSION" -o "$DESKTOP_NEXT_TRANSPORT_BINARY" ./cmd/hataori-desktop-transport
npm ci --ignore-scripts --prefix desktop
node desktop/node_modules/electron/install.js
npm test --prefix desktop
node desktop/node_modules/electron/cli.js desktop/test/runtime-main.mjs | tee "$RUNNER_TEMP/desktop-runtime.log"
for name in existing-page-csp-and-sandbox remote-navigation-and-unexpected-capability-refused fetch-event-and-terminal-streams private-duplex-first-message-and-close daemon-only-held-fake-turn-host-and-offset-reconnect cross-version-staged-production-launcher-upgrade persistent-stop-survives-launcher-restart-until-explicit-start fixture-graceful-cleanup-no-pid-lookup; do
  grep -q "^PASS desktop hosted $name$" "$RUNNER_TEMP/desktop-runtime.log"
  printf 'PASS %s %s %s\n' "$DESKTOP_HEAD" "$RUNNER_OS" "$name" >> "$GITHUB_STEP_SUMMARY"
done
npm run make --prefix desktop
# No artifact upload: unsigned outputs stay on this disposable runner unless
# the separate trusted-tag job delivers them to a checked unpublished draft.
