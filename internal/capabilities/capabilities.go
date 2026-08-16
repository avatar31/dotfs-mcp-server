// Package capabilities implements the structural repository matrix backing the
// list_repo_capabilities MCP tool. Curated profiles are loaded from an optional
// JSON file and merged with facts observed in the live cache.
package capabilities

import (
	"encoding/json"
	"fmt"
	"os"
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
