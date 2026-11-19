package plugin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// EnvRun is the variable that marks every process of one start of a
// plugin: a random value, new at each start, that the plugin's children
// inherit. A process of an earlier run is told by it (leftovers).
const EnvRun = "HATAORI_PLUGIN_RUN"

// runRecord is what a start of a plugin leaves in
// state/plugin-run/<id>/run.json while it runs: its process group, the
// value of EnvRun, and the boot it runs in. A stop that kills the group
// removes it, so one that is found at the next start is of a daemon that
// died without stopping.
type runRecord struct {
	Group int    `json:"group"`
	Run   string `json:"run"`
	Boot  string `json:"boot"`
}

func (r *Runner) recordPath(g Grant) string {
	return filepath.Join(r.State, "plugin-run", g.ID, "run.json")
}

// leftovers kills what an earlier run of g left, when the daemon died
// without stopping it, before g starts again: each process in the
// recorded process group that is of this OS user and was started in the
// recorded run, as its environment shows (EnvRun), and only on the
// recorded boot. A process the group id was given to again, after the
// earlier run's were gone or after a reboot, has no such value and is not
// signalled. On Linux the process is checked and signalled through a
// pidfd, so that a pid given again between the two is not signalled; on
// macOS its start time is read again just before the signal.
func (r *Runner) leftovers(g Grant) {
	path := r.recordPath(g)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	defer os.Remove(path)
	var rec runRecord
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.Group <= 1 || len(rec.Run) < 16 {
		r.Log.Warn("plugin: an earlier run's record cannot be read; nothing of it is signalled", "plugin", g.ID, "err", err)
		return
	}
	boot, err := bootID()
	if err != nil || boot != rec.Boot {
		return // another boot: nothing of that run is left
	}
	pids, err := groupMembers(rec.Group)
	if err != nil {
		r.Log.Warn("plugin: the processes of an earlier run cannot be listed", "plugin", g.ID, "err", err)
		return
	}
	marker := EnvRun + "=" + rec.Run
	killed := 0
	for _, pid := range pids {
		if pid == os.Getpid() {
			continue
		}
		if ok, err := killIfOfRun(pid, marker); err != nil {
			r.Log.Warn("plugin: a process of an earlier run could not be checked or killed", "plugin", g.ID, "pid", pid, "err", err)
		} else if ok {
			killed++
		}
	}
	if killed > 0 {
		r.Log.Warn("plugin: killed what an earlier run left", "plugin", g.ID, "processes", killed)
	}
}

// hasEnv reports whether the NUL-separated environment env holds the
// entry kv.
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
