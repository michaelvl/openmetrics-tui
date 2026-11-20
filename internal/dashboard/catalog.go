package dashboard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// nameRe is what a catalog name must look like once the extension is gone.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Catalog is the set of dashboards in one directory, keyed by file name
// without the extension. Names come only from the directory listing, so a
// lookup never turns user input into a path.
type Catalog struct {
	dir string

	mu      sync.RWMutex
	byName  map[string]*Dashboard
	names   []string // sorted
	skipped []string // files ignored for their name, as warnings
}

// LoadCatalog reads every *.yaml and *.yml file in dir (not recursive). It
// fails on the first problem of any kind: an unreadable directory, no
// dashboards, a duplicate name or a file that does not load. Files whose names
// do not fit the name rules are skipped; see Skipped.
func LoadCatalog(dir string) (*Catalog, error) {
	scan, err := scanCatalog(dir)
	if err != nil {
		return nil, err
	}
	if len(scan.failed) > 0 {
		return nil, errors.Join(scan.failed...)
	}
	if len(scan.byName) == 0 {
		return nil, fmt.Errorf("%s: no dashboards (*.yaml or *.yml)", dir)
	}
	c := &Catalog{dir: dir}
	c.set(scan)
	return c, nil
}

// Reload re-reads the directory. A file that fails to load is skipped and
// returned as a problem, and the rest stay usable. When the directory cannot
// be read, or no dashboard in it loads, the previous set is kept.
func (c *Catalog) Reload() []error {
	scan, err := scanCatalog(c.dir)
	if err != nil {
		return []error{err}
	}
	problems := scan.failed
	if len(scan.byName) == 0 {
		return append(problems, fmt.Errorf("%s: no usable dashboards, keeping the previous set", c.dir))
	}
	c.set(scan)
	return problems
}

// Names returns the dashboard names in sorted order.
func (c *Catalog) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.names...)
}

// Get returns the dashboard called name.
func (c *Catalog) Get(name string) (*Dashboard, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.byName[name]
	return d, ok
}

// Skipped lists the files ignored because their name does not fit the rules,
// one message each, as of the last load.
func (c *Catalog) Skipped() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.skipped...)
}

func (c *Catalog) set(s *scanResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byName, c.names, c.skipped = s.byName, s.names, s.skipped
}

type scanResult struct {
	byName  map[string]*Dashboard
	names   []string
	skipped []string
	failed  []error // files that did not load, and duplicate names
}

func scanCatalog(dir string) (*scanResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	s := &scanResult{byName: map[string]*Dashboard{}}
	files := map[string]string{} // name -> file, to report duplicates
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ext)
		if !nameRe.MatchString(name) {
			s.skipped = append(s.skipped, fmt.Sprintf("%s: skipped, name must match %s", filepath.Join(dir, e.Name()), nameRe))
			continue
		}
		if prev, dup := files[name]; dup {
			s.failed = append(s.failed, fmt.Errorf("%s: dashboard name %q is also used by %s", filepath.Join(dir, e.Name()), name, prev))
			continue
		}
		files[name] = e.Name()
		d, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			s.failed = append(s.failed, err)
			continue
		}
		s.byName[name] = d
	}
	s.names = make([]string, 0, len(s.byName))
	for n := range s.byName {
		s.names = append(s.names, n)
	}
	sort.Strings(s.names)
	return s, nil
}
