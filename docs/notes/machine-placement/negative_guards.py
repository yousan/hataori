# Run serially against isolated fixtures; always restore each candidate edit.
import json, os, subprocess
from pathlib import Path
cases = [
 ("ready gate", "internal/placement/placement.go", "case !o.Ready:", "case false:", "./internal/placement", "^TestEveryEligibilityGate/away$"),
 ("reservation before workspace", "internal/core/placement.go", "func reservePlacement(ctx context.Context, tx *store.Tx, thread int64, machine, agent string, now time.Time) error {", "func reservePlacement(ctx context.Context, tx *store.Tx, thread int64, machine, agent string, now time.Time) error { return nil", "./internal/core", "^TestPlacementConcurrentExplicitStartsReserveBeforeWorkspace$"),
 ("original accepted operation", "internal/core/core.go", "f.AcceptedOperation, f.NoInput = l.AcceptedOperation, l.NoInput", "f.NoInput = l.NoInput", "./internal/core", "^TestPlacementWorkspaceRecordPreservesAcceptedOperation$"),
 ("resident admission", "internal/worker/admission.go", "if a == nil {", "if true {", "./internal/worker", "^TestPlacementResidentRechecksEveryLimit$"),
 ("earlier local holder proof", "internal/runner/machines.go", "func (r *Runner) endPriorPlacements(ctx context.Context, id int64) error {", "func (r *Runner) endPriorPlacements(ctx context.Context, id int64) error { return nil", "./internal/runner", "^TestPlacementLocalPriorUnknownBlocksReplacementAndRelease$"),
 ("original phase2 host recovery", "internal/runner/machines.go", "return l.State == holder.Running || l.Ended(), nil", "_ = l; return false, nil", "./internal/runner", "^TestPlacementLocalRecoveryUsesOriginalHostBeforeNewAdmission$"),
 ("terminal trash history", "internal/worker/workspace_ops.go", 'tr.State != "restored" && tr.State != "purged" && (!ok || w.Target != tr.Target || w.Device != tr.Device || w.Inode != tr.Inode)', '(!ok || w.Target != tr.Target || w.Device != tr.Device || w.Inode != tr.Inode)', "./internal/worker", "^TestPlacementRestoredWorkspaceTransfersAndRestarts$"),
]
results=[]
for name, filename, old, new, package, pattern in cases:
 path=Path(filename); original=path.read_text()
 if original.count(old)!=1: raise RuntimeError("mutation anchor: "+name)
 try:
  path.write_text(original.replace(old,new,1))
  command=["go","test","-p","1",package,"-run",pattern,"-parallel","1","-count=1"]
  run=subprocess.run(command,env={**os.environ,"GOMAXPROCS":"1"},capture_output=True,text=True,timeout=55)
  output=run.stdout+run.stderr
  proved=run.returncode!=0 and "--- FAIL: Test" in output and "build failed" not in output
  results.append({"guard":name,"command":" ".join(command),"expected_failure":proved,"exit_code":run.returncode,"evidence":[line.strip() for line in output.splitlines() if "FAIL" in line or "placement_test.go:" in line][:6]})
  print(name+": "+("expected test failure" if proved else "NOT PROVED"),flush=True)
  if not proved: raise RuntimeError(output[-2500:])
 finally: path.write_text(original)
Path("docs/notes/machine-placement/negative-guards.json").write_text(json.dumps(results,indent=2)+"\n")
