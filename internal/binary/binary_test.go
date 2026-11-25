package binary

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestNames(t *testing.T) {
	dir := t.TempDir()
	bin, home, other := filepath.Join(dir, "bin"), filepath.Join(dir, "home"), filepath.Join(dir, "other")
	for _, d := range []string{bin, home, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(bin, "hataori-2")
	for _, p := range []string{file, filepath.Join(bin, "hataori-1"), filepath.Join(bin, "unrelated"), filepath.Join(other, "agent")} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range [][2]string{
		{"hataori-2", filepath.Join(bin, "hataori")},
		{"hataori-1", filepath.Join(bin, "hataori-old")},
		{"nothing", filepath.Join(bin, "dangling")},
		{filepath.Join("..", "bin", "hataori"), filepath.Join(home, "ht")},
		{bin, filepath.Join(dir, "linked")},
	} {
		if err := os.Symlink(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(file, filepath.Join(bin, "hard")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what   string
		exe    string
		others []string
		want   []string
	}{
		{"the file", file, nil, []string{"hard", "hataori", "hataori-2"}},
		{"the link", filepath.Join(bin, "hataori"), nil, []string{"hard", "hataori", "hataori-2"}},
		{"a link to the link", filepath.Join(home, "ht"), nil, []string{"hard", "hataori", "hataori-2", "ht"}},
		{"through a linked directory", filepath.Join(dir, "linked", "hataori"), nil, []string{"hard", "hataori", "hataori-2"}},
		{"started as", file, []string{filepath.Join(home, "ht")}, []string{"hard", "hataori", "hataori-2", "ht"}},
		{"started as nothing known", file, []string{"", filepath.Join(other, "agent"), filepath.Join(bin, "dangling")}, []string{"hard", "hataori", "hataori-2"}},
		{"gone", filepath.Join(bin, "gone"), nil, []string{"gone"}},
	} {
		got, err := Names(c.exe, c.others...)
		if (err != nil) != (c.what == "gone") {
			t.Errorf("%s: %v", c.what, err)
		}
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.what, got, c.want)
		}
	}
}

// Names says when names may be missing: a directory may let files in it be
// run and not be listed.
func TestNamesOfAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "hataori-2")
	if err := os.WriteFile(file, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if names, err := Names(file); err == nil || !slices.Equal(names, []string{"hataori-2"}) {
		t.Errorf("%q, %v", names, err)
	}
}
