package cipolicy

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Direct integration writes are opt-in authority configuration, never a PR
// label/input. Enabling requires the operator's provider/rules compatibility
// proof; this code does not establish that proof or activate the contract.
type IntegrationWriteContract struct {
	Enabled bool
	Writer  Identity
}

type integrationWrite struct {
	Pull                                                           int
	Base, Head, Merge, Source, Ref, Grant, Control, Authority, Key string
}

var branchName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func safeBranch(s string) bool {
	if !branchName.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, "//") || strings.HasSuffix(s, "/") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// pktLines accepts a single complete flush-terminated packet sequence. Unknown
// delimiters, truncation and trailing data are errors, never partial success.
func pktLines(raw []byte) ([][]byte, error) {
	var lines [][]byte
	for len(raw) >= 4 {
		n, err := strconv.ParseUint(string(raw[:4]), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid git packet")
		}
		raw = raw[4:]
		if n == 0 {
			if len(raw) != 0 {
				return nil, fmt.Errorf("trailing git data")
			}
			return lines, nil
		}
		if n < 4 || n > 65520 || int(n)-4 > len(raw) {
			return nil, fmt.Errorf("truncated git packet")
		}
		lines = append(lines, raw[:int(n)-4])
		raw = raw[int(n)-4:]
	}
	return nil, fmt.Errorf("missing git flush")
}
func gitPacket(s string) []byte { return append([]byte(fmt.Sprintf("%04x", len(s)+4)), []byte(s)...) }

func atomicCommands(w integrationWrite) []byte {
	var out []byte
	out = append(out, gitPacket(fmt.Sprintf("%s %s refs/heads/%s\x00report-status atomic\n", w.Base, w.Merge, w.Ref))...)
	// Ordinary git push omits this no-op. The raw command is essential: the
	// receiver must validate AND lock the candidate's expected old object too.
	out = append(out, gitPacket(fmt.Sprintf("%s %s refs/heads/%s\n", w.Head, w.Head, w.Source))...)
	out = append(out, []byte("0000")...)
	pack := []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0}
	sum := sha1.Sum(pack) // Git's SHA-1 object-format empty pack, not an authority hash.
	out = append(out, pack...)
	return append(out, sum[:]...)
}
func advertisedAtomic(raw []byte, w integrationWrite) error {
	header := gitPacket("# service=git-receive-pack\n")
	header = append(header, []byte("0000")...)
	if !bytes.HasPrefix(raw, header) {
		return fmt.Errorf("unexpected git service")
	}
	lines, err := pktLines(raw[len(header):])
	if err != nil || len(lines) == 0 {
		return fmt.Errorf("incomplete git advertisement")
	}
	refs := map[string]string{}
	caps := map[string]bool{}
	for i, line := range lines {
		fields := strings.SplitN(strings.TrimSuffix(string(line), "\n"), "\x00", 2)
		if i == 0 {
			if len(fields) != 2 {
				return fmt.Errorf("missing git capabilities")
			}
			for _, c := range strings.Fields(fields[1]) {
				caps[c] = true
			}
		} else if len(fields) != 1 {
			return fmt.Errorf("unexpected git capabilities")
		}
		ref := strings.Fields(fields[0])
		if len(ref) != 2 || len(ref[0]) != 40 || !revisionOK(ref[0]) {
			return fmt.Errorf("invalid git ref advertisement")
		}
		if _, ok := refs[ref[1]]; ok {
			return fmt.Errorf("duplicate advertised ref")
		}
		refs[ref[1]] = ref[0]
	}
	if !caps["atomic"] || !caps["report-status"] {
		return fmt.Errorf("atomic git transaction unsupported")
	}
	for c := range caps {
		if strings.HasPrefix(c, "object-format=") && c != "object-format=sha1" {
			return fmt.Errorf("unsupported git object format")
		}
	}
	if refs["refs/heads/"+w.Ref] != w.Base || refs["refs/heads/"+w.Source] != w.Head {
		return fmt.Errorf("base or candidate moved")
	}
	return nil
}
func atomicReport(raw []byte, w integrationWrite) error {
	lines, err := pktLines(raw)
	if err != nil || len(lines) != 3 {
		return fmt.Errorf("incomplete atomic git report")
	}
	expected := []string{"unpack ok\n", "ok refs/heads/" + w.Ref + "\n", "ok refs/heads/" + w.Source + "\n"}
	for i, line := range lines {
		if string(line) != expected[i] {
			return fmt.Errorf("atomic git update rejected or unknown")
		}
	}
	return nil
}

// gitHTTP has no URL/credential overrides in the public root or CLI. Test code
// substitutes the private transport base only. Never follow redirects or expose
// provider response bodies (which may echo private ref names or credentials).
func (a *Adapter) gitRequest(ctx context.Context, method, suffix string, body []byte) (*http.Request, error) {
	if b, ok := ctx.Value(budgetKey{}).(*apiBudget); ok && b.used.Add(1) > b.max {
		return nil, fmt.Errorf("trusted API request budget exhausted")
	}
	base := a.gitBase
	if base == "" {
		base = "https://github.com"
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/"+a.root.Repository+".git/"+suffix, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("git request unavailable")
	}
	req.SetBasicAuth("x-access-token", a.token)
	want := "application/x-git-receive-pack-advertisement"
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		want = "application/x-git-receive-pack-result"
	}
	req.Header.Set("Accept", want)
	return req, nil
}
func (a *Adapter) gitSend(req *http.Request) ([]byte, error) {
	client := *a.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("git transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != req.Header.Get("Accept") {
		return nil, fmt.Errorf("git transport contract unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return nil, fmt.Errorf("git response incomplete")
	}
	return raw, nil
}
func (a *Adapter) gitHTTP(ctx context.Context, method, suffix string, body []byte) ([]byte, error) {
	req, err := a.gitRequest(ctx, method, suffix, body)
	if err != nil {
		return nil, err
	}
	return a.gitSend(req)
}

func (a *Adapter) prepareIntegration(ctx context.Context, pull int, g RouteGrant) (integrationWrite, error) {
	w := integrationWrite{Pull: pull, Base: g.Base, Head: g.Head, Ref: g.Ref, Grant: grantDigest(g), Key: a.keyDigest, Control: a.root.CurrentControl, Authority: a.authorityDigest()}
	contract := a.root.IntegrationWrite
	if !contract.Enabled || contract.Writer != a.root.Dispatcher {
		return w, fmt.Errorf("provider integration write contract inactive")
	}
	var actor Identity
	if err := a.request(ctx, "GET", "/user", nil, &actor); err != nil || actor != contract.Writer {
		return w, fmt.Errorf("integration writer identity mismatch")
	}
	var p pullRecord
	if err := a.request(ctx, "GET", fmt.Sprintf("%s/pulls/%d", a.prefix(), pull), nil, &p); err != nil {
		return w, err
	}
	w.Source = p.Head.Ref
	w.Merge = p.MergeSHA
	if p.Number != pull || p.State != "open" || p.Draft || p.Merged || p.Head.Repo.ID != a.root.RepositoryID || p.Base.Repo.ID != a.root.RepositoryID || p.Head.SHA != g.Head || p.Base.SHA != g.Base || p.Base.Ref != g.Ref || g.Ref != a.root.IntegrationRef || w.Source == w.Ref || w.Source == a.root.ControlRef || !safeBranch(w.Source) || !safeBranch(w.Ref) || len(w.Base) != 40 || len(w.Head) != 40 || len(w.Merge) != 40 || !revisionOK(w.Merge) {
		return w, fmt.Errorf("integration PR write identity changed")
	}
	var object struct {
		SHA     string
		Parents []struct{ SHA string }
		Tree    struct{ SHA string }
	}
	if err := a.request(ctx, "GET", a.prefix()+"/git/commits/"+w.Merge, nil, &object); err != nil {
		return w, err
	}
	if object.SHA != w.Merge || len(object.Parents) != 2 || object.Parents[0].SHA != w.Base || object.Parents[1].SHA != w.Head || !revisionOK(object.Tree.SHA) {
		return w, fmt.Errorf("provider merge object is not exact base/head")
	}
	raw, err := a.gitHTTP(ctx, "GET", "info/refs?service=git-receive-pack", nil)
	if err != nil {
		return w, err
	}
	return w, advertisedAtomic(raw, w)
}

func (a *Adapter) writeIntegration(prepared *http.Request, w integrationWrite) error {
	raw, err := a.gitSend(prepared)
	if err != nil {
		return err
	}
	return atomicReport(raw, w)
}
