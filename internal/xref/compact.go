package xref

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avatar31/dotfs-mcp-server/internal/lsp"
)

// externalRepo labels a hit that lives outside the indexed workspace.
const externalRepo = "(external)"

// sourceCache lazily loads the files touched while compacting one answer, so a
// hot file with twenty references is read exactly once.
type sourceCache struct {
	files map[string][]string
}

func newSourceCache() *sourceCache {
	return &sourceCache{files: make(map[string][]string)}
}

// lines returns the split contents of path, or nil when it cannot be read.
func (c *sourceCache) lines(path string) []string {
	if cached, ok := c.files[path]; ok {
		return cached
	}
	var split []string
	if data, err := os.ReadFile(path); err == nil {
		split = strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	}
	c.files[path] = split
	return split
}

// line returns a single one-based source line, trimmed and length-capped.
func (c *sourceCache) line(path string, oneBased int) string {
	all := c.lines(path)
	if oneBased < 1 || oneBased > len(all) {
		return ""
	}
	return truncate(strings.TrimSpace(all[oneBased-1]))
}

// identifierAt extracts the identifier surrounding a one-based coordinate. The
// language server never echoes the symbol name back for a references request,
// so the name in the compact payload is recovered from the source itself.
func (c *sourceCache) identifierAt(path string, line, character int) string {
	all := c.lines(path)
	if line < 1 || line > len(all) {
		return ""
	}
	text := []rune(all[line-1])
	idx := character - 1
	if idx < 0 || idx > len(text) {
		return ""
	}
	// A caret sitting immediately after the identifier still resolves it.
	if idx == len(text) || !isIdentRune(text[idx]) {
		if idx == 0 || !isIdentRune(text[idx-1]) {
			return ""
		}
		idx--
	}

	start := idx
	for start > 0 && isIdentRune(text[start-1]) {
		start--
	}
	end := idx
	for end < len(text)-1 && isIdentRune(text[end+1]) {
		end++
	}
	return string(text[start : end+1])
}

// locate maps an absolute path back onto a workspace repository. Results that
// fall outside the workspace (standard library, module cache, system headers)
// are reported under a synthetic repository with an elided path so no absolute
// host path is ever handed to the model.
func (s *Service) locate(abs string) (repo, rel string) {
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return externalRepo, elide(abs)
	}

	rel = filepath.ToSlash(rel)
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) != 2 {
		return externalRepo, elide(abs)
	}
	return parts[0], parts[1]
}

// reference converts one protocol location into a compacted hit.
func (s *Service) reference(uri lsp.DocumentURI, pos lsp.Position, src *sourceCache) (Reference, bool) {
	abs, err := uri.Path()
	if err != nil {
		s.log.Debug("skipping unusable lsp uri", "uri", string(uri), "error", err)
		return Reference{}, false
	}

	repo, rel := s.locate(abs)
	line := pos.Line + 1
	return Reference{
		Repo:      repo,
		FilePath:  rel,
		Line:      line,
		Character: pos.Character + 1,
		Snippet:   src.line(abs, line),
	}, true
}

// compactLocations applies the full guardrail pipeline: deduplicate by file and
// line, cap at the configured limit and attach one line of evidence each. It
// returns the kept hits plus the pre-truncation total.
func (s *Service) compactLocations(locs []lsp.Location, src *sourceCache, named bool) ([]Reference, int) {
	seen := make(map[string]struct{}, len(locs))
	out := make([]Reference, 0, min(len(locs), s.limit))
	total := 0

	for _, loc := range locs {
		ref, ok := s.reference(loc.URI, loc.Range.Start, src)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s:%d", ref.FilePath, ref.Line)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		total++

		if len(out) >= s.limit {
			continue
		}
		if named {
			if abs, err := loc.URI.Path(); err == nil {
				ref.Name = src.identifierAt(abs, ref.Line, ref.Character)
			}
		}
		out = append(out, ref)
	}
	return out, total
}

func isIdentRune(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

// truncate caps a snippet so one pathological line cannot dominate the answer.
func truncate(s string) string {
	if len(s) <= maxSnippetChars {
		return s
	}
	return s[:maxSnippetChars] + " ..."
}

// elide keeps the last three path segments of an out-of-workspace file.
func elide(abs string) string {
	parts := strings.Split(filepath.ToSlash(abs), "/")
	if len(parts) <= 3 {
		return strings.TrimPrefix(filepath.ToSlash(abs), "/")
	}
	return ".../" + strings.Join(parts[len(parts)-3:], "/")
}

// decodeLocations normalises the three shapes a server may use for definition,
// typeDefinition and implementation results: an array of Location, an array of
// LocationLink, or a single Location object.
func decodeLocations(raw json.RawMessage) ([]lsp.Location, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}

	if trimmed[0] == '[' {
		var locations []lsp.Location
		if err := json.Unmarshal(trimmed, &locations); err == nil && hasURI(locations) {
			return locations, nil
		}
		var links []lsp.LocationLink
		if err := json.Unmarshal(trimmed, &links); err != nil {
			return nil, fmt.Errorf("xref: decode location array: %w", err)
		}
		converted := make([]lsp.Location, 0, len(links))
		for _, link := range links {
			if link.TargetURI == "" {
				continue
			}
			converted = append(converted, lsp.Location{URI: link.TargetURI, Range: link.TargetSelectionRange})
		}
		return converted, nil
	}

	var single lsp.Location
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return nil, fmt.Errorf("xref: decode location: %w", err)
	}
	if single.URI == "" {
		return nil, nil
	}
	return []lsp.Location{single}, nil
}

// hasURI reports whether a decode produced real locations rather than the empty
// husks left behind when LocationLink JSON is forced into a Location.
func hasURI(locations []lsp.Location) bool {
	for _, loc := range locations {
		if loc.URI != "" {
			return true
		}
	}
	return false
}
