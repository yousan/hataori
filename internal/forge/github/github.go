// Package github reads repository data through the GitHub REST API.
//
// Its complete list of request paths is:
//   - /repos/<owner>/<repo>/issues
//   - /repos/<owner>/<repo>/issues/<number>
//   - /repos/<owner>/<repo>/issues/<number>/comments
//   - /repos/<owner>/<repo>/pulls
//   - /repos/<owner>/<repo>/pulls/<number>
//   - /repos/<owner>/<repo>/pulls/<number>/reviews
//   - /repos/<owner>/<repo>/pulls/<number>/comments
//   - /repos/<owner>/<repo>/pulls/<number>/files
//   - /repos/<owner>/<repo>/commits/<ref>/check-runs
//   - /repos/<owner>/<repo>/commits/<ref>/status
//   - /repos/<owner>/<repo>/compare/<ref>...<ref>
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/forge"
)

// Name is the name the forge registers under.
const Name = "github"

const apiBase = "https://api.github.com"

// Settings is the forge's section of the instance config.
type Settings struct {
	// TokenFile names the token file in the config directory.
	TokenFile string `json:"token_file"`
}

// client reads the API using bounded requests.
type client struct {
	guard       *authTransport // the only holder of the token
	base        *url.URL       // the API origin
	http        *http.Client   // the client used for reads
	pageSize    int            // items requested per page
	listLimit   int            // issues or pull requests returned by a list
	pages       int            // maximum pages per paged read
	detailLimit int            // maximum items per detail read
}

// Format keeps all formatted client representations free of credentials.
func (c *client) Format(state fmt.State, verb rune) {
	io.WriteString(state, "github forge")
}

// authTransport sends authenticated reads only to the API origin.
type authTransport struct {
	token string
	inner http.RoundTripper
}

// RoundTrip checks the request before adding authentication to a clone.
func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("github: refusing method %q", strings.ReplaceAll(req.Method, a.token, "[token]"))
	}
	api, _ := url.Parse(apiBase)
	if req.URL.Scheme != api.Scheme || req.URL.Host != api.Host {
		return nil, errors.New("github: refusing request outside the API origin")
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+a.token)
	return a.inner.RoundTrip(clone)
}

// New is the forge's factory. It reads the token only from the config directory.
func New(config *os.Root, settings []byte) (forge.Forge, error) {
	var s Settings
	d := json.NewDecoder(bytes.NewReader(settings))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return nil, fmt.Errorf("github: settings: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("github: settings: unexpected trailing data")
	}
	name := strings.TrimSpace(s.TokenFile)
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		if strings.HasPrefix(name, prefix) {
			return nil, errors.New(`github: settings: "token_file" holds what looks like a token; give the name of a file in the config directory that holds it`)
		}
	}
	if name == "" || filepath.IsAbs(name) || !filepath.IsLocal(name) {
		return nil, fmt.Errorf("github: settings: %q: %q is not a file name inside the config directory", "token_file", s.TokenFile)
	}
	if config == nil {
		return nil, errors.New("github: no config directory")
	}
	f, err := config.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("github: read the token file %q in the config directory: %w", name, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("github: read the token file %q in the config directory: %w", name, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("github: the token file %q is not a regular file", name)
	}
	if st.Mode().Perm()&077 != 0 {
		return nil, fmt.Errorf("github: the token file %q is accessible to other users (mode %04o); run chmod 600 on it", name, st.Mode().Perm())
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("github: read the token file %q in the config directory: %w", name, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, fmt.Errorf("github: the token file %q is empty", name)
	}
	for _, c := range []byte(token) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return nil, fmt.Errorf("github: the token file %q holds a character a token cannot have", name)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	return newClient(token, transport), nil
}

// host is the host of the addresses of repositories on GitHub.
const host = "github.com"

// scpForm is git's form [user@]host:path.
var scpForm = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+@)?([A-Za-z0-9.-]+):([^/\s][^\s]*)$`)

// Serves returns the repository of an address on GitHub: https, http, ssh
// or git with the host github.com, or git's form [user@]github.com:path,
// whose path is <owner>/<name>, with or without ".git" or a trailing "/".
func (c *client) Serves(address string) (forge.Repo, bool) {
	var h, p string
	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" {
			return forge.Repo{}, false
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return forge.Repo{}, false
		}
		h, p = u.Hostname(), strings.TrimPrefix(u.Path, "/")
	} else if m := scpForm.FindStringSubmatch(address); m != nil {
		h, p = m[1], m[2]
	} else {
		return forge.Repo{}, false
	}
	if !strings.EqualFold(h, host) {
		return forge.Repo{}, false
	}
	r, err := forge.ParseRepo(strings.TrimSuffix(strings.TrimSuffix(p, "/"), ".git"))
	if err != nil {
		return forge.Repo{}, false
	}
	return r, true
}

// newClient makes a client with a testable inner transport.
func newClient(token string, transport http.RoundTripper) *client {
	u, _ := url.Parse(apiBase)
	guard := &authTransport{token: token, inner: transport}
	return &client{guard: guard, base: u, http: &http.Client{Transport: guard, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, pageSize: 100, listLimit: 100, pages: 3, detailLimit: 300}
}

// clean removes the token from an error while preserving safe errors.
func (c *client) clean(err error) error {
	if err == nil || !strings.Contains(err.Error(), c.guard.token) {
		return err
	}
	var e *forge.Error
	if errors.As(err, &e) {
		copy := *e
		copy.Message = strings.ReplaceAll(copy.Message, c.guard.token, "[token]")
		return &copy
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.guard.token, "[token]"))
}

// get sends one bounded read and decodes its answer.
func (c *client) get(ctx context.Context, path string, q url.Values, out any) (next bool, err error) {
	defer func() { err = c.clean(err) }()
	u := *c.base
	u.Path = path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "hataori")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return false, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return false, errors.New("forge answered with a redirect")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &body)
		if body.Message == "" {
			body.Message = http.StatusText(resp.StatusCode)
		}
		retry, hasRetry := resp.Header[http.CanonicalHeaderKey("Retry-After")]
		e := &forge.Error{Status: resp.StatusCode, Message: body.Message,
			RateLimited: resp.StatusCode == 429 || resp.StatusCode == 403 && (resp.Header.Get("X-RateLimit-Remaining") == "0" || hasRetry || strings.Contains(strings.ToLower(body.Message), "rate limit"))}
		if e.RateLimited {
			if hasRetry {
				value := ""
				if len(retry) > 0 {
					value = retry[0]
				}
				if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= int64((1<<63-1)/int64(time.Second)) {
					e.Reset = time.Now().Add(time.Duration(seconds) * time.Second)
				} else if date, err := http.ParseTime(value); err == nil {
					e.Reset = date
				}
			} else if seconds, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && seconds > 0 {
				e.Reset = time.Unix(seconds, 0)
			}
		}
		return false, e
	}
	if err := json.Unmarshal(b, out); err != nil {
		return false, err
	}
	return strings.Contains(resp.Header.Get("Link"), `rel="next"`), nil
}

// repoPath validates a repository and builds its API prefix.
func repoPath(r forge.Repo) (string, error) {
	if _, err := forge.ParseRepo(r.String()); err != nil {
		return "", errors.New("forge: invalid repository")
	}
	return "/repos/" + r.Owner + "/" + r.Name, nil
}

var refPart = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// refPath validates and escapes a ref segment by segment.
func refPath(ref string) (string, error) {
	if len(ref) == 0 || len(ref) > 255 {
		return "", errors.New("forge: invalid ref")
	}
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		if p == "." || p == ".." || !refPart.MatchString(p) {
			return "", errors.New("forge: invalid ref")
		}
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/"), nil
}

// numberPath builds a path to a numbered item.
func numberPath(r forge.Repo, n int, kind string) (string, error) {
	p, e := repoPath(r)
	if e != nil {
		return "", e
	}
	if n <= 0 {
		return "", errors.New("forge: invalid number")
	}
	return p + "/" + kind + "/" + strconv.Itoa(n), nil
}

// page builds a paging query.
func page(p, size int) url.Values {
	return url.Values{"page": {strconv.Itoa(p)}, "per_page": {strconv.Itoa(size)}}
}

// user holds the login in an API answer.
type user struct {
	Login string `json:"login"`
}

// textAuthor holds an author and their repository association.
type textAuthor struct {
	User        user   `json:"user"`
	Association string `json:"author_association"`
}

// author maps the association to repository write access.
func (a textAuthor) author() forge.Author {
	return forge.Author{Login: a.User.Login, Write: a.Association == "OWNER" || a.Association == "MEMBER" || a.Association == "COLLABORATOR"}
}

// issue holds the fields read from an issue answer.
type issue struct {
	textAuthor
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Body   string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []user          `json:"assignees"`
	Updated   time.Time       `json:"updated_at"`
	URL       string          `json:"html_url"`
	PR        json.RawMessage `json:"pull_request"`
}

// item maps an issue answer to the forge interface.
func (i issue) item() forge.Issue {
	x := forge.Issue{Number: i.Number, Title: i.Title, State: i.State, Author: i.author(), Updated: i.Updated, URL: i.URL}
	for _, v := range i.Labels {
		x.Labels = append(x.Labels, v.Name)
	}
	for _, v := range i.Assignees {
		x.Assignees = append(x.Assignees, v.Login)
	}
	return x
}

// pull holds the fields read from a pull request answer.
type pull struct {
	textAuthor
	Number      int                       `json:"number"`
	Title       string                    `json:"title"`
	State       string                    `json:"state"`
	Body        string                    `json:"body"`
	Draft       bool                      `json:"draft"`
	Head        struct{ Ref, SHA string } `json:"head"`
	Base        struct{ Ref string }      `json:"base"`
	Updated     time.Time                 `json:"updated_at"`
	URL         string                    `json:"html_url"`
	Merged      bool                      `json:"merged"`
	MergedAt    *time.Time                `json:"merged_at"`
	MergeCommit string                    `json:"merge_commit_sha"`
	MergeState  string                    `json:"mergeable_state"`
}

// item maps a pull request answer to the forge interface.
func (p pull) item() forge.Pull {
	state := p.State
	if p.Merged || p.MergedAt != nil {
		state = "merged"
	}
	return forge.Pull{Number: p.Number, Title: p.Title, State: state, Draft: p.Draft, Branch: p.Head.Ref, Base: p.Base.Ref, Author: p.author(), Updated: p.Updated, URL: p.URL}
}

// comment holds the fields read from a comment answer.
type comment struct {
	textAuthor
	Body    string    `json:"body"`
	Path    string    `json:"path"`
	Created time.Time `json:"created_at"`
	URL     string    `json:"html_url"`
}

// item maps a comment answer to the forge interface.
func (x comment) item() forge.Comment {
	return forge.Comment{Author: x.author(), Body: x.Body, Path: x.Path, Created: x.Created, URL: x.URL}
}

// review holds the fields read from a review answer.
type review struct {
	textAuthor
	State     string    `json:"state"`
	Body      string    `json:"body"`
	Submitted time.Time `json:"submitted_at"`
	URL       string    `json:"html_url"`
}

// file holds the fields read from a changed-file answer.
type file struct {
	Filename  string `json:"filename"`
	Previous  string `json:"previous_filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// pages reads a bounded list, optionally filtering its items.
func pages[T any](ctx context.Context, c *client, path string, limit int, extra url.Values, keep func(T) bool) (forge.List[T], error) {
	var out forge.List[T]
	for p := 1; p <= c.pages; p++ {
		q := page(p, c.pageSize)
		for k, v := range extra {
			q[k] = v
		}
		var items []T
		next, e := c.get(ctx, path, q, &items)
		if e != nil {
			return out, e
		}
		for i, v := range items {
			if keep != nil && !keep(v) {
				continue
			}
			if len(out.Items) == limit {
				out.More = true
				return out, nil
			}
			out.Items = append(out.Items, v)
			if len(out.Items) == limit {
				out.More = i < len(items)-1 || next
				return out, nil
			}
		}
		if len(items) == 0 || !next {
			return out, nil
		}
		if p == c.pages {
			out.More = true
		}
	}
	return out, nil
}

// Issues lists repository issues, excluding pull requests.
func (c *client) Issues(ctx context.Context, r forge.Repo, s forge.State) (forge.List[forge.Issue], error) {
	var out forge.List[forge.Issue]
	p, e := repoPath(r)
	if e != nil {
		return out, e
	}
	if s != forge.Open && s != forge.Closed && s != forge.All {
		return out, errors.New("forge: invalid state")
	}
	a, e := pages(ctx, c, p+"/issues", c.listLimit, url.Values{"state": {string(s)}, "sort": {"updated"}, "direction": {"desc"}}, func(i issue) bool { return len(i.PR) == 0 })
	out.More = a.More
	for _, v := range a.Items {
		out.Items = append(out.Items, v.item())
	}
	return out, e
}

// Pulls lists repository pull requests.
func (c *client) Pulls(ctx context.Context, r forge.Repo, s forge.State) (forge.List[forge.Pull], error) {
	var out forge.List[forge.Pull]
	p, e := repoPath(r)
	if e != nil {
		return out, e
	}
	if s != forge.Open && s != forge.Closed && s != forge.All {
		return out, errors.New("forge: invalid state")
	}
	a, e := pages[pull](ctx, c, p+"/pulls", c.listLimit, url.Values{"state": {string(s)}, "sort": {"updated"}, "direction": {"desc"}}, nil)
	out.More = a.More
	for _, v := range a.Items {
		out.Items = append(out.Items, v.item())
	}
	return out, e
}

// comments reads a bounded list of conversation or review comments.
func (c *client) comments(ctx context.Context, p string) (forge.List[forge.Comment], error) {
	var out forge.List[forge.Comment]
	a, e := pages[comment](ctx, c, p, c.detailLimit, nil, nil)
	out.More = a.More
	for _, v := range a.Items {
		out.Items = append(out.Items, v.item())
	}
	return out, e
}

// Issue reads an issue and its comments.
func (c *client) Issue(ctx context.Context, r forge.Repo, n int) (forge.IssueDetail, error) {
	var out forge.IssueDetail
	p, e := numberPath(r, n, "issues")
	if e != nil {
		return out, e
	}
	var i issue
	if _, e = c.get(ctx, p, nil, &i); e != nil {
		return out, e
	}
	out.Issue = i.item()
	out.Body = i.Body
	out.Comments, e = c.comments(ctx, p+"/comments")
	return out, e
}

// Pull reads a pull request, its discussion, files and head checks.
func (c *client) Pull(ctx context.Context, r forge.Repo, n int) (forge.PullDetail, error) {
	var out forge.PullDetail
	p, e := numberPath(r, n, "pulls")
	if e != nil {
		return out, e
	}
	var x pull
	if _, e = c.get(ctx, p, nil, &x); e != nil {
		return out, e
	}
	out.Pull = x.item()
	out.Body = x.Body
	out.Head = x.Head.SHA
	out.Merged = x.Merged || x.MergedAt != nil
	out.MergeState = x.MergeState
	if out.Merged {
		out.MergeCommit = x.MergeCommit
	}
	reviews, e := pages[review](ctx, c, p+"/reviews", c.detailLimit, nil, nil)
	if e != nil {
		return out, e
	}
	out.Reviews.More = reviews.More
	for _, v := range reviews.Items {
		out.Reviews.Items = append(out.Reviews.Items, forge.Review{Author: v.author(), State: v.State, Body: v.Body, Submitted: v.Submitted, URL: v.URL})
	}
	ip, _ := numberPath(r, n, "issues")
	if out.Comments, e = c.comments(ctx, ip+"/comments"); e != nil {
		return out, e
	}
	if out.ReviewComments, e = c.comments(ctx, p+"/comments"); e != nil {
		return out, e
	}
	files, e := pages[file](ctx, c, p+"/files", c.detailLimit, nil, nil)
	if e != nil {
		return out, e
	}
	out.Files.More = files.More
	for _, v := range files.Items {
		out.Files.Items = append(out.Files.Items, forge.File{Path: v.Filename, Previous: v.Previous, Status: v.Status, Additions: v.Additions, Deletions: v.Deletions})
	}
	out.Checks, e = c.Checks(ctx, r, x.Head.SHA)
	return out, e
}

// Merge reads a pull request's merge state, from its one request.
func (c *client) Merge(ctx context.Context, r forge.Repo, n int) (forge.Merge, error) {
	p, e := numberPath(r, n, "pulls")
	if e != nil {
		return forge.Merge{}, e
	}
	var x pull
	if _, e = c.get(ctx, p, nil, &x); e != nil {
		return forge.Merge{}, e
	}
	out := forge.Merge{State: x.item().State, Merged: x.Merged || x.MergedAt != nil}
	if out.Merged {
		out.Commit = x.MergeCommit
	}
	return out, nil
}

// comparison holds the fields read from a comparison of two commits.
type comparison struct {
	Status    string `json:"status"`
	AheadBy   int    `json:"ahead_by"`
	BehindBy  int    `json:"behind_by"`
	MergeBase struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
}

// Compare compares base and head, asking for one commit of the list,
// which it does not read.
func (c *client) Compare(ctx context.Context, r forge.Repo, base, head string) (forge.Comparison, error) {
	p, e := repoPath(r)
	if e != nil {
		return forge.Comparison{}, e
	}
	b, e := refPath(base)
	if e != nil {
		return forge.Comparison{}, e
	}
	h, e := refPath(head)
	if e != nil {
		return forge.Comparison{}, e
	}
	var x comparison
	if _, e = c.get(ctx, p+"/compare/"+b+"..."+h, url.Values{"per_page": {"1"}}, &x); e != nil {
		return forge.Comparison{}, e
	}
	return forge.Comparison{Status: x.Status, AheadBy: x.AheadBy, BehindBy: x.BehindBy, MergeBase: x.MergeBase.SHA}, nil
}

// checkRun holds the fields read from a check run.
type checkRun struct {
	Name, Status, Conclusion string
	URL                      string `json:"html_url"`
	Head                     string `json:"head_sha"`
}

// status holds the fields read from a commit status.
type status struct {
	Context, State string
	URL            string `json:"target_url"`
}

// Checks reads check runs and commit statuses for a ref.
func (c *client) Checks(ctx context.Context, r forge.Repo, ref string) (forge.Checks, error) {
	var out forge.Checks
	p, e := repoPath(r)
	if e != nil {
		return out, e
	}
	ref, e = refPath(ref)
	if e != nil {
		return out, e
	}
	p += "/commits/" + ref
	for n := 1; n <= c.pages; n++ {
		var a struct {
			Total int        `json:"total_count"`
			Runs  []checkRun `json:"check_runs"`
		}
		next, e := c.get(ctx, p+"/check-runs", page(n, c.pageSize), &a)
		if e != nil {
			return out, e
		}
		for _, v := range a.Runs {
			if len(out.Items.Items) >= c.detailLimit {
				out.Items.More = true
				break
			}
			out.Commit = v.Head
			out.Items.Items = append(out.Items.Items, forge.Check{Kind: forge.CheckRun, Name: v.Name, State: v.Status, Conclusion: v.Conclusion, URL: v.URL})
		}
		if len(out.Items.Items) >= c.detailLimit || n == c.pages {
			out.Items.More = out.Items.More || next || a.Total > len(out.Items.Items)
			break
		}
		if len(a.Runs) == 0 || !next {
			break
		}
	}
	var s struct {
		SHA, State string
		Statuses   []status `json:"statuses"`
	}
	// Commit statuses take one page of at most 100.
	next, e := c.get(ctx, p+"/status", page(1, c.pageSize), &s)
	if e != nil {
		return out, e
	}
	if out.Commit == "" {
		out.Commit = s.SHA
	}
	if len(s.Statuses) > 0 {
		out.State = s.State
	}
	for _, v := range s.Statuses {
		out.Items.Items = append(out.Items.Items, forge.Check{Kind: forge.CommitStatus, Name: v.Context, State: v.State, URL: v.URL})
	}
	out.Items.More = out.Items.More || next
	return out, nil
}
