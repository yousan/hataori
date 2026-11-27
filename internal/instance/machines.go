package instance

import (
	"fmt"
	"math"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/yousan/hataori/internal/registry"
	"github.com/yousan/hataori/internal/workerproto"
)

// LocalMachine is the main machine, reserved against SSH rebinding.
const LocalMachine = "local"

// Machine describes one configured installation. Limits are admission limits,
// not resource quotas for running processes. Zero leaves a limit unset.
type Machine struct {
	Key                string
	SSH                string
	Root               string   // absolute POSIX path on the worker, never resolved on main
	Installation       string   // explicit first-contact registration; later contacts use its pinned identity
	Agents             []string // nil: all configured adapters; empty: none
	MaxResident        int
	MaxStarting        int
	MaxLoad            float64 // one-minute load divided by logical CPUs
	MinFreeMemoryBytes int64
	MinFreeDiskBytes   int64
}

type fileMachine struct {
	Key                string   `json:"key"`
	SSH                string   `json:"ssh"`
	Root               string   `json:"root"`
	Installation       string   `json:"installation"`
	Agents             []string `json:"agents"`
	MaxResident        *int     `json:"max_resident"`
	MaxStarting        *int     `json:"max_starting"`
	MaxLoad            *float64 `json:"max_load"`
	MinFreeMemoryBytes *int64   `json:"min_free_memory_bytes"`
	MinFreeDiskBytes   *int64   `json:"min_free_disk_bytes"`
}

func machineConfig(fs []fileMachine, adapters map[string][]byte) ([]Machine, error) {
	ms := []Machine{{Key: LocalMachine}}
	seen := map[string]bool{}
	for i, f := range fs {
		key := fmt.Sprintf("machines[%d]", i)
		// Identity keys are deliberately not trimmed or case-folded.
		if err := registry.ValidName(f.Key); err != nil {
			return nil, fmt.Errorf("config: key %q: %w", key+".key", err)
		}
		if seen[f.Key] {
			return nil, fmt.Errorf("config: key %q: machine %q is listed twice", key+".key", f.Key)
		}
		seen[f.Key] = true
		m := Machine{Key: f.Key, SSH: strings.TrimSpace(f.SSH), Root: strings.TrimSpace(f.Root), Agents: slices.Clone(f.Agents)}
		m.Installation = f.Installation
		if m.Installation != "" && !workerproto.ValidID(m.Installation) {
			return nil, fmt.Errorf("config: %s.installation: invalid worker identity", key)
		}
		if m.Key == LocalMachine {
			if m.SSH != "" || m.Root != "" || m.Installation != "" {
				return nil, fmt.Errorf("config: key %q: local is reserved; ssh and root must be absent", key+".key")
			}
		} else {
			if m.SSH == "" || strings.HasPrefix(m.SSH, "-") || strings.ContainsAny(m.SSH, "/\\:") || strings.ContainsFunc(m.SSH, unicode.IsSpace) || strings.ContainsFunc(m.SSH, unicode.IsControl) {
				return nil, fmt.Errorf("config: key %q: expected an SSH destination or alias without options", key+".ssh")
			}
			if !path.IsAbs(m.Root) || path.Clean(m.Root) != m.Root || m.Root == "/" || strings.ContainsFunc(m.Root, unicode.IsControl) {
				return nil, fmt.Errorf("config: key %q: expected a canonical absolute worker directory", key+".root")
			}
		}
		for j, name := range m.Agents {
			k := fmt.Sprintf("%s.agents[%d]", key, j)
			if _, ok := adapters[name]; !ok {
				return nil, fmt.Errorf("config: key %q: agent is not configured under adapters", k)
			}
			if slices.Contains(m.Agents[:j], name) {
				return nil, fmt.Errorf("config: key %q: duplicate agent", k)
			}
		}
		for _, n := range []struct {
			key string
			v   *int
			to  *int
		}{
			{"max_resident", f.MaxResident, &m.MaxResident}, {"max_starting", f.MaxStarting, &m.MaxStarting},
		} {
			if n.v != nil {
				if *n.v <= 0 {
					return nil, fmt.Errorf("config: key %q: must be positive; omit for no limit", key+"."+n.key)
				}
				*n.to = *n.v
			}
		}
		if f.MaxLoad != nil {
			if *f.MaxLoad <= 0 || math.IsInf(*f.MaxLoad, 0) || math.IsNaN(*f.MaxLoad) {
				return nil, fmt.Errorf("config: key %q: must be positive and finite", key+".max_load")
			}
			m.MaxLoad = *f.MaxLoad
		}
		for _, n := range []struct {
			key string
			v   *int64
			to  *int64
		}{
			{"min_free_memory_bytes", f.MinFreeMemoryBytes, &m.MinFreeMemoryBytes}, {"min_free_disk_bytes", f.MinFreeDiskBytes, &m.MinFreeDiskBytes},
		} {
			if n.v != nil {
				if *n.v < 0 {
					return nil, fmt.Errorf("config: key %q: must be nonnegative", key+"."+n.key)
				}
				*n.to = *n.v
			}
		}
		if m.Key == LocalMachine {
			ms[0] = m
		} else {
			ms = append(ms, m)
		}
	}
	return ms, nil
}

func machineReferences(refs []string, ms []Machine, key string) ([]string, error) {
	for i, ref := range refs {
		if !slices.ContainsFunc(ms, func(m Machine) bool { return m.Key == ref }) {
			return nil, fmt.Errorf("config: key %q: unknown machine", fmt.Sprintf("%s[%d]", key, i))
		}
		if slices.Contains(refs[:i], ref) {
			return nil, fmt.Errorf("config: key %q: duplicate machine", fmt.Sprintf("%s[%d]", key, i))
		}
	}
	return slices.Clone(refs), nil
}
