package skills

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	CatalogBytes = 8000
	ViewBytes    = 30000
)

type Page struct {
	Skills     []Metadata `json:"skills"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type View struct {
	Metadata
	Path          string `json:"path"`
	Content       string `json:"content"`
	ContentSHA256 string `json:"content_sha256,omitempty"` // full-file identity for combining instruction pages
	Start         int    `json:"start"`
	End           int    `json:"end"`
	TotalLines    int    `json:"total_lines"`
	NextStart     int    `json:"next_start,omitempty"`
	ExecutionDir  string `json:"execution_dir,omitempty"` // set by the execution adapter, not discovery
	Instructions  bool   `json:"instructions,omitempty"`  // this file is the bundle's instruction source
}

func (c *Catalog) names() []string {
	names := make([]string, 0, len(c.entries))
	for name := range c.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve applies the same precedence and ambiguity rules used by List and View.
// Visibility in automatic catalogs is independent of name resolution.
func (c *Catalog) Resolve(name string) (Metadata, error) {
	entries := c.entries[name]
	if len(entries) == 0 {
		return Metadata{}, fmt.Errorf("skill %q is unavailable (not found or invalid)", name)
	}
	if len(entries) > 1 {
		return Metadata{}, fmt.Errorf("skill %q is ambiguous in %s scope", name, entries[0].Scope)
	}
	return entries[0], nil
}

// HasSource verifies a recorded identity against discovered valid bundles,
// including ones hidden by precedence or ambiguity. It does not resolve a name
// to its current winner or trust paths supplied by persisted conversation data.
func (c *Catalog) HasSource(meta Metadata) bool {
	known, ok := c.sources[meta.Source]
	return ok && known.Name == meta.Name && known.BaseDir == meta.BaseDir
}

// List returns a stable bounded page of automatically selectable metadata.
func (c *Catalog) List(cursor string) (Page, error) {
	start := 0
	if cursor != "" {
		var err error
		start, err = strconv.Atoi(cursor)
		if err != nil || start < 0 {
			return Page{}, fmt.Errorf("invalid skills cursor %q", cursor)
		}
	}
	var visible []Metadata
	for _, name := range c.names() {
		m, err := c.Resolve(name)
		if err == nil && !m.UserOnly {
			if m, fits := catalogMetadata(m); fits {
				visible = append(visible, m)
			}
		}
	}
	if start > len(visible) {
		return Page{}, fmt.Errorf("skills cursor %q is past catalog end", cursor)
	}
	page := Page{Skills: []Metadata{}}
	for i := start; i < len(visible); i++ {
		m := visible[i]
		candidate := Page{Skills: append(append([]Metadata{}, page.Skills...), m)}
		if i+1 < len(visible) {
			candidate.NextCursor = strconv.Itoa(i + 1)
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return Page{}, err
		}
		if len(encoded) > CatalogBytes {
			if len(page.Skills) == 0 {
				return Page{}, fmt.Errorf("metadata for skill %q exceeds catalog page limit", m.Name)
			}
			page.NextCursor = strconv.Itoa(i)
			break
		}
		page = candidate
	}
	return page, nil
}

// Account for the page wrapper and a maximum-width cursor before listing an
// entry. One unlistable identity must not prevent access to healthy entries.
func catalogMetadata(m Metadata) (Metadata, bool) {
	m.Description = abbreviate(m.Description, 1024)
	data, err := json.Marshal(Page{Skills: []Metadata{m}, NextCursor: "99999999999999999999"})
	return m, err == nil && len(data) <= CatalogBytes
}

func abbreviate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	n := limit - 3
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n] + "..."
}

// View reads current bytes. Relative resources may not escape the canonical
// bundle, even via symlinks. Line ranges are one-based and inclusive.
func (c *Catalog) View(name, file string, start, end int) (View, error) {
	m, err := c.Resolve(name)
	if err != nil {
		return View{}, err
	}
	return view(m, file, start, end)
}

func view(m Metadata, file string, start, end int) (View, error) {
	if file == "" {
		file = "SKILL.md"
	}
	f, path, err := openBundleFile(m.BaseDir, file)
	if err != nil {
		return View{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return View{}, err
	}
	// Count and validate with bounded storage before selecting a page. Exact
	// total_lines and rejection of invalid UTF-8 anywhere still require full I/O.
	digest := sha256.New()
	total, err := skillTextLines(io.TeeReader(f, digest))
	if err != nil {
		return View{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return View{}, err
	}
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > total {
		end = total
	}
	if end < start && start <= total {
		return View{}, fmt.Errorf("end must not precede start")
	}
	v := View{Metadata: m, Path: path, Start: start, End: start - 1, TotalLines: total}
	if source, err := filepath.EvalSymlinks(m.Source); err == nil {
		v.Instructions = path == source
		if v.Instructions {
			v.ContentSHA256 = fmt.Sprintf("%x", digest.Sum(nil))
		}
	}
	v.Description = abbreviate(v.Description, 1024)
	header, err := json.Marshal(v)
	if err != nil {
		return View{}, err
	}
	used := len(header) + 256 // space for changing range numbers and continuation
	if used >= ViewBytes {
		return View{}, fmt.Errorf("skill metadata exceeds view page limit")
	}
	var out strings.Builder
	reader := bufio.NewReaderSize(f, 32*1024)
	for i := 1; i <= end; i++ {
		// ReadSlice returns bounded fragments even for a huge newline-free
		// resource. Skipped lines never accumulate; selected lines cannot grow
		// beyond the output budget before JSON escaping is accounted for.
		var line []byte
		read := 0
		for {
			fragment, err := reader.ReadSlice('\n')
			read += len(fragment)
			if i >= start {
				if len(line)+len(fragment) > ViewBytes {
					return View{}, fmt.Errorf("skill resource line %d exceeds %d-byte page limit; split the line to read it", i, ViewBytes)
				}
				line = append(line, fragment...)
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil && err != io.EOF {
				return View{}, err
			}
			if read == 0 {
				return View{}, fmt.Errorf("skill resource changed while reading; retry")
			}
			break
		}
		if i < start {
			continue
		}
		encoded, err := json.Marshal(string(line))
		if err != nil {
			return View{}, err
		}
		size := len(encoded) - 2 // exclude the surrounding JSON quotes
		if size+len(header)+256 > ViewBytes {
			return View{}, fmt.Errorf("skill resource line %d exceeds %d-byte page limit; split the line to read it", i, ViewBytes)
		}
		if used+size > ViewBytes {
			v.NextStart = i
			break
		}
		used += size
		out.Write(line)
		v.End = i
	}
	if v.End < total && v.NextStart == 0 && v.End >= start {
		v.NextStart = v.End + 1
	}
	after, err := f.Stat()
	if err != nil {
		return View{}, err
	}
	if info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return View{}, fmt.Errorf("skill resource changed while reading; retry")
	}
	v.Content = out.String()
	return v, nil
}

// skillTextLines retains no resource content, including arbitrarily long lines.
// ReadRune handles UTF-8 characters split across its fixed-size input buffer.
func skillTextLines(file io.Reader) (int, error) {
	reader := bufio.NewReaderSize(file, 32*1024)
	lines := 0
	unterminated := false
	for {
		r, size, err := reader.ReadRune()
		if err == io.EOF {
			if unterminated {
				lines++
			}
			return lines, nil
		}
		if err != nil {
			return 0, err
		}
		if r == utf8.RuneError && size == 1 {
			return 0, fmt.Errorf("skill resource must be UTF-8 text")
		}
		unterminated = r != '\n'
		if !unterminated {
			lines++
		}
	}
}
