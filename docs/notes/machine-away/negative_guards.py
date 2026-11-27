"""Run serial isolated regression mutations; restore every edit even on failure."""
import json, os, subprocess, sys
from pathlib import Path
cases = [
 ("catch-up before ready", "internal/store/away.go", 'if n != 0 {', 'if false && n != 0 {', './internal/store', '^TestAwayGraceCatchupAndPersistentActiveAge$'),
 ("held original target", "internal/core/core.go", 'if guard != nil && guard.held {', 'if false && guard != nil && guard.held {', './internal/core', '^TestAwayHeldSendBindsOriginalTargetInsideAcceptance/new_run$'),
 ("definitely unsent marker rollback", "internal/runner/machines.go", 'failed(true) // readiness refused before any effect was journaled or sent', '// rollback removed', './internal/runner', '^TestAwayReadinessCutAtWriteAndStopBoundary/input$'),
 ("stop readiness refusal", "internal/runner/runner.go", ' || errors.Is(err, core.ErrMachineUnchecked) {', ' {', './internal/runner', '^TestAwayReadinessCutAtWriteAndStopBoundary/stop$'),
 ("unknown inventory is not reboot", "internal/runner/away.go", 'return err == nil && state == "gone"', 'return err == nil && state != "gone"', './internal/runner', '^TestAwayUnknownInventoryAndConfirmedGoneDuringWarmReplay$'),
 ("confirmed gone despite lost retained output", "internal/runner/runner.go", 'if r.confirmedGone(lv) {', 'if false && r.confirmedGone(lv) {', './internal/runner', '^TestAwayUnknownInventoryAndConfirmedGoneDuringWarmReplay$'),
 ("active deadline budget", "internal/store/away.go", 'now.Sub(since)-time.Duration(paused)*time.Millisecond', 'now.Sub(since)', './internal/core', '^TestAwayClosedCleanupPreservesRemainingActiveAge$'),
 ("delivery budget survives reconstruction", "internal/runner/away.go", 'func (r *Runner) writtenClock(lv *live, at int64) time.Time {', 'func (r *Runner) writtenClock(lv *live, at int64) time.Time { return time.Time{}', './internal/runner', '^TestAwayWrittenDeliveryBudgetSurvivesCodecRebuild$'),
 ("destructive unchecked refusal", "internal/core/away.go", 'if a.State != "ready" {', 'if false && a.State != "ready" {', './internal/core', '^TestAwayHeldInputStopRestartOrderAndUncheckedRefusals$'),
 ("initial authority after catch-up", "internal/core/away.go", 'func (c *Core) RevalidateInitial(ctx context.Context, tx *store.Tx, sid int64) error {', 'func (c *Core) RevalidateInitial(ctx context.Context, tx *store.Tx, sid int64) error { return nil', './internal/core', '^TestAwayRevokedInitialInputKeepsHostMonitoringAndAudit$'),
 ("pending authority before write", "internal/runner/runner.go", 'if r.RevalidatePending != nil {', 'if false && r.RevalidatePending != nil {', './internal/runner', '^TestAwayRevokedPendingInputPreservesEventSequence$'),
 ("API acceptance and held receipt atomicity", "internal/core/core.go", 'if accepted {\n\t\t\t\treturn a.acceptHeld(ctx, tx, pending)', 'if false && accepted {\n\t\t\t\treturn a.acceptHeld(ctx, tx, pending)', './internal/core', '^TestAwayAcceptedQueueReceiptCommitsBeforeRunnerWrite$'),
 ("expired worker hook withdrawal", "internal/runner/away.go", 'if reconciler, ok := lv.codec.(agent.HookReconciler); ok {', 'if reconciler, ok := lv.codec.(agent.HookReconciler); ok && false {', './internal/runner', '^TestAwayExpiredWorkerHookIsWithdrawnBeforeHeldAnswer$'),
 ("initial acknowledgement retired during catch-up", "internal/runner/runner.go", 'if lv.catchingUp && !(f.Sent && (reported(b.events, firstInputID) || notDelivered(b.events, firstInputID))) {', 'if lv.catchingUp {', './internal/runner', '^TestAwayNativeWrittenReportCorrelatesBeforeCatchup/first$'),
 ("written provenance before buffered reports", "internal/runner/runner.go", 'lv.warmPending = false\n\t\tlv.resend = true\n\t\tr.handOverWritten(lv)', 'lv.warmPending = false\n\t\tlv.resend = true', './internal/runner', '^TestAwayNativeWrittenReportCorrelatesBeforeCatchup$'),
 ("worker hook real clock", "internal/runner/away.go", 'if c, ok := lv.codec.(agent.ClockedTicker); ok {', 'if c, ok := lv.codec.(agent.ClockedTicker); ok && false {', './internal/runner', '^TestAwayNewNativeHookUsesWallClockAfterPause$'),
 ("accepted answer visible refusal", "internal/runner/runner.go", 'if err != nil && answer {', 'if false && err != nil && answer {', './internal/runner', '^TestAwayAcceptedAnswerRefusalIsDurable/codec_expired$'),

 ("missing receipt preserves uncertainty", "internal/runner/machines.go", '// Missing receipt is not proof the write did not occur.\n\t\t\t\treturn machineio.ErrUnknown', 'if err:=r.recordActionResolution(lv,a.id,a.inputs,"refused");err!=nil{return err}; continue', './internal/runner', '^TestAwayMissingReceiptAfterAppliedWriteNeverAllowsRetry$'),

 ("receipt and input marker atomic commit", "internal/runner/away.go", 'var unsent []string\n\tctx := context.Background()\n\terr := r.Store.Update', 'var unsent []string\n\tctx := context.Background()\n if action!=0 {if _,err:=r.Store.DB.ExecContext(ctx,`UPDATE session_machine_actions SET state=? WHERE id=?`,state,action);err!=nil{return err}}\n\terr := r.Store.Update', './internal/runner', '^TestAwayRefusedReceiptAndWrittenMarkerCommitAtomically$'),

 ("immediate refusal atomicity", "internal/runner/machines.go", 'if state == "refused" && len(inputs) > 0 {', 'if false && state == "refused" && len(inputs) > 0 {', './internal/runner', '^TestAwayImmediateNoWriteRefusalIsAtomic$'),
 ("accepted answer caller-turn binding", "internal/core/core.go", 'if req != "" {\n\t\tif err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM turns', 'if false && req != "" {\n\t\tif err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM turns', './internal/core', '^TestAwayAnswerAcceptanceBindsItsNewCallerTurn$'),
 ("rejected answer completes bound caller turn", "internal/runner/away.go", 'if turn != 0 {', 'if false && turn != 0 {', './internal/runner', '^TestAwayRejectedAnswerEndsOnlyItsBoundCallerTurn$'),

 ("immediate answer follower sequence", "internal/runner/machines.go", 'r.machineEffectFor(ctx, lv, remote, m, inputs...); err != nil {\n\t\t\treturn err', 'r.machineEffect(ctx, lv.id, remote, m, inputs...); err != nil {\n\t\t\treturn err', './internal/runner', '^TestAwayImmediateAnswerRefusalPreservesRealFollowerSequence$'),

 ("idle remaining active time", "internal/runner/away.go", 'func (r *Runner) activeAge(ctx context.Context, q store.RowQuerier, id int64, since time.Time) time.Duration {', 'func (r *Runner) activeAge(ctx context.Context, q store.RowQuerier, id int64, since time.Time) time.Duration { return time.Since(since)', './internal/runner', '^TestAwayIdleBudgetDoesNotExpireDuringDisconnection$'),

 ("answer receipt nonreusable identity", "internal/runner/away.go", 'if matches == 0 {', 'if false && matches == 0 {', './internal/runner', '^TestAwayAnswerReceiptCannotRetireReusedPendingRow$'),
 ("transient no-write answer retained", "internal/runner/away.go", 'else if len(retryUnavailable) > 0 && retryUnavailable[0] {', 'else if false && len(retryUnavailable) > 0 && retryUnavailable[0] {', './internal/runner', '^TestRemoteRunnerRefusedHookGetsNewAttemptAfterRestart$'),
 ("resolved refusal consumed", "internal/runner/machines.go", "state='complete' AND EXISTS", "state IN ('complete','refused') AND EXISTS", './internal/runner', '^TestRemoteRunnerRefusedHookGetsNewAttemptAfterRestart$'),

]
# The gone check appears twice; remove both to cover both warm and new reads.
selected=set(sys.argv[1:])
if selected: cases=[c for c in cases if c[0] in selected]
if not cases: raise RuntimeError('no selected guards')
output_path=Path('docs/notes/machine-away/negative-guards.json')
results=[]
if selected and output_path.exists():
 results=[r for r in json.loads(output_path.read_text()) if r['guard'] not in selected]
for name,filename,old,new,package,pattern in cases:
 path=Path(filename); original=path.read_text()
 expected=2 if name=='confirmed gone despite lost retained output' else 1
 if original.count(old)!=expected: raise RuntimeError('mutation anchor: '+name+': '+str(original.count(old)))
 try:
  path.write_text(original.replace(old,new,expected))
  command=['go','test','-p','1',package,'-run',pattern,'-parallel','1','-count=1']
  run=subprocess.run(command,env={**os.environ,'GOMAXPROCS':'1'},capture_output=True,text=True,timeout=55)
  output=run.stdout+run.stderr
  proved=run.returncode!=0 and '--- FAIL: Test' in output and 'build failed' not in output
  results.append({'guard':name,'command':' '.join(command),'expected_failure':proved,'exit_code':run.returncode,'evidence':[line.strip() for line in output.splitlines() if 'FAIL' in line or '_test.go:' in line][:8]})
  print(name+': '+('expected test failure' if proved else 'NOT PROVED'),flush=True)
  if not proved: raise RuntimeError(output[-2000:])
 finally: path.write_text(original)
output_path.write_text(json.dumps(results,indent=2)+'\n')
