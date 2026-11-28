#!/usr/bin/env python3
"""Run serial negative controls through Go overlays; never edit live sources."""
import json
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[3]
CASES = [
    ("login_payload_utf8", "internal/workerproto/login.go",
     "if !utf8.Valid(data) {", "if !utf8.Valid(data) && false {",
     "./internal/workerproto", "^TestEphemeralLoginDataUTF8$",
     "UTF-8 login payload validity = true, want false"),
    ("login_capability_minor_negotiation", "internal/workerproto/protocol.go",
     "if cap == LoginCapability && minor < 4 {", "if cap == LoginCapability && minor < 4 && false {",
     "./internal/workerproto", "^TestEphemeralLoginContractNegotiation$/old-peer$",
     "typed login capability survived an older minor"),
    ("login_operation_minor_rejection", "internal/workerproto/bodies.go",
     "{LoginStart, LoginCapability, false, false, 4}", "{LoginStart, LoginCapability, false, false, 0}",
     "./internal/workerproto", "^TestEphemeralLoginContractNegotiation$/old-peer$",
     "want refusal unsupported_operation"),
    ("login_capability_rejection", "internal/workerproto/protocol.go",
     "if !c.caps[op.Capability] {", "if !c.caps[op.Capability] && op.Capability != LoginCapability {",
     "./internal/workerproto", "^TestEphemeralLoginContractNegotiation$/missing-capability$",
     "want refusal unsupported_capability"),
    ("login_durable_key_rejection", "internal/workerproto/protocol.go",
     'if op.Capability == LoginCapability && m.Key != "" {',
     'if op.Capability == LoginCapability && m.Key != "" && false {',
     "./internal/workerproto", "^TestEphemeralLoginContractValidation$",
     "ephemeral login accepted a durable key"),
    ("login_typed_payload_validation", "internal/workerproto/bodies.go",
     "_, err := DecodeLoginData(b.Data)\n\t\t\t\t\treturn err == nil", "return len(b.Data) > 0",
     "./internal/workerproto", "^TestEphemeralLoginContractValidation$",
     "typed login accepted an invalid stream payload"),
    ("login_input_bound", "internal/workerproto/login.go",
     "len(b.Data) <= MaxLoginData", "len(b.Data) <= MaxChunk",
     "./internal/workerproto", "^TestEphemeralLoginContractValidation$",
     "typed login accepted invalid or oversized input"),
    ("login_contract_before_probe", "internal/machineio/login.go",
     " || !p.CanLogin()", "",
     "./internal/machineio", "^TestWorkerLoginContractRequiredBeforeProbe$",
     "unsupported login contract reached worker probe"),
    ("encoded_fixture_marker_scan", "internal/worker/login_stream_test.go",
     "if depth >= 8 {", "if depth >= 0 {",
     "./internal/worker", "^TestWorkerLoginMarkerScanFindsEncodedWirePayloads$",
     "encoded worker login payload escaped marker scan"),
    ("daemon_pinned_transport_wiring", "cmd/hataori/daemon.go",
     "lr.Workers[key] = relay.WorkerLoginEndpoint{Binding: remote.Binding, Open: remote.Open}",
     "_ = key\n\t\t_ = remote",
     "./cmd/hataori", "^TestWorkerLoginDaemonUsesPinnedTransport$",
     "daemon did not give the login relay its pinned worker transport"),
    ("explicit_reply_refusal", "internal/chat/login.go",
     "if answered == 0 {", "if answered == 0 && false {",
     "./internal/chat", "^TestWorkerLoginDirectReplyBoundary$/unknown$",
     "unrelated reply reached the waiting login"),
    ("reply_thread_identity", "internal/chat/login.go",
     "`AND dm_channel = ? AND dm_thread = ? AND dm_ref = ?`, in.ReplyTo.Conv.Channel, in.ReplyTo.Conv.Thread, in.ReplyTo.ID",
     "`AND dm_channel = ? AND dm_ref = ?`, in.ReplyTo.Conv.Channel, in.ReplyTo.ID",
     "./internal/chat", "^TestWorkerLoginDirectReplyBoundary$/wrong-thread$",
     "unrelated reply reached the waiting login"),
    ("followup_thread_identity", "internal/chat/login.go",
     'ref.Conv.Channel + "\\x00" + ref.Conv.Thread + "\\x00" + ref.ID',
     'ref.Conv.Channel + "\\x00" + ref.ID',
     "./internal/chat", "^TestWorkerLoginDirectReplyBoundary$/followup-wrong-thread$",
     "unrelated reply reached the waiting login"),
    ("reply_outage_selection", "internal/chat/login.go",
     "if answered != 0 {", "if answered != 0 && false {",
     "./internal/chat", "^TestWorkerLoginDirectReplyBoundary$/other-outage$",
     "unrelated reply reached the waiting login"),
    ("front_end_owner_input", "internal/chat/login.go",
     "if in.Author.Kind == surface.Bot || !f.owner(in.Author) {",
     "if in.Author.Kind == surface.Bot || false {",
     "./internal/chat", "^TestWorkerLoginDirectReplyBoundary$/guest$",
     "unrelated reply reached the waiting login"),
    ("guard_lifetime_fd_private", "internal/worker/login_guard.go",
     "syscall.CloseOnExec(3)", "",
     "./internal/worker", "^TestWorkerLoginGuardDescriptorsArePrivate$",
     "guard descriptors reached the CLI"),
    ("direct_child_session_change", "internal/worker/login_guard.go",
     "cmd.Process.Kill() // this still-owned direct child may have changed group", "",
     "./internal/worker", "^TestWorkerLoginGuardEndsDirectChildThatChangesSession$",
     "direct login child survived by changing session"),
    ("guard_status_fd_private", "internal/worker/login_guard.go",
     "syscall.CloseOnExec(4)", "",
     "./internal/worker", "^TestWorkerLoginGuardDescriptorsArePrivate$",
     "guard descriptors reached the CLI"),
    ("empty_environment_isolation", "internal/worker/login_command.go",
     "if env == nil {\n\t\tenv = []string{}\n\t}", "",
     "./internal/worker", "^TestWorkerLoginEmptyEnvironmentDoesNotInherit$",
     "empty login environment inherited the parent's fixture marker"),
    ("owner_input", "internal/relay/relay.go",
     "func (r *Relay) Code(ctx context.Context, from, agentName string, outage int64, code string) error {\n\tif !r.isOwner(from) {",
     "func (r *Relay) Code(ctx context.Context, from, agentName string, outage int64, code string) error {\n\tif false {",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "non-owner sent a code to the waiting command"),
    ("installation_pin", "internal/relay/worker_login.go",
     " || pinned != endpoint.Binding.Worker", "",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "unpinned installation reached transport"),
    ("bounded_status_opt_in", "internal/worker/login_attempt.go",
     "!canVerify", "(canVerify && false)",
     "./internal/worker", "^TestWorkerLoginRefusesAdapterWithoutBoundedStatus$",
     "adapter without bounded status was admitted"),
    ("status_deadline", "internal/worker/login_attempt.go",
     "probeCtx, stop := context.WithTimeout(ctx, 2*time.Second)", "probeCtx, stop := context.WithCancel(ctx)",
     "./internal/worker", "^TestWorkerLoginStatusExecutionIsBoundedAndMustExitCleanly$/status-hang$",
     "status command exceeded its enforced deadline"),
    ("stream_attempt_correlation", "internal/machineio/login.go",
     "if stream.Attempt != l.attempt {", "if stream.Attempt != l.attempt && false {",
     "./internal/machineio", "^TestWorkerLoginWireCorrelations$/stream-attempt$",
     "uncorrelated login frame was accepted"),
    ("input_ack_attempt_correlation", "internal/machineio/login.go",
     " || ack.Attempt != l.attempt", "",
     "./internal/machineio", "^TestWorkerLoginWireCorrelations$/input-ack-attempt$",
     "uncorrelated login frame was accepted"),
    ("cancel_ack_request_correlation", "internal/machineio/login.go",
     " || m.ID != l.cancelID", "",
     "./internal/machineio", "^TestWorkerLoginWireCorrelations$/cancel-ack-id$",
     "uncorrelated login frame was accepted"),
    ("complete_cancel_cleanup_barrier", "internal/machineio/login.go",
     "l.ended = true\n\t\t\t\tl.cleanupAck.Do(func() { close(l.cancelAck) })",
     "l.ended = true",
     "./internal/machineio", "^TestWorkerLoginCompleteWakesConcurrentCancel$",
     "completed cleanup did not wake concurrent cancellation"),
    ("owner_death_pipe", "internal/worker/login_guard.go",
     "lifetime.Read(b[:])\n\t\tcmd.Process.Kill() // this still-owned direct child may have changed group\n\t\tend()", "lifetime.Read(b[:])\n\t\treturn",
     "./internal/worker", "^TestWorkerLoginOwnerDeathClosesGuard$",
     "login child survived abrupt owner death"),
    ("selected_codex_directory", "internal/agent/codex/login.go",
     "Env: a.sessionEnv(spec)", "Env: a.env()",
     "./internal/agent/codex", "^TestWorkerLoginUsesSelectedConfiguration$",
     "login used another configuration"),
    ("login_line_bound", "internal/worker/login_attempt.go",
     "if len(w.pending)+take > loginLineLimit {", "if false {",
     "./internal/worker", "^TestWorkerLoginOutputGuard$", "oversized output was accepted"),
    ("status_output_bound", "internal/worker/login_command.go",
     "if len(b) > w.remaining {", "if false {",
     "./internal/worker", "^TestWorkerLoginStatusExecutionIsBoundedAndMustExitCleanly$/status-flood$",
     "failed/unbounded status command certified login"),
    ("status_clean_exit", "internal/worker/login_guard.go",
     "if cmd.Wait() == nil {", "if cmd.Wait() == nil || true {",
     "./internal/worker", "^TestWorkerLoginStatusExecutionIsBoundedAndMustExitCleanly$/status-failed-claims$",
     "failed/unbounded status command certified login"),
    ("descendant_cleanup", "internal/worker/login_guard.go",
     "status.Close()\n\tend()", "status.Close()\n\treturn 0",
     "./internal/worker", "^TestWorkerLoginGuardEndsDescendantAfterCleanLeaderExit$",
     "guard left a descendant alive after its parent exited"),
    ("configuration_metadata_match", "internal/machineio/login.go",
     " || health.ConfigDir != expectedDir", "",
     "./internal/worker", "^TestWorkerLoginMismatchedDirectoryNeverStartsCommand$",
     "wrong-directory outage started a command"),
    ("owner_start", "internal/relay/relay.go",
     "if !r.isOwner(from) {", "if false {",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "non-owner started worker login"),
    ("closed_outage_input", "internal/relay/relay.go",
     "if !l.Open {", "if l.Open && false {",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "closed outage accepted input before event cleanup"),
    ("outage_machine_match", "internal/session/worker_login.go",
     " || l.Machine != expected.Machine", "",
     "./internal/session", "^TestWorkerLoginClosesOnlyOriginalMachineOutage$",
     "closed = true, want false"),
    ("stored_spec_case", "internal/relay/worker_login.go",
     "$.spec.ConfigDir", "$.Spec.ConfigDir",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "relay: the agent cannot be logged in from here"),
    ("default_directory_match", "internal/relay/worker_login.go",
     "coalesce(json_extract(s.spec,'$.spec.ConfigDir'),'')", "json_extract(s.spec,'$.spec.ConfigDir')",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/default$",
     "relay: the agent cannot be logged in from here"),
    ("disconnect_during_blocked_input", "internal/worker/login_stream.go",
     "defer f.cancel()", "defer func() {}()",
     "./internal/worker", "^TestWorkerLoginDisconnectInterruptsBlockedInput$",
     "disconnect did not interrupt blocked CLI input"),
    ("forbidden_persistence_instrumentation", "internal/worker/login_stream.go",
     "json.Unmarshal(m.Body, &input)",
     "json.Unmarshal(m.Body, &input)\n\t\t\tr.root.WriteFile(\"state/worker/login-unexpected.log\", m.Body, 0600)",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "login marker persisted in login-unexpected.log"),
    ("forbidden_process_arguments_instrumentation", "internal/worker/login_command.go",
     "argv := append(append([]string{}, guard...), launch.Argv...)",
     "launch.Argv = append(launch.Argv, \"marker-secret\")\n\targv := append(append([]string{}, guard...), launch.Argv...)",
     "./internal/worker", "^TestWorkerLoginFakeSurfacePersistence$",
     "login marker persisted in argv-capture.jsonl"),
    ("forbidden_device_frame_instrumentation", "internal/worker/login_stream.go",
     'm.Body, _ = json.Marshal(workerproto.LoginStream{Attempt: attempt.Attempt, State: "data", Data: payload})',
     'm.Body, _ = json.Marshal(workerproto.LoginStream{Attempt: attempt.Attempt, State: "data", Data: payload})\n\t\t\tif d.ShowCode != "" {\n\t\t\t\tsaved, _ := workerproto.Encode(m)\n\t\t\t\tr.root.WriteFile("state/worker/login-device-frame.log", saved, 0600)\n\t\t\t}',
     "./internal/worker", "^TestWorkerLoginFakeSurfacePersistence$/device$",
     "login marker persisted in login-device-frame.log"),
]


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: login_negative.py OUTPUT_JSON")
    env = dict(os.environ, GOMAXPROCS="1")
    results = []
    with tempfile.TemporaryDirectory(prefix="worker-login-negative-") as temp:
        directory = Path(temp)
        for name, relative, before, after, package, pattern, assertion in CASES:
            source = ROOT / relative
            text = source.read_text()
            if before not in text:
                raise SystemExit(f"missing mutation anchor: {name}")
            modified = directory / (name + ".go")
            # owner_start intentionally mutates only Start, preserving Code.
            modified.write_text(text.replace(before, after, 1))
            overlay = directory / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {str(source): str(modified)}}))
            command = ["go", "test", "-p=1", "-parallel=1", "-count=1", "-timeout=25s",
                       "-overlay=" + str(overlay), package, "-run", pattern]
            started = time.monotonic()
            run = subprocess.run(command, cwd=ROOT, env=env, stdout=subprocess.PIPE,
                                 stderr=subprocess.STDOUT, text=True, timeout=35)
            output = run.stdout.replace(str(ROOT), "<repo>").replace(temp, "<overlay>")
            accepted = run.returncode != 0 and assertion in output and "[build failed]" not in output
            results.append({"guard": name, "source": relative, "package": package,
                            "kind": "persistence instrumentation" if name.endswith("_instrumentation") else "guard removal/reversion",
                            "test": pattern, "exit_code": run.returncode,
                            "seconds": round(time.monotonic() - started, 3),
                            "accepted_negative": accepted, "output": output})
            print(name + ": " + ("observed regression" if accepted else "NOT ESTABLISHED"), flush=True)
    sources = {case[1] for case in CASES}
    sources.update(str(path.relative_to(ROOT)) for path in (ROOT / "internal/worker").glob("login*.go"))
    for pattern in ("internal/agent/codex/worker_login*.go", "internal/session/worker_login*.go",
                    "internal/workerproto/login*.go", "internal/relay/worker_login.go",
                    "internal/workerproto/protocol*.go", "internal/workerproto/bodies.go",
                    "internal/workerproto/golden_test.go", "internal/workerproto/testdata/protocol/SUMS",
                    "internal/workerproto/testdata/protocol/1.4/*.json",
                    "internal/machineio/login*.go", "internal/agent/worker_login.go",
                    "internal/worker/server.go", "internal/worker/resident.go", "cmd/hataori/worker.go",
                    "cmd/hataori/worker_login_test.go", "internal/chat/worker_login_test.go",
                    "internal/worker/testdata/login_negative.py"):
        sources.update(str(path.relative_to(ROOT)) for path in ROOT.glob(pattern))
    hashes = {path: hashlib.sha256((ROOT / path).read_bytes()).hexdigest() for path in sorted(sources)}
    Path(sys.argv[1]).write_text(json.dumps({"method": "Go overlays; serial GOMAXPROCS=1/-p=1/-parallel=1; sources unchanged",
                                          "source_sha256": hashes, "results": results}, indent=2) + "\n")
    return 0 if all(r["accepted_negative"] for r in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
