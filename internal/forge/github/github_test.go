package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/forge"
)

type capture struct {
	Request string            `json:"request"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}
type fake struct {
	captures map[string]capture
	requests []*http.Request
	override func(*http.Request) (*http.Response, error)
}

func (f *fake) RoundTrip(r *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, r.Clone(r.Context()))
	if f.override != nil {
		return f.override(r)
	}
	c, ok := f.captures[r.Method+" "+r.URL.RequestURI()]
	if !ok {
		c = capture{Status: 404, Body: json.RawMessage(`{"message":"Not Found"}`)}
	}
	h := http.Header{}
	for k, v := range c.Headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: c.Status, Header: h, Body: io.NopCloser(bytes.NewReader(c.Body)), Request: r}, nil
}
func captureFile(t *testing.T, n string) capture {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("testdata", n+".json"))
	if e != nil {
		t.Fatal(e)
	}
	var c capture
	if e = json.Unmarshal(b, &c); e != nil {
		t.Fatal(e)
	}
	return c
}
func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{captures: map[string]capture{}}
	paths, e := filepath.Glob("testdata/*.json")
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range paths {
		n := strings.TrimSuffix(filepath.Base(p), ".json")
		if n == "rate-limited" {
			continue
		}
		c := captureFile(t, n)
		f.captures[c.Request] = c
	}
	return f
}
func TestReadsDecodeCaptures(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c := newClient("marker", "https://api.github.com", f)
	urfave := forge.Repo{Owner: "urfave", Name: "cli"}
	cli := forge.Repo{Owner: "cli", Name: "cli"}
	c.pageSize, c.pages = 5, 2
	issues, err := c.Issues(ctx, urfave, forge.All)
	if err != nil || len(issues.Items) != 1 || !issues.More {
		t.Fatalf("issues: %+v %v", issues, err)
	}
	issue := issues.Items[0]
	if issue.Number != 2555 || issue.Title != "Serialized map restore retains old keys and ignores JSON decode errors" || issue.State != "open" || issue.Author != (forge.Author{Login: "anishpentyala"}) {
		t.Fatalf("issue list item: %+v", issue)
	}
	c.pages = 1
	pulls, err := c.Pulls(ctx, urfave, forge.All)
	if err != nil || len(pulls.Items) != 5 || !pulls.More {
		t.Fatalf("pulls: %+v %v", pulls, err)
	}
	first := pulls.Items[0]
	if first.Number != 2543 || first.State != "closed" || first.Draft || first.Branch != "fix/positional-command-name-flags" || first.Base != "main" || first.Author != (forge.Author{Login: "yxshee"}) {
		t.Fatalf("pull list item: %+v", first)
	}
	c.pageSize, c.pages = 100, 3
	detail, err := c.Issue(ctx, cli, 15104)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Number != 15104 || detail.Title != "`gh event`: let the host answer, instead of refusing every host but github.com" || detail.State != "closed" || detail.Author != (forge.Author{Login: "mihar-22"}) || !slices.Equal(detail.Labels, []string{"enhancement", "gh-event"}) || detail.Updated.Format(time.RFC3339) != "2026-11-20T14:22:54Z" || detail.URL != "https://github.com/cli/cli/issues/15104" {
		t.Fatalf("issue: %+v", detail.Issue)
	}
	wantAuthors := []forge.Author{{Login: "cli-triage[bot]"}, {Login: "github-actions[bot]"}, {Login: "babakks", Write: true}}
	if len(detail.Comments.Items) != len(wantAuthors) {
		t.Fatalf("comments: %+v", detail.Comments)
	}
	for j, want := range wantAuthors {
		if detail.Comments.Items[j].Author != want {
			t.Errorf("comment %d author: %+v", j, detail.Comments.Items[j].Author)
		}
	}
	p, err := c.Pull(ctx, cli, 14910)
	if err != nil {
		t.Fatal(err)
	}
	const head = "6a79004e03cf5efc85a8d27dca55937e745f8b20"
	if p.Title != "chore: regenerate triage workflows with gh-aw v1.0.1" || p.State != "merged" || !p.Merged || p.Branch != "tidy-dev-merge-blocker" || p.Base != "trunk" || p.Head != head || p.MergeCommit != "c4513d446bf98cfcbe72ed83f17cd3220174d88f" || p.MergeState != "unknown" || p.Author != (forge.Author{Login: "tidy-dev"}) || p.Updated.Format(time.RFC3339) != "2026-11-03T00:25:45Z" {
		t.Fatalf("pull: %+v", p.Pull)
	}
	reviews := []struct {
		author forge.Author
		state  string
	}{{forge.Author{Login: "BagToad", Write: true}, "APPROVED"}, {forge.Author{Login: "BagToad", Write: true}, "APPROVED"}, {forge.Author{Login: "copilot-pull-request-reviewer[bot]"}, "COMMENTED"}}
	if len(p.Reviews.Items) != len(reviews) {
		t.Fatalf("reviews: %+v", p.Reviews)
	}
	for j, want := range reviews {
		got := p.Reviews.Items[j]
		if got.Author != want.author || got.State != want.state {
			t.Errorf("review %d: %+v", j, got)
		}
	}
	if len(p.Comments.Items) != 0 || len(p.ReviewComments.Items) != 1 {
		t.Fatalf("pull comments: %+v %+v", p.Comments, p.ReviewComments)
	}
	rc := p.ReviewComments.Items[0]
	if rc.Author != (forge.Author{Login: "Copilot"}) || rc.Path != ".github/aw/actions-lock.json" {
		t.Fatalf("review comment: %+v", rc)
	}
	paths := []string{".github/aw/actions-lock.json", ".github/workflows/agentics-maintenance.yml", ".github/workflows/copilot-setup-steps.yml", ".github/workflows/dependabot-triage.lock.yml", ".github/workflows/issue-triage.lock.yml"}
	if len(p.Files.Items) != len(paths) {
		t.Fatalf("files: %+v", p.Files)
	}
	for j, path := range paths {
		if p.Files.Items[j].Path != path {
			t.Errorf("file %d: %+v", j, p.Files.Items[j])
		}
	}
	if p.Files.Items[0].Additions != 6 || p.Files.Items[0].Deletions != 6 {
		t.Fatalf("file changes: %+v", p.Files.Items[0])
	}
	if p.Checks.Commit != head || p.Checks.State != "" || len(p.Checks.Items.Items) != 10 {
		t.Fatalf("head checks: %+v", p.Checks)
	}
	check := p.Checks.Items.Items[0]
	if check.Kind != forge.CheckRun || check.Name != "CodeQL" || check.State != "completed" || check.Conclusion != "success" {
		t.Fatalf("head check: %+v", check)
	}
	const sha = "d5c06cb04479eeacbbb9378ea26261b6be516320"
	checks, err := c.Checks(ctx, urfave, sha)
	if err != nil || checks.Commit != sha || checks.State != "success" || len(checks.Items.Items) != 19 {
		t.Fatalf("checks: %+v %v", checks, err)
	}
	names := []string{"Analyze (actions)", "Analyze (go)", "Dependabot", "Dependabot", "Dependabot", "publish", "update-pip-graph", "Analyze (actions)", "Analyze (go)", "test-docs", "macos-15 @ Go stable", "golangci-lint", "windows-2025 @ Go oldstable", "windows-2025 @ Go stable", "ubuntu-24.04 @ Go stable", "macos-15 @ Go oldstable", "ubuntu-24.04 @ Go oldstable"}
	for j, name := range names {
		if got := checks.Items.Items[j]; got.Kind != forge.CheckRun || got.Name != name {
			t.Errorf("check %d: %+v", j, got)
		}
	}
	check = checks.Items.Items[0]
	if check.State != "completed" || check.Conclusion != "success" {
		t.Fatalf("first check: %+v", check)
	}
	for j, name := range []string{"codecov/project", "codecov/patch"} {
		got := checks.Items.Items[17+j]
		if got.Kind != forge.CommitStatus || got.Name != name || got.State != "success" || got.URL != "https://app.codecov.io/gh/urfave/cli/commit/"+sha {
			t.Errorf("status %d: %+v", j, got)
		}
	}
}

func TestOnlyGETToTheAPIHost(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c := newClient("marker", "https://api.github.com", f)
	urfave := forge.Repo{Owner: "urfave", Name: "cli"}
	cli := forge.Repo{Owner: "cli", Name: "cli"}
	c.pageSize, c.pages, c.listLimit = 5, 2, 5
	if _, err := c.Issues(ctx, urfave, forge.All); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pulls(ctx, urfave, forge.All); err != nil {
		t.Fatal(err)
	}
	c.pageSize, c.pages = 100, 3
	if _, err := c.Issue(ctx, cli, 15104); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pull(ctx, cli, 14910); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Checks(ctx, urfave, "d5c06cb04479eeacbbb9378ea26261b6be516320"); err != nil {
		t.Fatal(err)
	}
	_, notFound := c.Issue(ctx, cli, 999999999)
	var refusal *forge.Error
	if !errors.As(notFound, &refusal) || refusal.Status != 404 || refusal.RateLimited || !refusal.Reset.IsZero() || refusal.Message != "Not Found" {
		t.Fatalf("refusal: %v", notFound)
	}
	if _, err := c.Checks(ctx, cli, "unknown-ref"); err == nil {
		t.Fatal("unknown ref succeeded")
	}
	rate := captureFile(t, "rate-limited")
	f.captures[rate.Request] = rate
	_, err := c.Issue(ctx, cli, 15104)
	var fe *forge.Error
	if !errors.As(err, &fe) || !fe.RateLimited || fe.Status != 403 {
		t.Fatalf("rate: %v", err)
	}
	var rateBody struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rate.Body, &rateBody); err != nil {
		t.Fatal(err)
	}
	seconds, err := strconv.ParseInt(rate.Headers["x-ratelimit-reset"], 10, 64)
	if err != nil || !fe.Reset.Equal(time.Unix(seconds, 0)) || fe.Message != rateBody.Message {
		t.Fatalf("rate fields: %+v", fe)
	}
	const prefix = `^/repos/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+`
	patterns := []string{`/issues$`, `/issues/[0-9]+$`, `/issues/[0-9]+/comments$`, `/pulls$`, `/pulls/[0-9]+$`, `/pulls/[0-9]+/reviews$`, `/pulls/[0-9]+/comments$`, `/pulls/[0-9]+/files$`, `/commits/[A-Za-z0-9._/-]+/check-runs$`, `/commits/[A-Za-z0-9._/-]+/status$`}
	paths := make([]*regexp.Regexp, len(patterns))
	counts := make([]int, len(patterns))
	for j, pattern := range patterns {
		paths[j] = regexp.MustCompile(prefix + pattern)
	}
	for _, q := range f.requests {
		if q.Method != http.MethodGet || q.URL.Scheme != "https" || q.URL.Host != "api.github.com" || q.Header.Get("Authorization") == "" {
			t.Fatalf("unexpected request: %v", q)
		}
		matched := false
		for j, path := range paths {
			if path.MatchString(q.URL.Path) {
				counts[j]++
				matched = true
			}
		}
		if !matched {
			t.Fatalf("unexpected path: %s", q.URL.Path)
		}
	}
	for j, count := range counts {
		if count == 0 {
			t.Errorf("no request matched %s", patterns[j])
		}
	}
	f.override = func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 301, Header: http.Header{"Location": {"https://elsewhere.example/x"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	before := len(f.requests)
	_, err = c.Issue(ctx, cli, 15104)
	if err == nil || !strings.Contains(err.Error(), "redirect") || len(f.requests) != before+1 {
		t.Fatalf("redirect: %v", err)
	}
	if q := f.requests[before]; q.URL.Scheme != "https" || q.URL.Host != "api.github.com" {
		t.Fatalf("redirect request: %v", q)
	}
}

func TestCleanPreservesErrors(t *testing.T) {
	f := newFake(t)
	c := newClient("marker", "https://api.github.com", f)
	f.override = func(*http.Request) (*http.Response, error) { return nil, context.Canceled }
	_, err := c.Issue(context.Background(), forge.Repo{Owner: "cli", Name: "cli"}, 15104)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error lost: %v", err)
	}
	original := &forge.Error{Status: 404, Message: "Not Found"}
	if c.clean(original) != original {
		t.Fatal("safe forge error was replaced")
	}
}

func TestTokenInNoErrorOrLog(t *testing.T) {
	const token = "hataori-marker-token-f73e1c92"
	f := newFake(t)
	c := newClient(token, "https://api.github.com", f)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	r := forge.Repo{Owner: "cli", Name: "cli"}
	for mode := 0; mode < 3; mode++ {
		f.override = func(req *http.Request) (*http.Response, error) {
			if mode == 2 {
				return nil, fmt.Errorf("transport %s", req.Header.Get("Authorization"))
			}
			body := `{"message":"` + token + `"}`
			status := 401
			h := http.Header{}
			if mode == 1 {
				v := captureFile(t, "rate-limited")
				body = string(v.Body)
				status = v.Status
				for k, v := range v.Headers {
					h.Set(k, v)
				}
			}
			return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		_, e := c.Issue(context.Background(), r, 15104)
		if e == nil || strings.Contains(e.Error(), token) || strings.Contains(logs.String(), token) {
			t.Fatalf("leaked token: %v", e)
		}
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", c, c, c), token) {
		t.Fatal("printed token")
	}
}
func TestCapturesHoldNoToken(t *testing.T) {
	paths, _ := filepath.Glob("testdata/*.json")
	token := regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_`)
	ip := regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if token.Match(b) {
			t.Errorf("token in %s", p)
		}
		var c capture
		if e = json.Unmarshal(b, &c); e != nil {
			t.Fatal(e)
		}
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(c.Body, &m)
		for _, v := range ip.FindAllString(m.Message, -1) {
			if v != "192.0.2.1" {
				t.Errorf("address in %s", p)
			}
		}
	}
}
