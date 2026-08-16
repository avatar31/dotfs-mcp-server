// Package store wraps the embedded BadgerDB cache that persists parsed
// multi-language AST metadata between server reboots.
//
// Key layout:
//
//	func:<function_name>          -> JSON model.FunctionRecord
//	idx:<repo_name>:<function_name> -> empty (repository ownership index)
//
// The primary key gives the LLM an O(1) lookup, while the secondary index makes
// per-repository enumeration and pruning possible during a re-index cycle.
package store

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dgraph-io/badger/v4"
)

const (
	funcPrefix = "func:"
	idxPrefix  = "idx:"
)

// ErrNotFound is returned when a function key is absent from the cache.
var ErrNotFound = errors.New("function not found in cache")

// Store is a concurrency-safe handle around BadgerDB.
type Store struct {
	db  *badger.DB
	log *slog.Logger
}

// RepoStat aggregates what the cache knows about a single repository.
type RepoStat struct {
	RepoName  string         `json:"repo_name"`
	Functions int            `json:"function_count"`
	Languages map[string]int `json:"languages"`
	Files     map[string]int `json:"-"`
	Samples   []string       `json:"sample_functions"`
}

// Open initialises (or re-opens) the on-disk cache at dir.
func Open(dir string, log *slog.Logger) (*Store, error) {
	opts := badger.DefaultOptions(dir).
		WithLogger(badgerLogger{log: log.With("component", "badger")}).
		WithCompactL0OnClose(true)

	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open badger cache at %q: %w", dir, err)
	}
	return &Store{db: db, log: log}, nil
}

// Close flushes and releases the cache. Always call it during shutdown.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close badger cache: %w", err)
	}
	return nil
}

// RunValueLogGC reclaims space in the value log. Safe to call periodically.
func (s *Store) RunValueLogGC(discardRatio float64) error {
	err := s.db.RunValueLogGC(discardRatio)
	if err != nil && errors.Is(err, badger.ErrNoRewrite) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("badger value log gc: %w", err)
	}
	return nil
}

// FunctionKey builds the primary cache key for a function name.
func FunctionKey(name string) []byte { return []byte(funcPrefix + name) }

func repoIndexKey(repo, name string) []byte {
	return []byte(idxPrefix + repo + ":" + name)
}

func repoIndexPrefix(repo string) []byte {
	return []byte(idxPrefix + repo + ":")
}

// badgerLogger adapts BadgerDB's logging interface onto slog.
//
// This matters for correctness, not just tidiness: the MCP transport owns
// stdout, so every internal log line must be routed to the structured logger
// (stderr) instead.
type badgerLogger struct{ log *slog.Logger }

func (l badgerLogger) Errorf(f string, a ...any) {
	l.log.Error(strings.TrimSpace(fmt.Sprintf(f, a...)))
}
func (l badgerLogger) Warningf(f string, a ...any) {
	l.log.Warn(strings.TrimSpace(fmt.Sprintf(f, a...)))
}

// Infof is intentionally demoted to debug: BadgerDB is chatty at info level
// and the operator log must stay focused on indexing and transport events.
func (l badgerLogger) Infof(f string, a ...any) { l.log.Debug(strings.TrimSpace(fmt.Sprintf(f, a...))) }
func (l badgerLogger) Debugf(f string, a ...any) {
	l.log.Debug(strings.TrimSpace(fmt.Sprintf(f, a...)))
}
