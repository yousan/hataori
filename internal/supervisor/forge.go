package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/forge"
)

// Reads gives api, a supervisor's caller in scope, the reads of a forge
// (design, section 7.1): Forge reads a repository of repos that has a
// forge name, on the forge of forges that serves its address, with that
// forge's token, and withholds the text of authors without write access
// unless trusted lists them. Any other repository is refused with
// control.ErrForbidden, and nothing is read. The daemon binds it to the
// supervisor's token, so nothing `hataori mcp` sends gets past it.
func Reads(api control.API, scope string, repos []Repo, forges map[string]forge.Forge, trusted []string, log *slog.Logger) control.API {
	return &reader{API: api, scope: scope, repos: repos, forges: forges, trusted: trusted, log: log}
}

type reader struct {
	control.API
	scope   string
	repos   []Repo
	forges  map[string]forge.Forge // by name; the token of each stays in it
	trusted []string               // logins, of any case
	log     *slog.Logger
}

// refused logs one refused call, without the repository it named, which
// the caller wrote.
func (b *reader) refused(op, reason string) {
	b.log.Warn("control call refused", "op", op, "from", ID(b.scope), "scope", b.scope, "reason", reason)
}

// entry returns the entry of the scope's repos that repo names, as listed
// or by its forge name, whose case does not matter.
func (b *reader) entry(repo string) (Repo, bool) {
	if repo == "" {
		return Repo{}, false
	}
	for _, e := range b.repos {
		if e.Address == repo {
			return e, true
		}
	}
	for _, e := range b.repos {
		if e.Forge != "" && strings.EqualFold(e.Forge, repo) {
			return e, true
		}
	}
	return Repo{}, false
}

// Forge makes one read of a forge for a repository of the scope's repos
// (Reads). The forge holds its token; nothing of it is in the answer.
func (b *reader) Forge(ctx context.Context, r control.ForgeRequest) (control.ForgeAnswer, error) {
	e, ok := b.entry(r.Repo)
	if !ok {
		b.refused("Forge", "the repository is not one of the scope's supervisor.repos")
		return control.ForgeAnswer{}, fmt.Errorf("the supervisor of scope %q reads a forge only for a repository its scope lists under supervisor.repos, "+
			"by the address listed there or by its forge name, and this is not one: %w", b.scope, control.ErrForbidden)
	}
	f, repo, ok := b.forgeOf(e)
	if !ok {
		b.refused("Forge", "the repository has no forge name, or no forge of the instance serves it")
		return control.ForgeAnswer{}, fmt.Errorf("the supervisor of scope %q cannot read this repository on a forge: "+
			"its address has no forge name, or no forge of this instance (forges) serves it: %w", b.scope, control.ErrForbidden)
	}
	state := forge.Open
	switch r.State {
	case "", control.ForgeOpen:
	case control.ForgeClosed:
		state = forge.Closed
	case control.ForgeAll:
		state = forge.All
	default:
		return control.ForgeAnswer{}, fmt.Errorf("forge: state %q is none of open, closed and all", r.State)
	}
	if (r.Read == control.ForgeReadIssue || r.Read == control.ForgeReadPull) && r.Number <= 0 {
		return control.ForgeAnswer{}, errors.New("forge: a read of one issue or pull request needs its number")
	}
	if r.Read == control.ForgeReadChecks && strings.TrimSpace(r.Ref) == "" {
		return control.ForgeAnswer{}, errors.New("forge: a read of checks needs a branch, a tag or a commit")
	}
	w := withholder{trusted: b.trusted}
	out := control.ForgeAnswer{Repo: repo.String()}
	switch r.Read {
	case control.ForgeReadIssues:
		l, err := f.Issues(ctx, repo, state)
		if err != nil {
			return control.ForgeAnswer{}, forgeErr(err)
		}
		out.Issues = &control.ForgeIssueList{Items: []control.ForgeIssue{}, More: l.More}
		for _, i := range l.Items {
			out.Issues.Items = append(out.Issues.Items, w.issue(i))
		}
	case control.ForgeReadIssue:
		d, err := f.Issue(ctx, repo, r.Number)
		if err != nil {
			return control.ForgeAnswer{}, forgeErr(err)
		}
		i := w.issue(d.Issue)
		detail := control.ForgeIssueDetail{ForgeIssue: i, Comments: w.comments(d.Comments, false)}
		if !i.Withheld {
			detail.Body = d.Body
		}
		out.Issue = &detail
	case control.ForgeReadPulls:
		l, err := f.Pulls(ctx, repo, state)
		if err != nil {
			return control.ForgeAnswer{}, forgeErr(err)
		}
		out.Pulls = &control.ForgePullList{Items: []control.ForgePull{}, More: l.More}
		for _, p := range l.Items {
			out.Pulls.Items = append(out.Pulls.Items, w.pull(p))
		}
	case control.ForgeReadPull:
		d, err := f.Pull(ctx, repo, r.Number)
		if err != nil {
			return control.ForgeAnswer{}, forgeErr(err)
		}
		p := w.pull(d.Pull)
		// In a withheld pull request, its author wrote the paths of its
		// files, and its workflows may name the checks of its head.
		detail := control.ForgePullDetail{ForgePull: p, Head: d.Head, Merged: d.Merged, MergeCommit: d.MergeCommit, MergeState: d.MergeState,
			Reviews:  control.ForgeReviewList{Items: []control.ForgeReview{}, More: d.Reviews.More},
			Comments: w.comments(d.Comments, false), ReviewComments: w.comments(d.ReviewComments, p.Withheld),
			Files:  control.ForgeFileList{Items: []control.ForgeFile{}, More: d.Files.More},
			Checks: checks(d.Checks, p.Withheld)}
		if !p.Withheld {
			detail.Body = d.Body
		}
		for _, v := range d.Reviews.Items {
			detail.Reviews.Items = append(detail.Reviews.Items, w.review(v))
		}
		for _, x := range d.Files.Items {
			f := control.ForgeFile{Status: x.Status, Additions: x.Additions, Deletions: x.Deletions, Withheld: p.Withheld}
			if !p.Withheld {
				f.Path, f.Previous = x.Path, x.Previous
			}
			detail.Files.Items = append(detail.Files.Items, f)
		}
		out.Pull = &detail
	case control.ForgeReadChecks:
		c, err := f.Checks(ctx, repo, r.Ref)
		if err != nil {
			return control.ForgeAnswer{}, forgeErr(err)
		}
		cs := checks(c, !namedRef(r.Ref))
		out.Checks = &cs
	default:
		return control.ForgeAnswer{}, fmt.Errorf("forge: no read %q: one of issues, issue, pulls, pull and checks", r.Read)
	}
	return out, nil
}

// hexRef is a ref that may be a commit or an abbreviation of one: 4 to 40
// hexadecimal digits, as git takes them.
var hexRef = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)

// namedRef reports whether ref names a branch or a tag of the repository,
// whose commits someone with write access put there: not a commit, which
// may be one a stranger's pull request made, and not a pull request's ref
// (pull/<number>/head, refs/pull/...). The checks of any other ref have
// their names withheld, since the workflows of whoever wrote its commit
// may name them.
func namedRef(ref string) bool {
	if hexRef.MatchString(ref) {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.EqualFold(part, "pull") {
			return false
		}
	}
	return true
}

// forgeOf returns the forge that serves e's address, and the repository
// there, trying the forges by name: none for an entry with no forge name.
func (b *reader) forgeOf(e Repo) (forge.Forge, forge.Repo, bool) {
	if e.Forge == "" {
		return nil, forge.Repo{}, false
	}
	names := make([]string, 0, len(b.forges))
	for n := range b.forges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if r, ok := b.forges[n].Serves(e.Address); ok {
			return b.forges[n], r, true
		}
	}
	return nil, forge.Repo{}, false
}

// forgeErr is a forge's error as the caller reads it: a refusal or a rate
// limit with the forge's own reason.
func forgeErr(err error) error {
	var e *forge.Error
	if errors.As(err, &e) {
		return fmt.Errorf("the forge refused the read: %w", e)
	}
	return fmt.Errorf("the forge could not be read: %w", err)
}

// withholder leaves out the text of authors without write access whom
// the scope does not trust (design, section 7.1).
type withholder struct{ trusted []string }

// shows reports whether a's text may be given.
func (w withholder) shows(a forge.Author) bool {
	if a.Write {
		return true
	}
	for _, t := range w.trusted {
		if strings.EqualFold(t, a.Login) {
			return true
		}
	}
	return false
}

func author(a forge.Author) control.ForgeAuthor {
	return control.ForgeAuthor{Login: a.Login, Write: a.Write}
}

func (w withholder) issue(i forge.Issue) control.ForgeIssue {
	out := control.ForgeIssue{Number: i.Number, State: i.State, Author: author(i.Author), Labels: i.Labels,
		Assignees: i.Assignees, Updated: i.Updated, URL: i.URL}
	if w.shows(i.Author) {
		out.Title = i.Title
	} else {
		out.Withheld = true
	}
	return out
}

func (w withholder) pull(p forge.Pull) control.ForgePull {
	out := control.ForgePull{Number: p.Number, State: p.State, Draft: p.Draft, Base: p.Base, Author: author(p.Author),
		Updated: p.Updated, URL: p.URL}
	if w.shows(p.Author) {
		out.Title, out.Branch = p.Title, p.Branch
	} else {
		out.Withheld = true
	}
	return out
}

// comments are l, each comment's text withheld by its own author, and,
// with noPaths, the files they are on left out.
func (w withholder) comments(l forge.List[forge.Comment], noPaths bool) control.ForgeCommentList {
	out := control.ForgeCommentList{Items: []control.ForgeComment{}, More: l.More}
	for _, c := range l.Items {
		x := control.ForgeComment{Author: author(c.Author), Created: c.Created, URL: c.URL}
		if !noPaths {
			x.Path = c.Path
		}
		if w.shows(c.Author) {
			x.Body = c.Body
		} else {
			x.Withheld = true
		}
		out.Items = append(out.Items, x)
	}
	return out
}

func (w withholder) review(v forge.Review) control.ForgeReview {
	out := control.ForgeReview{Author: author(v.Author), State: v.State, Submitted: v.Submitted, URL: v.URL}
	if w.shows(v.Author) {
		out.Body = v.Body
	} else {
		out.Withheld = true
	}
	return out
}

// checks are c, with the checks' names and addresses left out when
// noNames: a commit status's address is whatever its poster gave.
func checks(c forge.Checks, noNames bool) control.ForgeChecks {
	out := control.ForgeChecks{Commit: c.Commit, State: c.State, Items: []control.ForgeCheck{}, More: c.Items.More}
	for _, x := range c.Items.Items {
		k := control.ForgeCheck{Kind: x.Kind, State: x.State, Conclusion: x.Conclusion, Withheld: noNames}
		if !noNames {
			k.Name, k.URL = x.Name, x.URL
		}
		out.Items = append(out.Items, k)
	}
	return out
}
