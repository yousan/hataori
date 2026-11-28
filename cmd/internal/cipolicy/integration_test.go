package cipolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type ownerSeam struct {
	posts              int
	mode               string
	wireBase, wireHead string
}

// Mutated captured GitHub records model the provider seam. The separate actual
// local Git receiver tests validate wire atomicity. Neither grants live approval.
func integrationFixture(t *testing.T) (*Coordinator, *githubFixture, *ownerSeam) {
	t.Helper()
	c, f, g := routeFixture(t)
	ctx := context.Background()
	f.adapter.root.IntegrationWrite = IntegrationWriteContract{Enabled: true, Writer: f.root.Dispatcher}
	snap, _ := c.store.Read(ctx)
	o, err := f.adapter.Observe(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.store.SwitchControl(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	g.Feedback.Authority = f.adapter.authorityDigest()
	f.adapter.root.Routes[1107] = g
	f.run.Title = sweepTitle(g.Feedback)
	f.candidate.Head.Ref = "candidate"
	f.candidate.MergeSHA = sha(102)
	f.commits[sha(102)] = Commit{SHA: sha(102), Parent: g.Base, IntegratedAt: clock.Add(time.Minute), Code: true}
	seam := &ownerSeam{wireBase: g.Base, wireHead: g.Head}
	f.adapter.gitBase = f.adapter.base
	f.gitServe = func(out http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == f.adapter.prefix()+"/git/commits/"+sha(102) {
			f.mu.Lock()
			defer f.mu.Unlock()
			v := map[string]any{"sha": sha(102), "parents": []map[string]string{{"sha": g.Base}, {"sha": g.Head}}, "tree": map[string]string{"sha": sha(103)}}
			if f.tamper != nil {
				f.tamper(r.URL.Path, v)
			}
			json.NewEncoder(out).Encode(v)
			return true
		}
		if !strings.HasPrefix(r.URL.Path, "/"+f.root.Repository+".git/") {
			return false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == "GET" {
			out.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			raw := append(gitPacket("# service=git-receive-pack\n"), []byte("0000")...)
			caps := "report-status atomic object-format=sha1"
			if seam.mode == "no-atomic" {
				caps = "report-status object-format=sha1"
			}
			raw = append(raw, gitPacket(fmt.Sprintf("%s refs/heads/%s\x00%s\n", seam.wireBase, g.Ref, caps))...)
			raw = append(raw, gitPacket(fmt.Sprintf("%s refs/heads/candidate\n", seam.wireHead))...)
			out.Write(append(raw, []byte("0000")...))
			return true
		}
		seam.posts++
		body, _ := io.ReadAll(r.Body)
		expected := integrationWrite{Base: g.Base, Head: g.Head, Merge: sha(102), Ref: g.Ref, Source: "candidate"}
		if !bytes.Equal(body, atomicCommands(expected)) {
			http.Error(out, "contract", 400)
			return true
		}
		out.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		if seam.mode == "reject" {
			out.Write(append(append(gitPacket("unpack ok\n"), gitPacket("ng refs/heads/develop atomic transaction failed\n")...), []byte("0000")...))
			return true
		}
		seam.wireBase = sha(102)
		f.head = sha(102)
		if seam.mode != "delayed-recognition" {
			f.candidate.Merged = true
			f.candidate.State = "closed"
			f.candidate.MergedAt = clock.Add(time.Minute)
		}
		if seam.mode == "lost" {
			conn, _, err := out.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return true
		}
		raw := gitPacket("unpack ok\n")
		raw = append(raw, gitPacket("ok refs/heads/develop\n")...)
		raw = append(raw, gitPacket("ok refs/heads/candidate\n")...)
		out.Write(append(raw, []byte("0000")...))
		return true
	}
	return c, f, seam
}

func TestOwnerCallerAdmissionWriteObserveDone(t *testing.T) {
	c, _, seam := integrationFixture(t)
	ctx := context.Background()
	out, err := c.Execute(ctx, "integrate", 1107, 0)
	if err != nil || out.Route == nil || !out.Route.Integrated || out.Route.DoneAllowed || out.Route.ReleaseAllowed || out.Route.UpgradeAllowed || seam.posts != 1 {
		t.Fatal(out, err, seam.posts)
	}
	out, err = c.Execute(ctx, "done", 1107, 0)
	if err != nil || out.Route == nil || out.Route.DoneAllowed {
		t.Fatal("uncovered write allowed Done", out, err)
	}
	if _, err = c.Execute(ctx, "pump", 0, 0); err != nil {
		t.Fatal(err)
	}
	out, err = c.Execute(ctx, "done", 1107, 0)
	if err != nil || out.Route == nil || !out.Route.DoneAllowed || out.Route.ReleaseAllowed || out.Route.UpgradeAllowed {
		t.Fatal(out, err)
	}
	if _, err = c.Execute(ctx, "integrate", 1107, 0); err != nil || seam.posts != 1 {
		t.Fatal("repeat write", err, seam.posts)
	}
}

func TestOwnerCallerLostWriteRestartAndDelayedRecognition(t *testing.T) {
	for _, mode := range []string{"lost", "delayed-recognition", "reject"} {
		t.Run(mode, func(t *testing.T) {
			c, f, seam := integrationFixture(t)
			ctx := context.Background()
			seam.mode = mode
			if _, err := c.Execute(ctx, "integrate", 1107, 0); !errors.Is(err, ErrAmbiguous) {
				t.Fatal(err)
			}
			var file string
			if err := c.store.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&file); err != nil {
				t.Fatal(err)
			}
			other, err := OpenStore(file)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			other.clock = c.store.clock
			restarted := NewCoordinator(f.adapter, other)
			if mode == "delayed-recognition" {
				if _, err = restarted.Execute(ctx, "integrate", 1107, 0); !errors.Is(err, ErrAmbiguous) {
					t.Fatal("unrecognized effect accepted", err)
				}
				f.mu.Lock()
				f.candidate.Merged = true
				f.candidate.State = "closed"
				f.candidate.MergedAt = clock.Add(time.Minute)
				f.mu.Unlock()
			}
			out, err := restarted.Execute(ctx, "integrate", 1107, 0)
			if mode == "reject" {
				if !errors.Is(err, ErrAmbiguous) {
					t.Fatal(err)
				}
			} else if err != nil || out.Route == nil || !out.Route.Integrated {
				t.Fatal(out, err)
			}
			if seam.posts != 1 {
				t.Fatal("blind write replay", seam.posts)
			}
		})
	}
}

func TestOwnerCallerAuthorizationAndIdentityFailures(t *testing.T) {
	for _, scenario := range []string{"inactive", "unadopted", "missing-consent", "missing-review", "expired", "head-moved", "base-moved", "source-main", "wrong-merge-parents", "no-atomic"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, seam := integrationFixture(t)
			switch scenario {
			case "inactive":
				f.adapter.root.IntegrationWrite.Enabled = false
			case "unadopted":
				f.adapter.root.Routes = nil
			case "missing-consent":
				f.consent.(map[string]any)["body"] = "withdrawn"
			case "missing-review":
				g := f.adapter.root.Routes[1107]
				g.Reviews = []Identity{f.reviews[0].User}
				f.adapter.root.Routes[1107] = g
			case "expired":
				g := f.adapter.root.Routes[1107]
				g.ExpiresAt = c.store.clock()
				f.adapter.root.Routes[1107] = g
			case "head-moved":
				seam.wireHead = sha(999)
			case "base-moved":
				seam.wireBase = sha(999)
			case "source-main":
				f.candidate.Head.Ref = "main"
			case "wrong-merge-parents":
				f.tamper = func(p string, v any) {
					if strings.HasSuffix(p, "/git/commits/"+sha(102)) {
						v.(map[string]any)["parents"] = []map[string]string{{"sha": sha(999)}, {"sha": sha(101)}}
					}
				}
			case "no-atomic":
				seam.mode = "no-atomic"
			}
			out, err := c.Execute(context.Background(), "integrate", 1107, 0)
			if err == nil || seam.posts != 0 || (out.Route != nil && out.Route.Allowed) {
				t.Fatal("unsafe write", scenario, out, err, seam.posts)
			}
			if scenario == "unadopted" && (out.Route == nil || out.Route.Route != "main") {
				t.Fatal("existing owner retargeted", out)
			}
		})
	}
}

func prepareOwnerIntent(t *testing.T, c *Coordinator, f *githubFixture) (Snapshot, integrationIntent, verifiedControl) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.Execute(ctx, "admission", 1107, 0); err != nil {
		t.Fatal(err)
	}
	snap, err := c.store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control, err := f.adapter.control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g := f.adapter.root.Routes[1107]
	w, err := f.adapter.prepareIntegration(ctx, 1107, g)
	if err != nil {
		t.Fatal(err)
	}
	i, err := c.store.prepareIntegration(ctx, snap, w, g, control.selection, f.adapter.root.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return snap, i, control
}

func TestOwnerClaimRejectsRenewedWithdrawnExpiredReservation(t *testing.T) {
	for _, scenario := range []string{"renewed", "withdrawn", "expired", "control-transition"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, seam := integrationFixture(t)
			ctx := context.Background()
			snap, i, control := prepareOwnerIntent(t, c, f)
			switch scenario {
			case "renewed":
				g := f.adapter.root.Routes[1107]
				g.ConsentID++
				if err := c.store.reserve(ctx, snap.Version, 1107, g, control.selection, f.adapter.root.ExpiresAt); err != nil {
					t.Fatal(err)
				}
			case "withdrawn":
				f.candidate.State = "closed"
				if err := c.reconcileReservation(ctx, snap); err != nil {
					t.Fatal(err)
				}
			case "expired":
				now := i.ExpiresAt
				c.store.clock = func() time.Time { return now }
			case "control-transition":
				o, err := f.adapter.Observe(ctx, snap)
				if err != nil {
					t.Fatal(err)
				}
				if err = c.store.SwitchControl(ctx, snap.Version, o); err != nil {
					t.Fatal(err)
				}
			}
			claim, err := c.store.claimIntegration(ctx, snap.Version, i, control.selection, f.adapter.root.ExpiresAt)
			if claim || err == nil || seam.posts != 0 {
				t.Fatal("stale claim crossed boundary", claim, err, seam.posts)
			}
		})
	}
}

func TestOwnerPreparedCloseRetiresButUncertainCloseDoesNot(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			c, f, _ := integrationFixture(t)
			ctx := context.Background()
			snap, i, control := prepareOwnerIntent(t, c, f)
			if uncertain {
				claim, err := c.store.claimIntegration(ctx, snap.Version, i, control.selection, f.adapter.root.ExpiresAt)
				if err != nil || !claim {
					t.Fatal(claim, err)
				}
			}
			f.candidate.State = "closed"
			err := c.reconcileReservation(ctx, snap)
			current, e := integrationIntentAt(ctx, c.store.db, 1107)
			if e != nil {
				t.Fatal(e)
			}
			r, e := c.store.reservation(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if uncertain {
				if !errors.Is(err, ErrAmbiguous) || r == nil || current.Status != "uncertain" {
					t.Fatal(current, r, err)
				}
			} else {
				if err != nil || r != nil || current.Status != "retired" {
					t.Fatal(current, r, err)
				}
			}
		})
	}
}

func TestOwnerConcurrentCoordinatorsOneWrite(t *testing.T) {
	c, f, seam := integrationFixture(t)
	ctx := context.Background()
	var file string
	c.store.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&file)
	other, err := OpenStore(filepath.Clean(file))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.clock = c.store.clock
	var wg sync.WaitGroup
	for _, v := range []*Coordinator{c, NewCoordinator(f.adapter, other)} {
		wg.Add(1)
		go func(v *Coordinator) { defer wg.Done(); v.Execute(ctx, "integrate", 1107, 0) }(v)
	}
	wg.Wait()
	if seam.posts != 1 {
		t.Fatal("concurrent duplicate/missing write", seam.posts)
	}
	if _, err = c.Execute(ctx, "integrate", 1107, 0); err != nil {
		t.Fatal("unique proof not recoverable", err)
	}
}

func TestOwnerReceiptUsesAuthenticatedHistoryPrefix(t *testing.T) {
	c, f, _ := integrationFixture(t)
	ctx := context.Background()
	if _, err := c.Execute(ctx, "integrate", 1107, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, "pump", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, "observe", 0, 0); err != nil {
		t.Fatal(err)
	}
	parent := sha(102)
	// Retain more immutable merges than fit in a root-to-tip API scan. Each
	// trusted observation adds one suffix; no arbitrary state/history is injected.
	for n := 0; n < 30; n++ {
		next := sha(1000 + n)
		f.mu.Lock()
		f.commits[next] = Commit{SHA: next, Parent: parent, IntegratedAt: clock.Add(time.Minute + time.Duration(n+2)*time.Second)}
		f.head = next
		f.mu.Unlock()
		if _, err := c.Execute(ctx, "observe", 0, 0); err != nil {
			t.Fatal(n, err)
		}
		parent = next
	}
	f.mu.Lock()
	before := f.historyReads
	f.mu.Unlock()
	out, err := c.Execute(ctx, "integrate", 1107, 0)
	if err != nil || out.Route == nil || !out.Route.Integrated {
		t.Fatal("covered prefix rescan stranded receipt", out, err)
	}
	f.mu.Lock()
	reads := f.historyReads - before
	f.mu.Unlock()
	if reads != 0 {
		t.Fatal("already authenticated history fetched again", reads)
	}
}

func TestOwnerPreparedGrantReplacementPreservesAudit(t *testing.T) {
	c, f, _ := integrationFixture(t)
	ctx := context.Background()
	snap, old, control := prepareOwnerIntent(t, c, f)
	g := f.adapter.root.Routes[1107]
	g.ConsentID++
	if err := c.store.reserve(ctx, snap.Version, 1107, g, control.selection, f.adapter.root.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	w := old.Write
	w.Grant = grantDigest(g)
	replacement, err := c.store.prepareIntegration(ctx, snap, w, g, control.selection, f.adapter.root.ExpiresAt)
	if err != nil || replacement.ID == old.ID {
		t.Fatal(replacement, err)
	}
	var status string
	if err = c.store.db.QueryRow("SELECT status FROM integration_intent WHERE id=?", old.ID).Scan(&status); err != nil || status != "retired" {
		t.Fatal("old audit lost", status, err)
	}
	if ok, err := c.store.claimIntegration(ctx, snap.Version, old, control.selection, f.adapter.root.ExpiresAt); ok || err == nil {
		t.Fatal("retired grant sent", ok, err)
	}
}

func TestOwnerPostBudgetExhaustionRemainsDefinitelyUnsent(t *testing.T) {
	c, f, seam := integrationFixture(t)
	ctx := context.Background()
	setBudget := func(n int) {
		f.adapter.root.MaxRequests = n
		snap, err := c.store.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		o, err := f.adapter.Observe(ctx, snap)
		if err != nil {
			t.Fatal(err)
		}
		if err = c.store.SwitchControl(ctx, snap.Version, o); err != nil {
			t.Fatal(err)
		}
		g := f.adapter.root.Routes[1107]
		g.Feedback.Authority = f.adapter.authorityDigest()
		f.adapter.root.Routes[1107] = g
		f.mu.Lock()
		f.run.Title = sweepTitle(g.Feedback)
		f.mu.Unlock()
	}
	// Eleven control reads + two four-read write preflights consume 19.
	// The final POST itself is charged before claim, not after uncertainty.
	setBudget(19)
	if _, err := c.Execute(ctx, "integrate", 1107, 0); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatal("budget boundary not exercised", err)
	}
	i, err := integrationIntentAt(ctx, c.store.db, 1107)
	if err != nil || i.Status != "prepared" || seam.posts != 0 {
		t.Fatal("definitely-unsent failure stranded intent", i, err, seam.posts)
	}
	setBudget(64)
	out, err := c.Execute(ctx, "integrate", 1107, 0)
	if err != nil || out.Route == nil || !out.Route.Integrated || seam.posts != 1 {
		t.Fatal("authorized larger-budget transition unrecoverable", out, err, seam.posts)
	}
}

func TestOwnerNonUTCGrantExpiryUsesInstantIdentity(t *testing.T) {
	c, f, seam := integrationFixture(t)
	g := f.adapter.root.Routes[1107]
	g.ExpiresAt = g.ExpiresAt.In(time.FixedZone("JST", 9*60*60))
	f.adapter.root.Routes[1107] = g
	f.adapter.root.ExpiresAt = f.adapter.root.ExpiresAt.In(time.FixedZone("JST", 9*60*60))
	f.consent.(map[string]any)["body"] = consentText(f.root.RepositoryID, 1107, g)
	out, err := c.Execute(context.Background(), "integrate", 1107, 0)
	if err != nil || out.Route == nil || !out.Route.Integrated || seam.posts != 1 {
		t.Fatal("equivalent expiry timezone blocked first write", out, err, seam.posts)
	}
}
