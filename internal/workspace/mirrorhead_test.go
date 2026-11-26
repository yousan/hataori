package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMirrorFollowsDefaultBranchWhileOldBranchRemains(t *testing.T) {
	x := newMirrorFixture(t)
	x.push("main", "refs/heads/develop")
	x.make()
	before := x.refs("refs/heads", "refs/tags")
	for _, branch := range []string{"develop", "main"} {
		x.git(x.bare, "symbolic-ref", "HEAD", "refs/heads/"+branch)
		dir := x.make()
		if got := x.git(dir, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
			t.Fatalf("workspace HEAD = %q, want %q", got, "refs/heads/"+branch)
		}
		if got := x.git(x.mirror, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
			t.Fatal("mirror HEAD:", got)
		}
		x.wantSame()
		if got := x.refs("refs/heads", "refs/tags"); !slices.Equal(got, before) {
			t.Fatalf("default branch change altered refs: %v, want %v", got, before)
		}
	}
}

func TestMirrorKeepsHeadWhenUpstreamHeadCannotBeUsed(t *testing.T) {
	for _, scenario := range []string{"missing branch", "detached", "query fails", "branch cannot be fetched"} {
		t.Run(scenario, func(t *testing.T) {
			x := newMirrorFixture(t)
			x.make()
			switch scenario {
			case "missing branch":
				x.git(x.bare, "symbolic-ref", "HEAD", "refs/heads/nowhere")
			case "detached":
				id := x.git(x.bare, "rev-parse", "HEAD")
				x.git(x.bare, "update-ref", "--no-deref", "HEAD", id)
			case "query fails":
				x.push("main", "refs/heads/develop")
				x.git(x.bare, "symbolic-ref", "HEAD", "refs/heads/develop")
				x.wrapGit(`case " $* " in *' ls-remote --symref '*) exit 1;; esac`)
			case "branch cannot be fetched":
				x.push("main", "refs/heads/develop")
				x.git(x.bare, "symbolic-ref", "HEAD", "refs/heads/develop")
				if err := os.WriteFile(filepath.Join(x.mirror, "refs", "heads", "develop.lock"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dir := x.make()
			if got := x.git(dir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
				t.Fatal("workspace HEAD changed:", got)
			}
			if got := x.git(x.mirror, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
				t.Fatal("mirror HEAD changed:", got)
			}
			if got := x.refs("refs/heads/main"); len(got) != 1 {
				t.Fatal("old branch was dropped:", got)
			}
		})
	}
}
