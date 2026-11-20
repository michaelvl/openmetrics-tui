package dashboard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const catDash = "title: %s\npanels:\n  - title: P\n    query: up\n"

func catDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func titled(title string) string { return strings.Replace(catDash, "%s", title, 1) }

func TestCatalogListingAndOrder(t *testing.T) {
	dir := catDir(t, map[string]string{
		"team-b.yaml": titled("B"), "team-a.yml": titled("A"), "c.yaml": titled("C"),
		"notes.txt": "ignored",
	})
	if err := os.Mkdir(filepath.Join(dir, "sub.yaml"), 0o700); err != nil { // a directory is not a dashboard
		t.Fatal(err)
	}
	c, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Names(), []string{"c", "team-a", "team-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("names %v, want %v", got, want)
	}
	if d, ok := c.Get("team-a"); !ok || d.Title != "A" {
		t.Errorf("Get(team-a) = %v, %v", d, ok)
	}
}

func TestCatalogInvalidFile(t *testing.T) {
	dir := catDir(t, map[string]string{"good.yaml": titled("G"), "bad.yaml": "title: X\nbogus: 1\n"})
	if _, err := LoadCatalog(dir); err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Fatalf("startup error %v, want one naming bad.yaml", err)
	}

	// A file that breaks later is skipped and reported; the rest stay usable.
	if err := os.Remove(filepath.Join(dir, "bad.yaml")); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"bad.yaml": "title: X\nbogus: 1\n", "good.yaml": "not: [valid", "new.yaml": titled("N")} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	problems := c.Reload()
	if len(problems) != 2 {
		t.Errorf("problems %v, want 2", problems)
	}
	if got, want := c.Names(), []string{"new"}; !reflect.DeepEqual(got, want) {
		t.Errorf("names after reload %v, want %v", got, want)
	}

	// Nothing usable: the previous set is kept.
	if err := os.Remove(filepath.Join(dir, "new.yaml")); err != nil {
		t.Fatal(err)
	}
	if problems := c.Reload(); len(problems) == 0 {
		t.Error("reload with nothing usable reported no problem")
	}
	if got := c.Names(); len(got) != 1 || got[0] != "new" {
		t.Errorf("names %v, want the previous set", got)
	}
}

func TestCatalogNameRules(t *testing.T) {
	dir := catDir(t, map[string]string{
		"ok.yaml": titled("OK"), "a.b_c-1.yaml": titled("X"), "0start.yml": titled("Z"),
		"Upper.yaml": titled("U"), "-dash.yaml": titled("D"), ".hidden.yaml": titled("H"), "sp ace.yaml": titled("S"),
	})
	c, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Names(), []string{"0start", "a.b_c-1", "ok"}; !reflect.DeepEqual(got, want) {
		t.Errorf("names %v, want %v", got, want)
	}
	if got := len(c.Skipped()); got != 4 {
		t.Errorf("skipped %d files, want 4: %v", got, c.Skipped())
	}
}

func TestCatalogDuplicateName(t *testing.T) {
	dir := catDir(t, map[string]string{"a.yaml": titled("1"), "a.yml": titled("2")})
	if _, err := LoadCatalog(dir); err == nil || !strings.Contains(err.Error(), "also used by") {
		t.Errorf("error %v, want a duplicate name error", err)
	}
}

func TestCatalogLookupNeverTouchesPaths(t *testing.T) {
	dir := catDir(t, map[string]string{"a.yaml": titled("A")})
	outside := filepath.Join(filepath.Dir(dir), "x.yaml")
	if err := os.WriteFile(outside, []byte(titled("X")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	c, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../x", "../x.yaml", outside, filepath.Join(dir, "a.yaml"), "a.yaml", ""} {
		if d, ok := c.Get(name); ok {
			t.Errorf("Get(%q) found %v", name, d)
		}
	}
}

func TestCatalogEmptyDirectory(t *testing.T) {
	if _, err := LoadCatalog(t.TempDir()); err == nil {
		t.Error("empty directory loaded")
	}
	if _, err := LoadCatalog(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing directory loaded")
	}
}
