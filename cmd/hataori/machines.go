package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/workerproto"
	"github.com/yousan/hataori/internal/workerssh"
)

func machineTargets(ctx context.Context, st *store.Store, machine string) ([]workerproto.Target, error) {
	rows, err := st.DB.QueryContext(ctx, `SELECT thread_id,session_id,run FROM machine_run_history WHERE machine=?`, machine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workerproto.Target{}
	for rows.Next() {
		var target workerproto.Target
		var sid int64
		if err := rows.Scan(&target.Thread, &sid, &target.Run); err != nil {
			return nil, err
		}
		target.Session = fmt.Sprintf("s-%d", sid)
		out = append(out, target)
	}
	return out, rows.Err()
}

func startMachineConnections(ctx context.Context, root string, cfg *instance.Config, st *store.Store, r *runner.Runner) (*workerssh.Pool, error) {
	if len(cfg.Machines) == 1 {
		return nil, nil
	}
	main, err := st.MainIdentity(ctx)
	if err != nil {
		return nil, err
	}
	pool, err := workerssh.New(ctx, workerssh.Options{RunDir: filepath.Join(root, "state/worker-connections")})
	if err != nil {
		return nil, err
	}
	good := false
	defer func() {
		if !good {
			pool.Close()
		}
	}()
	bindings, err := st.Machines(ctx)
	if err != nil {
		return nil, err
	}
	pinned := map[string]string{}
	for _, m := range bindings {
		pinned[m.Key] = m.Installation
	}
	r.Remotes = map[string]runner.RemoteMachine{}
	for _, m := range cfg.Machines {
		if m.Key == store.LocalMachine {
			continue
		}
		installation := pinned[m.Key]
		if installation != "" && m.Installation != "" && installation != m.Installation {
			return nil, store.ErrMachineBinding
		}
		if installation == "" {
			installation = m.Installation
		}
		if installation == "" {
			continue
		} // no implicit trust-on-first-contact
		binding := workerproto.Binding{Main: main, Worker: installation}
		w, err := pool.Worker(workerssh.Config{Destination: m.SSH, Root: m.Root, Binding: binding,
			Expected: func(ctx context.Context) ([]workerproto.Target, error) { return machineTargets(ctx, st, m.Key) },
			Reconcile: func(ctx context.Context, o workerssh.Observation) error {
				if err := st.BindMachineInstallation(ctx, m.Key, binding.Worker); err != nil {
					return err
				}
				return st.Update(ctx, func(tx *store.Tx) error {
					t := o.Health.Telemetry
					var load, memory, disk any
					if workerproto.FreshTelemetry(t, time.Now().UnixMilli()) {
						load, memory, disk = t.Load, t.MemoryFree, t.DiskFree
					}
					if _, err := tx.ExecContext(ctx, `INSERT INTO machine_health(machine,observed_at,boot,reconciled,load,free_memory_bytes,free_disk_bytes) VALUES(?,?,?,1,?,?,?) ON CONFLICT(machine) DO UPDATE SET observed_at=excluded.observed_at,boot=excluded.boot,reconciled=1,load=excluded.load,free_memory_bytes=excluded.free_memory_bytes,free_disk_bytes=excluded.free_disk_bytes`, m.Key, t.ObservedMS, o.Health.Boot, load, memory, disk); err != nil {
						return err
					}
					for _, a := range o.Health.Agents {
						state := a.Login
						if !a.Installed {
							state = a.Installation
						}
						if _, err := tx.ExecContext(ctx, `INSERT INTO machine_agent_health(machine,agent,config_dir,observed_at,state) VALUES(?,?,?,?,?) ON CONFLICT(machine,agent,config_dir) DO UPDATE SET observed_at=excluded.observed_at,state=excluded.state`, m.Key, a.Agent, a.ConfigDir, time.Now().UnixMilli(), state); err != nil {
							return err
						}
					}
					return nil
				})
			}})
		if err != nil {
			return nil, err
		}
		r.Remotes[m.Key] = runner.RemoteMachine{Binding: binding, Destination: m.SSH, Root: m.Root, Open: func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
			s, err := w.Open(ctx)
			if err != nil {
				return nil, nil, err
			}
			return s, s.Protocol, nil
		}}
	}
	good = true
	return pool, nil
}
