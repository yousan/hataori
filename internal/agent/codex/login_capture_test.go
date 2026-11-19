package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginCaptureRefuses(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	own := filepath.Join(home, ".codex")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{own, bin} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(own, "config.toml"), []byte("config"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "ran")
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(own, link); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(root, "auth")
	fresh := filepath.Join(root, "fresh")
	for _, dir := range []string{auth, fresh} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(auth, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, config string
		set, exists  bool
	}{
		{"unset", "", false, false},
		{"empty", "", true, false},
		{"own", own, true, false},
		{"symlink", link, true, false},
		{"auth", auth, true, false},
		{"output exists", fresh, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(root, "out-"+tc.name)
			if tc.exists {
				if err := os.Mkdir(out, 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(python, "testdata/login_capture.py", out)
			cmd.Dir = "."
			cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + home}
			if tc.set {
				cmd.Env = append(cmd.Env, "CODEX_HOME="+tc.config)
			}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			err := cmd.Run()
			if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 2 {
				t.Errorf("%s: exit = %v, stderr = %q", tc.name, err, stderr.String())
			}
			if s := stderr.String(); !strings.HasPrefix(s, "login_capture.py:") || !strings.HasSuffix(s, "\n") || strings.Count(s, "\n") != 1 {
				t.Errorf("%s: expected one stderr line, got %q", tc.name, s)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("%s: codex ran or marker check failed: %v", tc.name, err)
			}
			if _, err := os.Stat(out); (err == nil) != tc.exists {
				t.Errorf("%s: output existence: %v", tc.name, err)
			}
		})
	}
}
