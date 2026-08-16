// Package capabilities implements the structural repository matrix backing the
// list_repo_capabilities MCP tool. Curated profiles are loaded from an optional
// JSON file and merged with facts observed in the live cache.
package capabilities

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Profile is the curated, human-authored operational description of a service.
type Profile struct {
	Repo        string   `json:"repo"`
	Language    string   `json:"language"`
	Summary     string   `json:"summary"`
	Features    []string `json:"features,omitempty"`
	Interfaces  []string `json:"interfaces,omitempty"`
	Owners      []string `json:"owners,omitempty"`
	Criticality string   `json:"criticality,omitempty"`
}

// Observation carries the facts derived from the BadgerDB cache. It keeps this
// package decoupled from the storage layer.
type Observation struct {
	Indexed       bool
	FunctionCount int
	FileCount     int
	Languages     map[string]int
	Samples       []string
}

// Matrix maps repository names to curated profiles.
type Matrix struct {
	profiles map[string]Profile
}

// NewMatrix builds a matrix from an in-memory slice of profiles.
func NewMatrix(profiles []Profile) *Matrix {
	m := &Matrix{profiles: make(map[string]Profile, len(profiles))}
	for _, p := range profiles {
		if strings.TrimSpace(p.Repo) == "" {
			continue
		}
		m.profiles[p.Repo] = p
	}
	return m
}

// Load reads a JSON array of profiles. An empty path yields an empty matrix so
// the server can run without any curated metadata.
func Load(path string) (*Matrix, error) {
	if strings.TrimSpace(path) == "" {
		return NewMatrix(nil), nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read capability matrix %q: %w", path, err)
	}

	var profiles []Profile
	if err := json.Unmarshal(raw, &profiles); err != nil {
		return nil, fmt.Errorf("parse capability matrix %q: %w", path, err)
	}
	return NewMatrix(profiles), nil
}

// Lookup returns the curated profile for repo, when one exists.
func (m *Matrix) Lookup(repo string) (Profile, bool) {
	p, ok := m.profiles[repo]
	return p, ok
}

// Describe renders the high-level capability briefing handed to the LLM. It
// always reports the language stack, falling back to cache-derived facts when
// no curated profile is registered.
func (m *Matrix) Describe(repo string, obs Observation) string {
	var b strings.Builder

	profile, curated := m.Lookup(repo)
	fmt.Fprintf(&b, "# Repository: %s\n\n", repo)

	stack := languageStack(obs.Languages)
	switch {
	case curated && profile.Language != "":
		fmt.Fprintf(&b, "Engineering language stack: %s\n", profile.Language)
		if stack != "" && !strings.EqualFold(stack, profile.Language) {
			fmt.Fprintf(&b, "Observed in cache: %s\n", stack)
		}
	case stack != "":
		fmt.Fprintf(&b, "Engineering language stack: %s (derived from the indexed AST cache)\n", stack)
	default:
		b.WriteString("Engineering language stack: unknown (repository not indexed yet)\n")
	}

	if curated {
		if profile.Summary != "" {
			fmt.Fprintf(&b, "\n## Business responsibility\n%s\n", profile.Summary)
		}
		if len(profile.Features) > 0 {
			b.WriteString("\n## Implemented semantic features\n")
			for _, f := range profile.Features {
				fmt.Fprintf(&b, "- %s\n", f)
			}
		}
		if len(profile.Interfaces) > 0 {
			b.WriteString("\n## Interfaces and integration points\n")
			for _, i := range profile.Interfaces {
				fmt.Fprintf(&b, "- %s\n", i)
			}
		}
		if profile.Criticality != "" {
			fmt.Fprintf(&b, "\nOperational criticality: %s\n", profile.Criticality)
		}
		if len(profile.Owners) > 0 {
			fmt.Fprintf(&b, "Owning team(s): %s\n", strings.Join(profile.Owners, ", "))
		}
	} else {
		b.WriteString("\n## Business responsibility\n")
		b.WriteString("No curated profile is registered for this repository. ")
		b.WriteString("The description below is inferred exclusively from the indexed structural cache.\n")
	}

	b.WriteString("\n## Indexed structural footprint\n")
	if !obs.Indexed {
		b.WriteString("- No cached symbols. Trigger POST /api/v1/")
		b.WriteString(repo)
		b.WriteString("/update to index it.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "- Cached functions: %d\n", obs.FunctionCount)
	fmt.Fprintf(&b, "- Source files carrying cached symbols: %d\n", obs.FileCount)
	if len(obs.Samples) > 0 {
		fmt.Fprintf(&b, "- Representative entry points: %s\n", strings.Join(obs.Samples, ", "))
	}
	b.WriteString("\nUse global_codebase_search with any of the symbols above to retrieve the exact source body and documentation.\n")
	return b.String()
}

// languageStack renders "go (42 symbols), c (17 symbols)" deterministically.
func languageStack(languages map[string]int) string {
	if len(languages) == 0 {
		return ""
	}
	keys := make([]string, 0, len(languages))
	for k := range languages {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if languages[keys[i]] != languages[keys[j]] {
			return languages[keys[i]] > languages[keys[j]]
		}
		return keys[i] < keys[j]
	})

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s (%d symbols)", k, languages[k]))
	}
	return strings.Join(parts, ", ")
}
