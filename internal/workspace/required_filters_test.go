package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateWithARequiredFilter(t *testing.T) {
	f := newRepo(t)
	ran := f.recordFilter(f.clone, "x", "clean")
	f.git(f.clone, "config", "filter.x.required", "true")
	f.write("a", "changed here\n")
	st := f.mustState()
	if st.Uncommitted != 1 || !st.FiltersDisabled {
		t.Fatalf("state: %+v", st)
	}
	if _, err := os.Lstat(ran); !os.IsNotExist(err) {
		t.Fatalf("State ran the filter, or marker unreadable: %v", err)
	}
}

func TestRequiredFilterIsRefusedAsModified(t *testing.T) {
	x := newRemoval(t)
	ran := x.recordFilter(x.clone, "x", "clean")
	x.git(x.clone, "config", "filter.x.required", "true")
	x.write("a", "changed here\n")
	wantRefusal(t, x.check(), Modified, "")
	if _, err := os.Lstat(ran); !os.IsNotExist(err) {
		t.Fatalf("Check ran the filter, or marker unreadable: %v", err)
	}
}

// The clean filter maps WORK to the committed bytes. Touching the file
// forces git to compare content, but State cannot run that clean command.
// The raw difference must carry an explicit qualification to its counts.
func TestStateExplainsUnchangedFilteredContent(t *testing.T) {
	f := newRepo(t)
	ran := f.recordFilter(f.clone, "x", "clean")
	f.git(f.clone, "config", "filter.x.clean", "sed 's/WORK/one/g'")
	f.git(f.clone, "config", "filter.x.smudge", "sed 's/one/WORK/g'")
	f.git(f.clone, "config", "filter.x.required", "true")
	f.write("a", "replace through smudge\n")
	f.git(f.clone, "checkout-index", "--force", "a")
	// Refresh the index with the normalized bytes before checking the
	// fixture. The committed content remains unchanged.
	f.git(f.clone, "add", "a")
	if got := f.git(f.clone, "status", "--porcelain"); got != "" {
		t.Fatalf("filtered checkout is not clean: %s", got)
	}
	p := filepath.Join(f.clone, "a")
	before, err := os.ReadFile(p)
	if err != nil || string(before) != "WORK\ntwo\nthree\n" {
		t.Fatalf("smudged content %q: %v", before, err)
	}
	f.git(f.clone, "config", "filter.x.clean", "touch "+ran+"; sed 's/WORK/one/g'")
	at := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	st := f.mustState()
	if st.Uncommitted != 1 || !st.FiltersDisabled {
		t.Fatalf("raw comparison lacks qualification: %+v", st)
	}
	if _, err := os.Lstat(ran); !os.IsNotExist(err) {
		t.Fatalf("State ran the filter, or marker unreadable: %v", err)
	}
	after, err := os.ReadFile(p)
	if err != nil || string(after) != string(before) {
		t.Fatalf("State changed the filtered content: %q, %v", after, err)
	}
}

func TestStateWithARequiredFilterWithoutCommands(t *testing.T) {
	f := newRepo(t)
	f.git(f.clone, "config", "filter.x.required", "true")
	if err := os.WriteFile(filepath.Join(f.clone, ".git", "info", "attributes"), []byte("* filter=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.write("a", "changed\n")
	if st := f.mustState(); st.Uncommitted != 1 || !st.FiltersDisabled {
		t.Fatalf("state: %+v", st)
	}
}
