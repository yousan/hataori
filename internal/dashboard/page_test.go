package dashboard

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// fetch is a GET of path without a session, keeping the body.
func fetch(t *testing.T, s *Server, path string) (*http.Response, string) {
	t.Helper()
	a := do(t, s, req{path: path})
	return a.Response, a.body
}

// references are the src and href attributes of an HTML page.
var references = regexp.MustCompile(`\s(?:src|href)\s*=\s*["']([^"']*)["']`)

// The page comes from the binary: no Node, no files on disk. Its files
// hold no data, so they are served without a session: the page reads its
// session from its own storage once it runs.
func TestThePageIsServed(t *testing.T) {
	s := start(t, Config{})
	resp, body := fetch(t, s, "/")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /: %s, %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, `<div id="root">`) {
		t.Fatalf("GET / is not the built page:\n%s", body)
	}
	refs := references.FindAllStringSubmatch(body, -1)
	if len(refs) == 0 {
		t.Fatalf("the page names no script or style:\n%s", body)
	}
	for _, m := range refs {
		ref := m[1]
		resp, _ := fetch(t, s, ref)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %s", ref, resp.Status)
		}
		if ct := resp.Header.Get("Content-Type"); (strings.HasSuffix(ref, ".js") && !strings.HasPrefix(ct, "text/javascript")) ||
			(strings.HasSuffix(ref, ".css") && !strings.HasPrefix(ct, "text/css")) {
			t.Errorf("GET %s: Content-Type %q", ref, ct)
		}
	}
}

// sameOrigin reports whether ref names a file of the page's own server: a
// path, with no scheme and no host.
func sameOrigin(ref string) bool {
	first, _, _ := strings.Cut(ref, "/")
	return !strings.HasPrefix(ref, "//") && !strings.Contains(first, ":")
}

var (
	cssURL    = regexp.MustCompile(`url\(\s*["']?([^"')]*)`)
	cssImport = regexp.MustCompile(`@import`)
	// A script that loads code from elsewhere names an absolute URL in an
	// import.
	// An inline script would need the policy to allow inline scripts.
	inlineScript = regexp.MustCompile(`<script(?:\s[^>]*)?>\s*[^<\s]`)
	jsImport     = regexp.MustCompile("(?:\\bimport\\s*\\(|\\bfrom)\\s*[\"'`](?:[a-z]+:)?//")
)

// The page loads nothing beyond its own server: every file it names is
// one of its own, and the browser is told to refuse anything else. The
// sign-in page is the same page under the same policy.
func TestThePageLoadsNothingElse(t *testing.T) {
	s := start(t, Config{})
	resp, index := fetch(t, s, "/")
	if got := resp.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("Content-Security-Policy %q, want %q", got, contentSecurityPolicy)
	}
	a := do(t, s, req{path: SignInPath})
	if got := a.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("sign-in: Content-Security-Policy %q, want %q", got, contentSecurityPolicy)
	}
	if a.body != index {
		t.Errorf("the sign-in page is not the page:\n%s", a.body)
	}
	directives := map[string]bool{}
	for _, d := range strings.Split(contentSecurityPolicy, ";") {
		f := strings.Fields(d)
		directives[strings.Join(f, " ")] = true
		if strings.Join(f, " ") == styleElements {
			continue // the one exception, for xterm.js's own <style> elements
		}
		if len(f) > 1 && f[1] != "'self'" && f[1] != "'none'" || len(f) > 2 {
			t.Errorf("directive %q allows more than the page's own server", d)
		}
	}
	if !directives["default-src 'self'"] {
		t.Errorf("Content-Security-Policy %q does not limit every kind of request to the page's own server", contentSecurityPolicy)
	}

	files := 0
	err := fs.WalkDir(page(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		b, err := fs.ReadFile(page(), p)
		if err != nil {
			return err
		}
		text := string(b)
		switch {
		case strings.HasSuffix(p, ".html"):
			for _, m := range references.FindAllStringSubmatch(text, -1) {
				if !sameOrigin(m[1]) {
					t.Errorf("%s names %q", p, m[1])
				}
			}
			if inlineScript.MatchString(text) {
				t.Errorf("%s has an inline script", p)
			}
		case strings.HasSuffix(p, ".css"):
			if cssImport.MatchString(text) {
				t.Errorf("%s imports a style sheet", p)
			}
			for _, m := range cssURL.FindAllStringSubmatch(text, -1) {
				if !sameOrigin(m[1]) {
					t.Errorf("%s names %q", p, m[1])
				}
			}
		case strings.HasSuffix(p, ".js"):
			if m := jsImport.FindString(text); m != "" {
				t.Errorf("%s imports from elsewhere: %s", p, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("the embedded page has no files")
	}
}
