package plugin

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// rowsUnder returns the cells of each table row whose first cell is a
// name in backquotes, under heading in the file at path, up to the next
// heading; the first cell without its backquotes.
func rowsUnder(t *testing.T, path, heading string) [][]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]string
	in, found := false, false
	fenced := false
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "```") {
			fenced = !fenced
		}
		if fenced {
			continue
		}
		if strings.HasPrefix(l, "#") {
			if in {
				break
			}
			in = strings.TrimSpace(l) == heading
			found = found || in
			continue
		}
		if in && strings.HasPrefix(l, "| `") {
			cells := strings.Split(strings.Trim(l, "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			cells[0] = strings.Trim(cells[0], "`")
			rows = append(rows, cells)
		}
	}
	if !found {
		t.Fatalf("%s has no heading %q", path, heading)
	}
	return rows
}

func keysUnder(t *testing.T, path, heading string) []string {
	var keys []string
	for _, r := range rowsUnder(t, path, heading) {
		keys = append(keys, r[0])
	}
	return keys
}

// The plugins' documentation names every key of a manifest and of a config
// entry, and every hook point, that the code reads, and no other: a field
// added without its documentation fails this.
func TestDocsNameEveryFieldAndPoint(t *testing.T) {
	const plugins, config = "../../docs/plugins.md", "../../docs/configuration.md"
	for _, tc := range []struct {
		path, heading string
		code          []string
	}{
		{plugins, "## The manifest", manifestKeys},
		{plugins, "### `hooks[]`", manifestHookKeys},
		{plugins, "### Hook points", slices.Collect(maps.Keys(points))},
		{config, "### `plugins[]`", entryKeys},
		{config, "#### `plugins[].hooks[]`", entryHookKeys},
	} {
		got, want := keysUnder(t, tc.path, tc.heading), slices.Clone(tc.code)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s, %s: the document has %q, the code %q", tc.path, tc.heading, got, want)
		}
	}
	// Each point's kind, as the dispatcher has it, and its policy are in the
	// table too, and the dispatcher knows no other point.
	for p := range kinds {
		if _, ok := points[p]; !ok {
			t.Errorf("the dispatcher has a kind for %s, which is no hook point", p)
		}
	}
	policy := map[Failure]string{Block: "the input is blocked", Skip: "the handler is skipped"}
	for _, r := range rowsUnder(t, plugins, "### Hook points") {
		k, ok := kinds[r[0]]
		if !ok || len(r) < 4 {
			t.Errorf("hook point %s: no kind, or a short row %q", r[0], r)
			continue
		}
		if r[3] != policy[points[r[0]]] {
			t.Errorf("point %s: the document's policy is %q, the code's %q", r[0], r[3], policy[points[r[0]]])
		}
		var want, got []string
		if k.stop {
			want = append(want, "gate")
		}
		if k.change || k.add {
			want = append(want, "modify")
		}
		for _, s := range strings.Split(r[1], ",") {
			got = append(got, strings.TrimSpace(s))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("point %s: the document says %q, the dispatcher %q", r[0], got, want)
		}
	}
}
