#!/usr/bin/env python3
"""Serial disposable regression mutations; always restore exact source bytes.

Run from the repository root. This touches only workerinstall implementation,
invokes only named focused tests, and never uses a service manager or agent CLI.
"""

import hashlib
import json
import os
import re
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[3]
PACKAGE = ROOT / "internal/workerinstall"

CASES = [
    ("manifest-provenance", "manifest.go",
     " || !ed25519.Verify(trusted, raw, signature)", "",
     "TestVerifyRefusesBeforeInstallation"),
    ("protocol-major", "manifest.go",
     'if strings.Split(m.Protocol, ".")[0] != strings.Split(protocol, ".")[0] {',
     'if false && strings.Split(m.Protocol, ".")[0] != strings.Split(protocol, ".")[0] {',
     "TestVerifyRefusesBeforeInstallation/major"),
    ("download-checksum", "store.go",
     " || hex.EncodeToString(h.Sum(nil)) != a.SHA256", "",
     "TestInstallFailuresPreserveSelectionAndVersions/checksum"),
    ("complete-download-size", "store.go",
     " || n != a.Size", " || n < 0",
     "TestInstallFailuresPreserveSelectionAndVersions/size_mismatch"),
    ("download-read-error", "store.go",
     "if err != nil || n != a.Size || hex.EncodeToString(h.Sum(nil)) != a.SHA256 {",
     "if n != a.Size || hex.EncodeToString(h.Sum(nil)) != a.SHA256 {",
     "TestInstallFailuresPreserveSelectionAndVersions/reader_failure"),
    ("executable-platform", "binary.go",
     "func checkBinary(f *os.File, p Platform) error {",
     "func checkBinary(f *os.File, p Platform) error {\n if true {return nil}",
     "TestCompletePlatformBinaries"),
    ("exclusive-version-publication", "store.go",
     "s.root.Link(temp, name)", "s.root.Rename(temp, name)",
     "TestInstallRetainsImmutableVersions"),
    ("refuse-unregistered-work", "store.go",
     "if len(entries) != 0 || e != io.EOF {",
     "if false && (len(entries) != 0 || e != io.EOF) {",
     "TestInstallRefusesAdoptionAndLinks/unregistered"),
    ("pinned-trust-key", "store.go",
     " || key != nil && !bytes.Equal(marker, key)", "",
     "TestInstallFailuresPreserveSelectionAndVersions/trust_key"),
    ("exclusive-installer", "store.go",
     "unix.LOCK_EX|unix.LOCK_NB", "unix.LOCK_UN",
     "TestInstallFailuresPreserveSelectionAndVersions/busy"),
    ("reverify-selected-executable", "store.go",
     "if err = s.verifyBinary(name, a); err != nil {",
     "if err = s.verifyBinary(name, a); false && err != nil {",
     "TestSelectedRefusesChangedBinary"),
    ("trusted-physical-ancestors", "store.go",
     "if err = secureAncestors(physicalParent); err != nil {",
     "if err = secureAncestors(physicalParent); false && err != nil {",
     "TestInstallRefusesMutableAncestors"),
    ("stable-physical-prefix", "store.go",
     "path = filepath.Join(physicalParent, filepath.Base(path))",
     "path = filepath.Join(filepath.Dir(path), filepath.Base(path))",
     "TestInstallResolvesAncestorAliasesOnce"),
    ("go-build-platform", "binary.go",
     "if goos != p.OS || goarch != p.Arch {",
     "if false && (goos != p.OS || goarch != p.Arch) {",
     "TestSignedForeignOperatingSystemKeepsSelection"),
    ("elf-os-abi", "binary.go",
     "(e.OSABI == elf.ELFOSABI_NONE || e.OSABI == elf.ELFOSABI_LINUX) && ", "",
     "TestSignedForeignOperatingSystemKeepsSelection"),
    ("complete-elf-segments", "binary.go",
     "if !within(segment.Off, segment.Filesz) || segment.Type == elf.PT_LOAD && segment.Filesz > segment.Memsz {",
     "if false && (!within(segment.Off, segment.Filesz) || segment.Type == elf.PT_LOAD && segment.Filesz > segment.Memsz) {",
     "TestCompletePlatformBinaries"),
    ("complete-macho-segments", "binary.go",
     "ok && (!within(segment.Offset, segment.Filesz) || segment.Maxprot != 0 && segment.Filesz > segment.Memsz)",
     "ok && false && (!within(segment.Offset, segment.Filesz) || segment.Maxprot != 0 && segment.Filesz > segment.Memsz)",
     "TestCompletePlatformBinaries"),
    ("extended-permission-mutation-rights", "permissions.go",
     "if kind&0xf == 1 && rights & ^readOnlyRights != 0 {",
     "if false && kind&0xf == 1 && rights & ^readOnlyRights != 0 {",
     "TestDarwinACLBufferBoundsAndMutationRights"),
    ("systemd-detached-host-preservation", "service.go",
     "KillMode=process", "KillMode=control-group",
     "TestExplicitServiceTemplatesKeepHosts"),
    ("launchagent-detached-host-preservation", "service.go",
     "<key>AbandonProcessGroup</key><true/>",
     "<key>AbandonProcessGroup</key><false/>",
     "TestExplicitServiceTemplatesKeepHosts"),
]


def main():
    env = dict(os.environ, GOMAXPROCS="1")
    originals = {name: (PACKAGE / name).read_bytes() for _, name, *_ in CASES}
    results = []
    try:
        for name, file, before, after, test in CASES:
            path = PACKAGE / file
            original = originals[file]
            text = original.decode()
            # The download/installed-byte checks share a spelling. Mutate only
            # the first (download) guard; all other anchors must be unique.
            count = text.count(before)
            expected = 2 if name in {"download-checksum", "complete-download-size", "download-read-error"} else 1
            if count != expected:
                raise RuntimeError(f"{name}: expected {expected} anchors, got {count}")
            path.write_text(text.replace(before, after, 1))
            command = ["go", "test", "-p=1", "-parallel=1", "-count=1",
                       "-timeout=90s", "-run", "^" + test + "$", "./internal/workerinstall"]
            try:
                run = subprocess.run(command, cwd=ROOT, env=env,
                                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                     text=True, timeout=120)
                # A compiler failure or timeout is not regression proof.
                failed = run.returncode != 0 and "--- FAIL:" in run.stdout and "build failed" not in run.stdout and "test timed out" not in run.stdout
                results.append({"guard": name, "test": test, "exit": run.returncode,
                                "regression_failed": failed,
                                "failure_lines": [re.sub(r"/[^\s{}]*Test[^\s{}]*", "<fixture-path>", line.strip()) for line in run.stdout.splitlines()
                                                  if "--- FAIL:" in line or ".go:" in line]})
                print(f"{name}: {'regression failed' if failed else 'INVALID PROOF'}", flush=True)
                if not failed:
                    print(run.stdout, file=sys.stderr)
            finally:
                path.write_bytes(original)
    finally:
        for name, original in originals.items():
            if (PACKAGE / name).read_bytes() != original:
                (PACKAGE / name).write_bytes(original)
    evidence = {"scope": "isolated focused tests; no runtime/service-manager acceptance",
                "redaction": "Disposable absolute test paths, if printed, are replaced with <fixture-path>.",
                "source_sha256": {name: hashlib.sha256(b).hexdigest() for name, b in originals.items()},
                "results": results,
                "native_acl_guard_proof": "TestDarwinACLGuardRemoval runs only in macOS CI; not executed locally. A passing native test proves the unsafe-ACL regression fails when its guard is bypassed in a child process.",
                "sources_restored": all((PACKAGE / name).read_bytes() == b for name, b in originals.items())}
    output = PACKAGE / "testdata/negative-guards.json"
    output.write_text(json.dumps(evidence, indent=2) + "\n")
    return 0 if len(results) == len(CASES) and all(r["regression_failed"] for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
