package cipolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the actual git receiver, rather than assuming that an ordinary push sends
// a no-op assertion. These repositories contain only generated test objects.
func realGitWrite(t *testing.T) (string, integrationWrite) {
	t.Helper()
	dir := t.TempDir()
	command(t, dir, "git", "init", "--bare", "-q", "repo.git")
	git := func(input string, args ...string) string {
		c := exec.Command("git", append([]string{"--git-dir=repo.git"}, args...)...)
		c.Dir = dir
		c.Stdin = strings.NewReader(input)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatal(args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	tree := git("", "mktree")
	base := git("base\n", "commit-tree", tree)
	head := git("head\n", "commit-tree", tree, "-p", base)
	merge := git("merge\n", "commit-tree", tree, "-p", base, "-p", head)
	git("", "update-ref", "refs/heads/integration", base)
	git("", "update-ref", "refs/heads/candidate", head)
	return dir, integrationWrite{Base: base, Head: head, Merge: merge, Ref: "integration", Source: "candidate"}
}
func receive(t *testing.T, dir string, data []byte) []byte {
	t.Helper()
	c := exec.Command("git", "receive-pack", "--stateless-rpc", "repo.git")
	c.Dir = dir
	c.Stdin = bytes.NewReader(data)
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func actualAdvertisement(t *testing.T, dir string) []byte {
	t.Helper()
	c := exec.Command("git", "receive-pack", "--stateless-rpc", "--advertise-refs", "repo.git")
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	return append(append(gitPacket("# service=git-receive-pack\n"), []byte("0000")...), out...)
}

func TestAtomicGitRealReceiverBothRefGuards(t *testing.T) {
	for _, scenario := range []string{"success", "base-moved", "head-moved", "head-races-after-advertisement", "source-rejected"} {
		t.Run(scenario, func(t *testing.T) {
			dir, w := realGitWrite(t)
			if err := advertisedAtomic(actualAdvertisement(t, dir), w); err != nil {
				t.Fatal(err)
			}
			if scenario == "base-moved" {
				command(t, dir, "git", "--git-dir=repo.git", "update-ref", "refs/heads/integration", w.Head)
			}
			if scenario == "head-moved" || scenario == "head-races-after-advertisement" {
				command(t, dir, "git", "--git-dir=repo.git", "update-ref", "refs/heads/candidate", w.Base)
			}
			if scenario == "source-rejected" {
				hook := filepath.Join(dir, "repo.git/hooks/update")
				os.WriteFile(hook, []byte("#!/bin/sh\n[ \"$1\" != refs/heads/candidate ]\n"), 0700)
			}
			response := receive(t, dir, atomicCommands(w))
			err := atomicReport(response, w)
			tip := strings.TrimSpace(command(t, dir, "git", "--git-dir=repo.git", "rev-parse", "refs/heads/integration"))
			if scenario == "success" {
				if err != nil || tip != w.Merge {
					t.Fatal(string(response), err, tip)
				}
			} else {
				if err == nil || tip == w.Merge {
					t.Fatal("partial/unguarded update", string(response), tip)
				}
			}
		})
	}
}

func TestGitCapturedProtocolAndClosedVariants(t *testing.T) {
	var w integrationWrite
	raw, err := os.ReadFile("testdata/git/transaction.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	adv, _ := os.ReadFile("testdata/git/advertisement.pkt")
	report, _ := os.ReadFile("testdata/git/success.pkt")
	if err = advertisedAtomic(adv, w); err != nil {
		t.Fatal(err)
	}
	if err = atomicReport(report, w); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{nil, report[:len(report)-1], append(append([]byte{}, report...), byte('x')), bytes.ReplaceAll(report, []byte("ok refs/heads/candidate"), []byte("ng refs/heads/candidate")), []byte("0000")} {
		if atomicReport(bad, w) == nil {
			t.Fatal("incomplete/rejected report accepted")
		}
	}
	noAtomic := bytes.ReplaceAll(adv, []byte("atomic"), []byte("denied"))
	if advertisedAtomic(noAtomic, w) == nil {
		t.Fatal("unsupported atomic accepted")
	}
	for _, ref := range []string{"main.lock", "a/../b", "a//b", "a/.hidden", "../main", "main\ncommand", "a@{1}"} {
		if safeBranch(ref) {
			t.Fatal("unsafe ref", ref)
		}
	}
}

func TestGitTransportRedirectAndPartialSuccessClose(t *testing.T) {
	_, w := realGitWrite(t)
	received := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received++ }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		http.Redirect(out, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	a := &Adapter{root: AuthorityRoot{Repository: "example/repo"}, gitBase: server.URL, client: server.Client(), token: "test-only-not-live"}
	if _, err := a.gitHTTP(context.Background(), "POST", "git-receive-pack", atomicCommands(w)); err == nil || received != 0 {
		t.Fatal("redirect followed", received, err)
	}
}
