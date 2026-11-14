package check

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Limits that keep the worker inside its memory budget. A bundle past any of
// them is reported as malformed.
const (
	maxFiles      = 256
	maxFileBytes  = 512 << 10
	maxTotalBytes = 4 << 20
)

const (
	manifestName = "shakedown.json"
	licenseName  = "LICENSE"
)

// ErrUnreadable is returned when the folder itself cannot be opened. It says
// nothing about where the folder is.
var ErrUnreadable = errors.New("the folder could not be read")

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// knownExt lists the file types the suite has rules for.
var knownExt = map[string]bool{".html": true, ".js": true, ".css": true, ".json": true, ".txt": true, ".png": true}

type bundle struct {
	exists map[string]bool   // every regular file with an acceptable name
	data   map[string][]byte // contents of the files that were read
	paths  []string          // sorted keys of data
}

func (b *bundle) withExt(ext string) []string {
	var out []string
	for _, p := range b.paths {
		if path.Ext(p) == ext {
			out = append(out, p)
		}
	}
	return out
}

func (c *checker) load(dir string) (*bundle, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ErrUnreadable
	}
	defer root.Close()
	fsys := root.FS()

	b := &bundle{exists: map[string]bool{}, data: map[string][]byte{}}
	total := int64(0)
	count := 0
	walkErr := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if !nameRE.MatchString(d.Name()) {
			c.add(wholeBundle, "BUNDLE-FILE-NAME", EParse, "", "a file or folder name uses characters outside letters, digits, dot, dash and underscore")
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			c.add(wholeBundle, "BUNDLE-FILE-KIND", EParse, p, "not a regular file; links and special files are not allowed")
			return nil
		}
		count++
		if count > maxFiles {
			c.add(wholeBundle, "BUNDLE-SIZE", EParse, "", "the bundle has more files than the limit allows")
			return fs.SkipAll
		}
		b.exists[p] = true
		if p != licenseName && !knownExt[path.Ext(p)] {
			c.add(wholeBundle, "BUNDLE-FILE-TYPE", ENeedsSubtool, p, "no rule covers this type of file")
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFileBytes {
			c.add(wholeBundle, "BUNDLE-SIZE", EParse, p, "the file is larger than the limit allows")
			return nil
		}
		total += info.Size()
		if total > maxTotalBytes {
			c.add(wholeBundle, "BUNDLE-SIZE", EParse, "", "the bundle is larger than the limit allows")
			return fs.SkipAll
		}
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
		if err != nil {
			return err
		}
		if len(data) > maxFileBytes {
			c.add(wholeBundle, "BUNDLE-SIZE", EParse, p, "the file is larger than the limit allows")
			return nil
		}
		b.data[p] = data
		return nil
	})
	if walkErr != nil {
		return nil, ErrUnreadable
	}
	for p := range b.data {
		b.paths = append(b.paths, p)
	}
	sort.Strings(b.paths)
	return b, nil
}

func validBundlePath(raw string) bool {
	if raw == "" || strings.HasPrefix(raw, "/") || strings.HasSuffix(raw, "/") || strings.Contains(raw, "\\") {
		return false
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == "" || seg == "." || seg == ".." || !nameRE.MatchString(seg) {
			return false
		}
	}
	return true
}

func (c *checker) loadMap(files map[string][]byte) *bundle {
	b := &bundle{exists: map[string]bool{}, data: map[string][]byte{}}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	total := int64(0)
	count := 0
	for _, p := range keys {
		if !validBundlePath(p) {
			c.add(wholeBundle, "BUNDLE-FILE-NAME", EParse, "", "a file or folder name uses characters outside letters, digits, dot, dash and underscore")
			continue
		}
		count++
		if count > maxFiles {
			c.add(wholeBundle, "BUNDLE-SIZE", EParse, "", "the bundle has more files than the limit allows")
			break
		}
		b.exists[p] = true
		if p != licenseName && !knownExt[path.Ext(p)] {
			c.add(wholeBundle, "BUNDLE-FILE-TYPE", ENeedsSubtool, p, "no rule covers this type of file")
			continue
		}
		raw := files[p]
		if len(raw) > maxFileBytes {
			c.add(wholeBundle, "BUNDLE-SIZE", EParse, p, "the file is larger than the limit allows")
			continue
		}
		total += int64(len(raw))
		if total > maxTotalBytes {
			c.add(wholeBundle, "BUNDLE-SIZE", EParse, "", "the bundle is larger than the limit allows")
			break
		}
		cp := make([]byte, len(raw))
		copy(cp, raw)
		b.data[p] = cp
	}
	for p := range b.data {
		b.paths = append(b.paths, p)
	}
	sort.Strings(b.paths)
	return b
}

var disallowedMarkings = []string{
	"controlled unclassified information",
	"cui//",
	"//noforn",
	"for official use only",
	"source selection information",
	"proprietary and confidential",
}

func (c *checker) checkHygiene(b *bundle) {
	for _, p := range b.paths {
		if c.done() {
			return
		}
		if path.Ext(p) == ".png" {
			continue
		}
		lower := strings.ToLower(string(b.data[p]))
		for _, mark := range disallowedMarkings {
			if strings.Contains(lower, mark) {
				c.add(wholeBundle, "BUNDLE-HYGIENE", EParse, p, "controlled or proprietary markings are not allowed")
				break
			}
		}
	}
}

// refKind says what a URL written in the bundle points at.
type refKind int

const (
	refEmpty refKind = iota
	refFragment
	refLocal
	refMissing
	refOutside
	refRemote
	refInlineCode
	refUnfamiliar
)

var schemeRE = regexp.MustCompile(`^[a-z][a-z0-9+.-]*:`)

// classifyRef resolves a URL found in the file `from`. It never guesses: any
// form it has no rule for is refUnfamiliar.
func (b *bundle) classifyRef(from, raw string) refKind {
	v := strings.Trim(raw, " \t\r\n\f")
	if v == "" {
		return refEmpty
	}
	for _, r := range v {
		if r < 0x20 || r == '\\' || r == '%' || r > 0x7e {
			return refUnfamiliar
		}
	}
	if strings.HasPrefix(v, "#") {
		return refFragment
	}
	lower := strings.ToLower(v)
	if strings.HasPrefix(lower, "//") {
		return refRemote
	}
	if scheme := schemeRE.FindString(lower); scheme != "" {
		switch scheme {
		case "http:", "https:", "ws:", "wss:", "ftp:":
			return refRemote
		case "javascript:":
			return refInlineCode
		}
		return refUnfamiliar
	}
	if strings.HasPrefix(v, "/") {
		return refUnfamiliar
	}
	if i := strings.IndexAny(v, "?#"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return refFragment
	}
	target := path.Join(path.Dir(from), v)
	if target == ".." || strings.HasPrefix(target, "../") {
		return refOutside
	}
	if b.exists[target] {
		return refLocal
	}
	return refMissing
}
