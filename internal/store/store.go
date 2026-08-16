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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dgraph-io/badger/v4"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
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
// func:<function_name> is the primary key for a function record.
func FunctionKey(name string) []byte { return []byte(funcPrefix + name) }

// idx:<repo_name>:<function_name> is the secondary index key for a function name.
func repoIndexKey(repo, name string) []byte {
	return []byte(idxPrefix + repo + ":" + name)
}

// idx:<repo_name>: is the prefix for all secondary index keys for a repository.
func repoIndexPrefix(repo string) []byte {
	return []byte(idxPrefix + repo + ":")
}

// Get performs the constant-time primary lookup used by the MCP search tool.
func (s *Store) Get(name string) (model.FunctionRecord, error) {
	var rec model.FunctionRecord
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(FunctionKey(name))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &rec)
		})
	})
	switch {
	case errors.Is(err, badger.ErrKeyNotFound):
		return model.FunctionRecord{}, ErrNotFound
	case err != nil:
		return model.FunctionRecord{}, fmt.Errorf("read function %q: %w", name, err)
	}
	return rec, nil
}

// Put writes a record only when it differs from the cached copy (structural
// delta check) and keeps the repository ownership index in sync. It reports
// whether a physical write occurred.
func (s *Store) Put(name string, rec model.FunctionRecord) (bool, error) {
	if strings.TrimSpace(name) == "" {
		return false, fmt.Errorf("refusing to cache a record with an empty function name")
	}
	if err := rec.Validate(); err != nil {
		return false, fmt.Errorf("invalid record for %q: %w", name, err)
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("marshal record for %q: %w", name, err)
	}

	changed := false
	err = s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(FunctionKey(name))
		switch {
		case err == nil:
			var existing model.FunctionRecord
			if verr := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &existing)
			}); verr == nil && existing.Fingerprint() == rec.Fingerprint() {
				// No structural delta: keep the index fresh and skip the write.
				return txn.Set(repoIndexKey(rec.RepoName, name), nil)
			}
		case errors.Is(err, badger.ErrKeyNotFound):
		default:
			return err
		}

		if err := txn.Set(FunctionKey(name), payload); err != nil {
			return err
		}
		if err := txn.Set(repoIndexKey(rec.RepoName, name), nil); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("cache function %q: %w", name, err)
	}
	return changed, nil
}

// ListRepoFunctions returns every function name currently attributed to repo.
func (s *Store) ListRepoFunctions(repo string) ([]string, error) {
	prefix := repoIndexPrefix(repo)
	var names []string

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			names = append(names, strings.TrimPrefix(string(it.Item().Key()), string(prefix)))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list functions for repo %q: %w", repo, err)
	}
	return names, nil
}

// PruneRepo removes index entries (and their primary records when still owned
// by repo) for every stale function name. It is used after a re-index cycle to
// evict functions that disappeared from the source tree.
func (s *Store) PruneRepo(repo string, stale []string) (int, error) {
	if len(stale) == 0 {
		return 0, nil
	}
	removed := 0
	err := s.db.Update(func(txn *badger.Txn) error {
		for _, name := range stale {
			if err := txn.Delete(repoIndexKey(repo, name)); err != nil {
				return err
			}
			item, err := txn.Get(FunctionKey(name))
			if errors.Is(err, badger.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			var rec model.FunctionRecord
			if err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &rec)
			}); err != nil {
				return err
			}
			// Another repository may have taken ownership of the key meanwhile.
			if rec.RepoName != repo {
				continue
			}
			if err := txn.Delete(FunctionKey(name)); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune repo %q: %w", repo, err)
	}
	return removed, nil
}

// Stats walks the primary keyspace and aggregates per-repository metrics used
// by the list_repo_capabilities tool.
func (s *Store) Stats() (map[string]RepoStat, error) {
	stats := make(map[string]RepoStat)
	prefix := []byte(funcPrefix)

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			name := strings.TrimPrefix(string(item.Key()), funcPrefix)

			var rec model.FunctionRecord
			if err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &rec)
			}); err != nil {
				return err
			}

			stat, ok := stats[rec.RepoName]
			if !ok {
				stat = RepoStat{
					RepoName:  rec.RepoName,
					Languages: make(map[string]int),
					Files:     make(map[string]int),
				}
			}
			stat.Functions++
			stat.Languages[string(rec.Language)]++
			stat.Files[rec.FilePath]++
			if len(stat.Samples) < 12 {
				stat.Samples = append(stat.Samples, name)
			}
			stats[rec.RepoName] = stat
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("aggregate cache statistics: %w", err)
	}
	return stats, nil
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
