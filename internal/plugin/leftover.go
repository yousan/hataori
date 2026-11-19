package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// EnvRun is the variable that marks every process of one start of a
// plugin: a random value, new at each start, that the plugin's children
// inherit. A process of an earlier run is told by it (leftovers).
const EnvRun = "HATAORI_PLUGIN_RUN"

// runRecord is what a start of a plugin leaves in
// state/plugin-run/<id>/run-<run>.json, written before the plugin starts
// and removed once its process group is killed: the value of EnvRun and
// the boot it runs in. A record found at the next start is of a daemon
// that died without stopping the plugin.
type runRecord struct {
	Run  string `json:"run"`
	Boot string `json:"boot"`
}

func (r *Runner) recordPath(g Grant, run string) string {
	return filepath.Join(r.State, "plugin-run", g.ID, "run-"+run+".json")
}

// leftovers kills what earlier runs of g left, when the daemon died
// without stopping it, before g starts again. A process is of an earlier
// run when it runs as this OS user, on the boot the run recorded, and its
// environment holds that run's EnvRun: a random value no other process
// has, unless it inherited it from that run. Nothing else is signalled,
// whatever its process group or pid. On Linux the process is held by a
// pidfd from before the check until the signal; on macOS its start time is
// read again just before the signal. A record is removed once its run's
// processes were all looked for; one that could not be, or whose look ctx
// ended, is kept and tried again at the next start.
func (r *Runner) leftovers(ctx context.Context, g Grant) {
	paths, _ := filepath.Glob(filepath.Join(r.State, "plugin-run", g.ID, "run-*.json"))
	if len(paths) == 0 {
		return
	}
	boot, err := bootID()
	if err != nil {
		r.Log.Warn("plugin: this boot's id cannot be read; what earlier runs left is not looked for", "plugin", g.ID, "err", err)
		return
	}
	for _, path := range paths {
		var rec runRecord
		b, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(b, &rec) != nil || len(rec.Run) != 32 || strings.Trim(rec.Run, "0123456789abcdef") != "" ||
			filepath.Base(path) != "run-"+rec.Run+".json" {
			r.Log.Warn("plugin: an earlier run's record cannot be read; nothing of it is signalled", "plugin", g.ID, "record", filepath.Base(path))
			os.Remove(path)
			continue
		}
		if rec.Boot != boot {
			os.Remove(path) // another boot: nothing of that run runs
			continue
		}
		killed, done := r.sweep(ctx, g, rec.Run)
		if killed > 0 {
			r.Log.Warn("plugin: killed what an earlier run left", "plugin", g.ID, "processes", killed)
		}
		if done {
			os.Remove(path)
		}
	}
}

// sweep kills every process of this user whose environment holds run's
// EnvRun, the daemon's own aside. It looks again after a pass that killed
// some, so that what one forked meanwhile is found too, up to a few
// passes. It returns how many it killed, and whether every process was
// looked for: not when the processes could not be listed, one could not
// be checked or killed, or ctx ended.
func (r *Runner) sweep(ctx context.Context, g Grant, run string) (int, bool) {
	list := ownProcesses
	if r.processes != nil {
		list = r.processes
	}
	marker := EnvRun + "=" + run
	total := 0
	for pass := 0; pass < 5; pass++ {
		pids, err := list()
		if err != nil {
			r.Log.Warn("plugin: processes cannot be listed; what a run left is looked for at the next start", "plugin", g.ID, "err", err)
			return total, false
		}
		killed, failed := 0, 0
		for _, pid := range pids {
			if ctx.Err() != nil {
				return total + killed, false
			}
			if pid == os.Getpid() {
				continue
			}
			ok, err := killIfOfRun(pid, marker)
			if err != nil {
				failed++
				r.Log.Warn("plugin: a process could not be checked or killed", "plugin", g.ID, "pid", pid, "err", err)
			} else if ok {
				killed++
			}
		}
		total += killed
		if failed > 0 {
			return total, false
		}
		if killed == 0 {
			return total, true
		}
	}
	return total, false // still more, after the last pass: tried again at the next start
}

// hasEnv reports whether the NUL-separated list env holds the entry kv.
func hasEnv(env []byte, kv string) bool {
	for len(env) > 0 {
		i := 0
		for i < len(env) && env[i] != 0 {
			i++
		}
		if string(env[:i]) == kv {
			return true
		}
		if i == len(env) {
			break
		}
		env = env[i+1:]
	}
	return false
}
