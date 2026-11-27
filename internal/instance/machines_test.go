package instance

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func parseMachines(t *testing.T, machines, scopeMachines string) *Config {
	t.Helper()
	s := `{"owner":"ada","people":[{"name":"ada","ids":["chat:one"]},{"name":"grace","ids":["chat:two"]}],"trigger":"!go","idle":"1m","adapters":{"fake":{}},"drivers":{"chat":{}},"scopes":[{"key":"room","client":"chat","people":["grace"]` + scopeMachines + `}]` + machines + `}`
	c, err := ParseConfig([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMachineConfigDefaultsAndScopeLists(t *testing.T) {
	base := parseMachines(t, "", "")
	if !reflect.DeepEqual(base.Machines, []Machine{{Key: LocalMachine}}) || base.Scopes[0].Machines != nil {
		t.Fatalf("legacy config changed: %+v %+v", base.Machines, base.Scopes[0].Machines)
	}
	workers := `,"machines":[{"key":"build","ssh":"worker-alias","root":"/srv/worker","agents":["fake"],"max_resident":4,"max_starting":2,"max_load":0.75,"min_free_memory_bytes":1024,"min_free_disk_bytes":2048},{"key":"local","agents":[]}]`
	c := parseMachines(t, workers, `,"machines":[]`)
	if c.Scopes[0].Machines == nil || len(c.Scopes[0].Machines) != 0 || c.Machines[0].Agents == nil {
		t.Fatal("explicit empty list became unrestricted")
	}
	m := c.Machines[1]
	if m.Key != "build" || m.MaxResident != 4 || m.MaxStarting != 2 || m.MaxLoad != 0.75 || m.MinFreeMemoryBytes != 1024 || m.MinFreeDiskBytes != 2048 {
		t.Fatalf("machine limits: %+v", m)
	}
	if !reflect.DeepEqual(base.People, c.People) || !reflect.DeepEqual(base.Scopes[0].People, c.Scopes[0].People) {
		t.Fatal("machines changed person or scope authorization")
	}
	c = parseMachines(t, workers, `,"machines":["build"]`)
	if !reflect.DeepEqual(c.Scopes[0].Machines, []string{"build"}) {
		t.Fatal(c.Scopes[0].Machines)
	}
}

func TestMachineConfigRefusalsNameTheKey(t *testing.T) {
	for _, tc := range []struct{ machines, scope, key string }{
		{`[{"key":"Build"}]`, "", "machines[0].key"},
		{`[{"key":" build"}]`, "", "machines[0].key"},
		{`[{"key":"../build"}]`, "", "machines[0].key"},
		{`[{"key":""}]`, "", "machines[0].key"},
		{`[{"key":"local"},{"key":"local"}]`, "", "machines[1].key"},
		{`[{"key":"local","ssh":"worker"}]`, "", "machines[0].key"},
		{`[{"key":"local","root":"/srv/worker"}]`, "", "machines[0].key"},
		{`[{"key":"build","ssh":"-oProxyCommand=bad","root":"/srv/worker"}]`, "", "machines[0].ssh"},
		{`[{"key":"build","ssh":"worker alias","root":"/srv/worker"}]`, "", "machines[0].ssh"},
		{`[{"key":"build","root":"/srv/worker"}]`, "", "machines[0].ssh"},
		{`[{"key":"build","ssh":"worker","root":"relative"}]`, "", "machines[0].root"},
		{`[{"key":"build","ssh":"worker","root":"/srv/../worker"}]`, "", "machines[0].root"},
		{`[{"key":"build","ssh":"worker","root":"/"}]`, "", "machines[0].root"},
		{`[{"key":"local","agents":["unknown"]}]`, "", "machines[0].agents[0]"},
		{`[{"key":"local","agents":["fake","fake"]}]`, "", "machines[0].agents[1]"},
		{`[{"key":"local","max_resident":-1}]`, "", "machines[0].max_resident"},
		{`[{"key":"local","max_starting":0}]`, "", "machines[0].max_starting"},
		{`[{"key":"local","max_load":0}]`, "", "machines[0].max_load"},
		{`[{"key":"local","max_load":-0.5}]`, "", "machines[0].max_load"},
		{`[{"key":"local","min_free_memory_bytes":-1}]`, "", "machines[0].min_free_memory_bytes"},
		{`[{"key":"local","min_free_disk_bytes":-1}]`, "", "machines[0].min_free_disk_bytes"},
		{`[]`, `,"machines":["missing"]`, "scopes[0].machines[0]"},
		{`[]`, `,"machines":["local","local"]`, "scopes[0].machines[1]"},
		{`[]`, `,"machines":[" local "]`, "scopes[0].machines[0]"},
	} {
		t.Run(tc.key+"/"+tc.machines+tc.scope, func(t *testing.T) {
			data := `{"owner":"ada","trigger":"!go","idle":"1m","adapters":{"fake":{}},"machines":` + tc.machines + `,"scopes":[{"key":"room","client":"chat"` + tc.scope + `}]}`
			_, err := ParseConfig([]byte(data))
			if err == nil || !strings.Contains(err.Error(), tc.key) || strings.Contains(err.Error(), "\n") {
				t.Fatalf("expected key %s: %v", tc.key, err)
			}
		})
	}
	// Preserve index information in list decoding errors too.
	var raw map[string]any
	json.Unmarshal([]byte(`{"owner":"ada","trigger":"!go","idle":"1m","machines":[{"key":"local","max_load":"bad"}]}`), &raw)
	b, _ := json.Marshal(raw)
	if _, err := ParseConfig(b); err == nil || !strings.Contains(err.Error(), "max_load") {
		t.Fatal(err)
	}
}
