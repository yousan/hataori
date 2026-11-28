package cipolicy

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Coordinator is invoked only by the trusted host binary. No candidate process
// receives its authority directory, database handle or dispatcher credential.
type Coordinator struct {
	adapter *Adapter
	store   *Store
}

func NewCoordinator(a *Adapter, s *Store) *Coordinator { return &Coordinator{a, s} }

type Outcome struct {
	Candidate      *CandidateReport `json:",omitempty"`
	Operation      string
	Revision       int64
	Policy         Report
	Decision       *Decision    `json:",omitempty"`
	Route          *RouteResult `json:",omitempty"`
	State          *State       `json:",omitempty"`
	Intent         *Intent      `json:",omitempty"`
	Summary        *RunSummary  `json:",omitempty"`
	RoutingEnabled bool
}

func (c *Coordinator) Execute(ctx context.Context, operation string, pull int, run int64) (Outcome, error) {
	return c.execute(ctx, operation, pull, run, "")
}

// Recover authenticates the responsible account and records a diagnosis through
// the existing CAS. It preserves debt and prepares proof; it never dispatches it.
func (c *Coordinator) Recover(ctx context.Context, reason string) (Outcome, error) {
	return c.execute(ctx, "recover", 0, 0, reason)
}

func (c *Coordinator) execute(ctx context.Context, operation string, pull int, run int64, reason string) (Outcome, error) {
	if strings.HasPrefix(operation, "candidate-") {
		return c.candidateOperation(ctx, operation, pull, reason)
	}
	if operation == "integrate" {
		return c.integrate(ctx, pull)
	}
	out := Outcome{Operation: operation, RoutingEnabled: c.adapter.root.RoutingEnabled}
	if operation == "summary" {
		v, err := c.adapter.Summarize(ctx, run)
		out.Summary = &v
		return out, err
	}
	if operation == "bootstrap-prepare" || operation == "bootstrap-pump" {
		var i Intent
		var err error
		if operation == "bootstrap-prepare" {
			i, err = c.store.PrepareBootstrap(ctx, c.adapter)
		} else {
			i, err = c.store.PumpBootstrap(ctx, c.adapter)
		}
		out.Intent = &i
		return out, err
	}
	if operation == "bootstrap" {
		if err := c.store.bootstrap(ctx, c.adapter); err != nil {
			return out, err
		}
	}
	if operation == "pump" {
		if err := c.store.Pump(ctx, c.adapter); err != nil {
			return out, err
		}
	}
	snap, err := c.store.Read(ctx)
	if err != nil {
		return out, err
	}
	switch operation {
	case "bootstrap", "pump", "status":
		// Status is a fresh authority/expiry check, never a cached admission permit.
		control, err := c.adapter.control(c.adapter.budget(ctx))
		if err != nil {
			return out, err
		}
		out.Policy = control.selection.Report(c.store.clock())
		if control.authority != snap.State.Authority {
			return out, fmt.Errorf("control transition required")
		}
		if out.Policy.FullOnly {
			return out, fmt.Errorf("policy closed")
		}
	case "observe", "admission", "done", "control-transition", "recover":
		observation, err := c.adapter.Observe(ctx, snap)
		if err != nil {
			return out, err
		}
		if operation == "recover" {
			observation, err = c.adapter.Diagnose(c.adapter.budget(ctx), observation, reason)
			if err != nil {
				return out, err
			}
			observation.event.Force = true
		}
		out.Policy = observation.control.selection.Report(c.store.clock())
		if operation == "control-transition" {
			err = c.store.SwitchControl(ctx, snap.Version, observation)
		} else {
			var d Decision
			d, err = c.store.Advance(ctx, snap.Version, observation)
			out.Decision = &d
		}
		if err != nil {
			return out, err
		}
		snap, err = c.store.Read(ctx)
		if err != nil {
			return out, err
		}
		if err = c.reconcileReservation(c.adapter.budget(ctx), snap); err != nil {
			return out, err
		}
		if operation == "admission" || operation == "done" {
			r, err := c.adapter.route(ctx, snap, pull, operation == "done", out.Decision)
			if err == nil && r.Allowed && operation == "admission" {
				err = c.store.reserve(ctx, snap.Version, pull, c.adapter.root.Routes[pull], observation.control.selection, observation.expires)
				if err != nil {
					r.Allowed = false
					r.Reason = "closed: integration reservation unresolved"
				}
			}
			if err == nil && operation == "done" && r.DoneAllowed {
				var p pullRecord
				err = c.adapter.request(c.adapter.budget(ctx), "GET", fmt.Sprintf("%s/pulls/%d", c.adapter.prefix(), pull), nil, &p)
				if err == nil {
					err = c.store.integrationReceipt(ctx, pull, p.MergeSHA)
				}
				if err != nil {
					r.Allowed = false
					r.DoneAllowed = false
					r.Integrated = false
					r.Reason = "closed: admitted integration write receipt missing"
				}
			}
			out.Route = &r
			if err != nil {
				return out, err
			}
		}
	default:
		return out, fmt.Errorf("unknown operation")
	}
	out.Revision = snap.Version
	out.State = &snap.State
	out.Intent = snap.Intent
	// Ambiguity remains visible even on status; no reader calls it healthy.
	var writes int
	if err := c.store.db.QueryRowContext(ctx, "SELECT count(*) FROM integration_intent WHERE status='uncertain'").Scan(&writes); err != nil {
		return out, err
	}
	if writes != 0 {
		return out, ErrAmbiguous
	}
	if snap.Intent != nil && snap.Intent.Status == "uncertain" {
		return out, ErrAmbiguous
	}
	return out, nil
}

// LoadCoordinator accepts a dedicated private directory outside Git checkouts.
// The independently signed root pins the executable digest as well as controls.
// It never prints paths, payloads or credential contents in error messages.
func LoadCoordinator(dir, binary string) (*Coordinator, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("authority directory required")
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("private authority directory required")
	}
	// Refuse symlinked ancestors and candidate Git checkouts, including .git files.
	for p := abs; ; p = filepath.Dir(p) {
		i, e := os.Lstat(p)
		if e != nil || i.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe authority ancestor")
		}
		stat, ok := i.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) || (i.Mode().Perm()&0022 != 0 && !(i.Mode()&os.ModeSticky != 0 && stat.Uid == 0)) {
			return nil, fmt.Errorf("replaceable authority ancestor")
		}

		if _, e := os.Lstat(filepath.Join(p, ".git")); e == nil || !os.IsNotExist(e) {
			return nil, fmt.Errorf("authority must be outside Git checkouts")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	read := func(name string, secret bool) ([]byte, error) {
		f, e := os.OpenFile(filepath.Join(abs, name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if e != nil {
			return nil, fmt.Errorf("authority file unavailable")
		}
		defer f.Close()
		i, e := f.Stat()
		if e != nil || !i.Mode().IsRegular() || i.Size() > 1<<20 {
			return nil, fmt.Errorf("invalid authority file")
		}
		stat, ok := i.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) || stat.Nlink != 1 || i.Mode().Perm()&0022 != 0 || (secret && i.Mode().Perm()&0077 != 0) {
			return nil, fmt.Errorf("untrusted authority file")
		}
		b, e := io.ReadAll(io.LimitReader(f, 1<<20+1))
		if e != nil || len(b) > 1<<20 {
			return nil, fmt.Errorf("authority file unavailable")
		}
		return b, nil
	}
	pub, err := read("authority.pub", false)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(pub)))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid authority key")
	}
	root, err := read("root.json", false)
	if err != nil {
		return nil, err
	}
	sig, err := read("root.sig", false)
	if err != nil {
		return nil, err
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return nil, fmt.Errorf("invalid authority signature")
	}
	credential, err := read("dispatcher.token", true)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(credential))
	if !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(token) {
		return nil, fmt.Errorf("invalid dispatcher credential")
	}
	a, err := NewAdapter(key, root, signature, token)
	if err != nil {
		return nil, fmt.Errorf("authority verification failed")
	}
	executable, err := os.ReadFile(binary)
	if err != nil || !digestOK(a.root.CoordinatorDigest) || Digest(executable) != a.root.CoordinatorDigest {
		return nil, fmt.Errorf("unapproved coordinator executable")
	}
	if !time.Now().Before(a.root.ExpiresAt) {
		return nil, fmt.Errorf("authority expired")
	}
	store, err := OpenStore(filepath.Join(abs, "coverage.sqlite"))
	if err != nil {
		return nil, fmt.Errorf("private store unavailable")
	}
	return NewCoordinator(a, store), nil
}
func (c *Coordinator) Close() error { return c.store.Close() }

// RunCLI has no daemon, scheduler, signing, key generation, merge or deployment
// command. Every operation is explicit and defaults to closed on failure.
func RunCLI(ctx context.Context, args []string, out io.Writer, binary string) int {
	if len(args) == 3 && args[0] == "bundle" {
		executable, e := os.ReadFile(binary)
		if e != nil {
			writeCLI(out, Outcome{}, errors.New("binary unavailable"))
			return 1
		}
		b, e := Bundle(args[1], args[2], Digest(executable))
		if e != nil {
			writeCLI(out, Outcome{}, e)
			return 1
		}
		json.NewEncoder(out).Encode(b)
		return 0
	}
	if len(args) != 2 && len(args) != 4 {
		writeCLI(out, Outcome{}, errors.New("usage: ci-coordinator OP AUTHORITY_DIR [--pull N|--run N|--reason DIAGNOSIS]"))
		return 2
	}
	op, dir := args[0], args[1]
	pull := 0
	var run int64
	reason := ""
	if len(args) == 4 && args[2] == "--reason" {
		if (op != "recover" && op != "bootstrap-recover" && op != "candidate-invalidate") || strings.TrimSpace(args[3]) == "" || len(args[3]) > 4096 {
			writeCLI(out, Outcome{}, errors.New("recovery diagnosis required"))
			return 2
		}
		reason = args[3]
	} else if len(args) == 4 {
		n, e := strconv.ParseInt(args[3], 10, 64)
		if e != nil || n <= 0 {
			writeCLI(out, Outcome{}, errors.New("positive identity required"))
			return 2
		}
		switch args[2] {
		case "--pull":
			pull = int(n)
		case "--run":
			run = n
		default:
			writeCLI(out, Outcome{}, errors.New("unknown option"))
			return 2
		}
	}
	allowed := map[string]bool{"bootstrap": true, "bootstrap-prepare": true, "bootstrap-pump": true, "observe": true, "admission": true, "integrate": true, "pump": true, "status": true, "control-transition": true, "done": true, "summary": true, "recover": true, "bootstrap-recover": true}
	for _, name := range []string{"candidate-freeze", "candidate-verify", "candidate-main", "candidate-status", "candidate-invalidate"} {
		allowed[name] = true
	}
	if strings.HasPrefix(op, "candidate-") && ((op != "candidate-invalidate" && pull == 0) || (op == "candidate-invalidate" && reason == "")) {
		writeCLI(out, Outcome{}, errors.New("candidate identity or diagnosis required"))
		return 2
	}
	if !allowed[op] || ((op == "admission" || op == "done" || op == "integrate") && pull == 0) || (op == "summary" && run == 0) || ((op == "recover" || op == "bootstrap-recover") && reason == "") {
		writeCLI(out, Outcome{}, errors.New("invalid operation arguments"))
		return 2
	}
	c, err := LoadCoordinator(dir, binary)
	if err != nil {
		writeCLI(out, Outcome{}, err)
		return 1
	}
	defer c.Close()
	var result Outcome
	if op == "bootstrap-recover" {
		result = Outcome{Operation: op, RoutingEnabled: c.adapter.root.RoutingEnabled}
		var i Intent
		i, err = c.store.RecoverBootstrap(ctx, c.adapter, reason)
		result.Intent = &i
	} else if op == "recover" {
		result, err = c.Recover(ctx, reason)
	} else if op == "candidate-invalidate" {
		result, err = c.execute(ctx, op, 0, 0, reason)
	} else {
		result, err = c.Execute(ctx, op, pull, run)
	}
	writeCLI(out, result, err)
	if err != nil {
		return 1
	}
	if result.Candidate != nil && !result.Candidate.PromotionAllowed {
		return 1 // Evidence collected, but the unproved promotion gate stays closed.
	}
	if result.State != nil && result.State.Failed {
		return 1
	}
	if result.Route != nil && !result.Route.Allowed {
		return 1
	}
	if result.Decision != nil && !result.Decision.AllowCode && op == "admission" {
		return 1
	}
	return 0
}
func writeCLI(w io.Writer, out Outcome, err error) {
	// Internal errors may contain API details. The CLI deliberately exposes only
	// a closed reason category; no response body, token, file path or raw error.
	reason := ""
	if err != nil {
		reason = "closed: verification or operation failed"
		if errors.Is(err, ErrConflict) {
			reason = "closed: concurrent state changed"
		}
		if errors.Is(err, ErrAmbiguous) {
			reason = "closed: dispatch ambiguous; reconcile without retry"
		}
	}
	json.NewEncoder(w).Encode(struct {
		Outcome Outcome
		Closed  bool
		Reason  string
	}{out, err != nil || (out.State != nil && out.State.Failed) || (out.Route != nil && !out.Route.Allowed) || (out.Candidate != nil && !out.Candidate.PromotionAllowed) || out.Policy.FullOnly, reason})
}
