# Run serially against isolated fixtures; always restore each candidate edit.
import json, os, subprocess, sys
from pathlib import Path
cases = [
 ("waiting launch operation", "internal/core/places.go", "l := Launch{AcceptedOperation: in.AcceptedOperation, Session: w.session", "l := Launch{Session: w.session", "./internal/core", "^TestWhatWaitedIsLaunchedAsWithoutAWait$"),
 ("startup retry operation", "internal/core/trust.go", "first := FirstInput{AcceptedOperation: old.AcceptedOperation, Principal: old.Principal", "first := FirstInput{Principal: old.Principal", "./internal/core", "^TestConversationRetryWaitsForAPlace$"),
 ("ready gate", "internal/placement/placement.go", "case !o.Ready:", "case false:", "./internal/placement", "^TestEveryEligibilityGate/away$"),
 ("reservation before workspace", "internal/core/placement.go", "func reservePlacement(ctx context.Context, tx *store.Tx, thread int64, machine, agent string, now time.Time) error {", "func reservePlacement(ctx context.Context, tx *store.Tx, thread int64, machine, agent string, now time.Time) error { return nil", "./internal/core", "^TestPlacementConcurrentExplicitStartsReserveBeforeWorkspace$"),
 ("original accepted operation", "internal/core/core.go", "f.AcceptedOperation, f.NoInput = l.AcceptedOperation, l.NoInput", "f.NoInput = l.NoInput", "./internal/core", "^TestPlacementWorkspaceRecordPreservesAcceptedOperation$"),
 ("resident admission", "internal/worker/admission.go", "if a == nil {", "if true {", "./internal/worker", "^TestPlacementResidentRechecksEveryLimit$"),
 ("earlier local holder proof", "internal/runner/machines.go", "func (r *Runner) endPriorPlacements(ctx context.Context, id int64) error {", "func (r *Runner) endPriorPlacements(ctx context.Context, id int64) error { return nil", "./internal/runner", "^TestPlacementLocalPriorUnknownBlocksReplacementAndRelease$"),
 ("original phase2 host recovery", "internal/runner/machines.go", "return l.State == holder.Running || l.Ended(), nil", "_ = l; return false, nil", "./internal/runner", "^TestPlacementLocalRecoveryUsesOriginalHostBeforeNewAdmission$"),
 ("terminal trash history", "internal/worker/workspace_ops.go", 'tr.State != "restored" && tr.State != "purged" && (!ok || w.Target != tr.Target || w.Device != tr.Device || w.Inode != tr.Inode)', '(!ok || w.Target != tr.Target || w.Device != tr.Device || w.Inode != tr.Inode)', "./internal/worker", "^TestPlacementRestoredWorkspaceTransfersAndRestarts$"),
 ("canceled dispatch survives restarts", "internal/core/placement.go", "const PlacementLaunchCanceled = 3", "const PlacementLaunchCanceled = 0", "./internal/runner", "^TestPlacementRevocationStaysCanceledAcrossTwoRestarts$"),
 ("durable placement mode", "internal/store/placement_migration.go", "return enabled, err", "return configured, err", "./cmd/hataori", "^TestPlacementModeSurvivesRemovingLastLimitAndRevocation$"),
 ("reconnect launch authority", "internal/runner/runner.go", "if r.RevalidateLaunch != nil {", "if false && r.RevalidateLaunch != nil {", "./internal/runner", "^TestPlacementRevocationStaysCanceledAcrossTwoRestarts/phase-0$"),
]
if len(sys.argv)>1:
 cases=[c for c in cases if c[0] in sys.argv[1:]]
 if not cases: raise RuntimeError("unknown guard selection")
 previous=json.loads(Path("docs/notes/machine-placement/negative-guards.json").read_text())
 results=[r for r in previous if r["guard"] not in {c[0] for c in cases}]
else: results=[]
for name, filename, old, new, package, pattern in cases:
 path=Path(filename); original=path.read_text()
 if original.count(old)!=1: raise RuntimeError("mutation anchor: "+name)
 try:
  path.write_text(original.replace(old,new,1))
  command=["go","test","-p","1",package,"-run",pattern,"-parallel","1","-count=1"]
  run=subprocess.run(command,env={**os.environ,"GOMAXPROCS":"1"},capture_output=True,text=True,timeout=55)
  output=run.stdout+run.stderr
  proved=run.returncode!=0 and "--- FAIL: Test" in output and "build failed" not in output
  results.append({"guard":name,"command":" ".join(command),"expected_failure":proved,"exit_code":run.returncode,"evidence":[line.strip() for line in output.splitlines() if "FAIL" in line or "_test.go:" in line][:6]})
  print(name+": "+("expected test failure" if proved else "NOT PROVED"),flush=True)
  if not proved: raise RuntimeError(output[-2500:])
 finally: path.write_text(original)
Path("docs/notes/machine-placement/negative-guards.json").write_text(json.dumps(results,indent=2)+"\n")
