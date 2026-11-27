package main

import (
	"context"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/placement"
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
		var viewMu sync.RWMutex
		var view workerssh.Observation
		var received time.Time
		binding := workerproto.Binding{Main: main, Worker: installation}
		w, err := pool.Worker(workerssh.Config{Destination: m.SSH, Root: m.Root, Binding: binding,
			Expected: func(ctx context.Context) ([]workerproto.Target, error) { return machineTargets(ctx, st, m.Key) },
			Reconcile: func(ctx context.Context, o workerssh.Observation) error {
				if err := st.BindMachineInstallation(ctx, m.Key, binding.Worker); err != nil {
					return err
				}
				err := st.Update(ctx, func(tx *store.Tx) error {
					t := o.Health.Telemetry
					var load, memory, disk any
					if workerproto.FreshTelemetry(t, time.Now().UnixMilli()) {
						load, memory, disk = t.Load, t.MemoryFree, t.DiskFree
					}
					if _, err := tx.ExecContext(ctx, `INSERT INTO machine_health(machine,observed_at,boot,reconciled,load,free_memory_bytes,free_disk_bytes) VALUES(?,?,?,1,?,?,?) ON CONFLICT(machine) DO UPDATE SET observed_at=excluded.observed_at,boot=excluded.boot,reconciled=1,load=excluded.load,free_memory_bytes=excluded.free_memory_bytes,free_disk_bytes=excluded.free_disk_bytes`, m.Key, t.ObservedMS, o.Health.Boot, load, memory, disk); err != nil {
						return err
					}
					runs := make([]store.MachineRunObservation, 0, len(o.Inventory.Hosts))
					for _, host := range o.Inventory.Hosts {
						runs = append(runs, store.MachineRunObservation{Thread: int64(host.Target.Thread), Session: host.Target.Session, Run: host.Target.Run, State: host.State})
					}
					if err := tx.ObserveMachineRuns(ctx, m.Key, runs); err != nil {
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
				if err != nil {
					return err
				}
				viewMu.Lock()
				view = o
				received = time.Now()
				viewMu.Unlock()
				return nil
			}})
		if err != nil {
			return nil, err
		}
		r.Remotes[m.Key] = runner.RemoteMachine{Admission: &workerproto.Admission{MaxResident: m.MaxResident, MaxStarting: m.MaxStarting, MaxLoad: m.MaxLoad, MinFreeMemory: uint64(m.MinFreeMemoryBytes), MinFreeDisk: uint64(m.MinFreeDiskBytes)},
			Status: func() control.MachineAvailability {
				status := w.Status()
				viewMu.RLock()
				o, at := view, received
				viewMu.RUnlock()
				x := control.MachineAvailability{State: "unavailable", At: status.Since, Reason: "machine connection is unavailable"}
				if status.Ready {
					x.At = at
					x.Reason = "placement capability or resource observation is unavailable"
					if o.Placement && workerproto.FreshTelemetry(o.Health.Telemetry, time.Now().UnixMilli()) {
						x.State = "ready"
						x.Reason = ""
					}
				}
				return x
			}, Observe: func(ctx context.Context, name string) (placement.Observation, error) {
				viewMu.RLock()
				o, at := view, received
				viewMu.RUnlock()
				t := o.Health.Telemetry
				x := placement.Observation{Ready: w.Status().Ready && o.Placement, Reconciled: o.Inventory.Complete && o.Inventory.Boot == o.Health.Boot, Known: t.Known && t.MemoryFree <= math.MaxInt64 && t.DiskFree <= math.MaxInt64, At: time.UnixMilli(t.ObservedMS), HealthAt: at, ValidFor: time.Duration(t.ValidMS) * time.Millisecond, Load: t.Load, Memory: int64(t.MemoryFree), Disk: int64(t.DiskFree)}
				for _, a := range o.Health.Agents {
					if a.Agent == name && a.Config == "default" {
						x.Installed = a.Installed && a.Installation == "installed"
						x.LoggedIn = a.Login == "logged_in"
						break
					}
				}
				return x, nil
			}, Binding: binding, Destination: m.SSH, Root: m.Root, Open: func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
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
