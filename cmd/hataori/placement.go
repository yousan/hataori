package main

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/placement"
	"github.com/yousan/hataori/internal/plugin"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/worker"
)

func configurePlacement(ctx context.Context, root string, cfg *instance.Config, c *core.Core, r *runner.Runner, grants []plugin.Grant, adapters map[string]agent.Adapter) error {
	enabled := len(cfg.Machines) > 1
	for _, m := range cfg.Machines {
		enabled = enabled || m.MaxResident > 0 || m.MaxStarting > 0 || m.MaxLoad > 0 || m.MinFreeMemoryBytes > 0 || m.MinFreeDiskBytes > 0
	}
	var err error
	enabled, err = c.Store.PlacementEnabled(ctx, enabled)
	if err != nil {
		return fmt.Errorf("placement mode: %w", err)
	}
	if !enabled {
		return nil
	} // never-enabled implicit local retains existing single-machine behavior
	p := &core.MachinePlacement{Machines: map[string]core.PlacementMachine{}}
	for _, m := range cfg.Machines {
		p.Machines[m.Key] = core.PlacementMachine{Agents: slices.Clone(m.Agents), Limits: placement.Limits{MaxResident: m.MaxResident, MaxStarting: m.MaxStarting, MaxLoad: m.MaxLoad, MinFreeMemoryBytes: m.MinFreeMemoryBytes, MinFreeDiskBytes: m.MinFreeDiskBytes}}
	}
	p.Observe = func(ctx context.Context, key, name string) (placement.Observation, error) {
		if key != "local" {
			status, err := r.MachineAvailability(ctx, key)
			if err != nil {
				return placement.Observation{}, err
			}
			if status.State != "ready" {
				return placement.Observation{}, nil
			}
			if m, ok := r.Remotes[key]; ok && m.Observe != nil {
				return m.Observe(ctx, name)
			}
			return placement.Observation{}, nil
		}
		if err := c.ReconcileLocalPlacements(ctx); err != nil {
			return placement.Observation{}, err
		}
		t := worker.Resources(root, time.Now())
		x := placement.Observation{Ready: true, Reconciled: true, Known: t.Known && t.MemoryFree <= math.MaxInt64 && t.DiskFree <= math.MaxInt64, At: time.UnixMilli(t.ObservedMS), ValidFor: time.Duration(t.ValidMS) * time.Millisecond, Load: t.Load, Memory: int64(t.MemoryFree), Disk: int64(t.DiskFree)}
		ad, ok := adapters[name]
		if !ok {
			return x, nil
		}
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		h, err := ad.Probe(probe, agent.Spec{})
		if err == nil && !h.Unknown {
			x.HealthAt = time.Now()
			x.Installed = h.Installed
			x.LoggedIn = h.LoggedIn
		}
		return x, nil
	}
	p.Status = func(key string) control.MachineAvailability {
		if key == "local" {
			return control.MachineAvailability{State: "ready", At: time.Now()}
		}
		if m, ok := r.Remotes[key]; ok && m.Status != nil {
			return m.Status()
		}
		return control.MachineAvailability{State: "unavailable", At: time.Now(), Reason: "machine is not configured or registered"}
	}
	p.Caller = func(ctx context.Context, old control.Caller, scope, repo string) (control.Caller, error) {
		refuse := func() (control.Caller, error) {
			return control.Caller{}, fmt.Errorf("placement authorization changed: %w", control.ErrForbidden)
		}
		client, id, relayed := people.Split(old.From)
		if relayed && client == plugin.Client {
			for _, g := range grants {
				if g.ID == id {
					return g.Caller(), nil
				}
			}
			return refuse()
		}
		if relayed && client == supervisor.Client {
			if id != scope {
				return refuse()
			}
			for _, sc := range cfg.Scopes {
				if sc.Key == scope && sc.Supervisor != nil {
					fresh := supervisor.Caller(scope, old.AsksIn, sc.Supervisor.Autonomy)
					if fresh.Propose && old.Via != control.ViaProposal {
						return refuse()
					}
					if old.Via == control.ViaProposal {
						fresh.Propose = false
						fresh.Via = control.ViaProposal
					}
					if c.ApprovedStart == nil {
						return refuse()
					}
					if err := c.ApprovedStart(scope, repo); err != nil {
						return refuse()
					}
					return fresh, nil
				}
			}
			return refuse()
		}
		if old.Limit != nil || old.OrdinaryOnly || old.AsksIn != 0 {
			return refuse()
		}
		if relayed {
			if _, ok := cfg.Drivers[client]; !ok {
				return refuse()
			}
			for _, sc := range cfg.Scopes {
				if sc.Key == scope && (sc.Client == client || slices.ContainsFunc(sc.ShowIn, func(s string) bool { return strings.HasPrefix(s, client+":") })) {
					return control.Caller{From: old.From, Scopes: []string{scope}, May: control.OpStart | control.OpSend | control.OpAttach | control.OpStop | control.OpAnswer}, nil
				}
			}
			return refuse()
		}
		if old.Via == "dashboard" && cfg.Dashboard != nil {
			return dashboardCaller(cfg.People, []string{scope}, cfg.HostTerminals, cfg.HostInput, string(old.Principal)), nil
		}
		if old.Principal == control.Principal(cfg.People.Owner()) && old.Via == "" {
			return control.Caller{Principal: old.Principal, Scopes: []string{scope}, May: control.OpStart | control.OpSend | control.OpAttach | control.OpStop | control.OpAnswer}, nil
		}
		return refuse()
	}
	c.Placement = p
	r.AwayEnabled = true
	r.AwayGrace = cfg.MachineAwayGrace
	c.Away = &core.MachineAway{Status: r.MachineAvailability, Caller: p.Caller}
	r.RevalidatePending = c.RevalidatePending
	r.RevalidateInitial = c.RevalidateInitial
	r.Admit = c.AdmitSession
	r.RevalidateLaunch = c.RevalidatePlacementLaunch
	return nil
}
