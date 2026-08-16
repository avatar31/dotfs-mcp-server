// Package store wraps the embedded BadgerDB cache that persists parsed
// multi-language AST metadata between server reboots.
//
// Phase 2 key layout (a single typed namespace, no single-dimension keys):
//
//	sym:<repo>:<file_path>:<symbol_type>:<name>:<offset>          -> JSON model.SymbolRecord
//	idx:name:<name>:<repo>:<file_path>:<offset>                   -> primary key
//	idx:type:<symbol_type>:<name>:<repo>:<file_path>:<offset>     -> primary key
//	idx:file:<repo>:<file_path>:<offset>                          -> primary key
//
// <offset> is the symbol start byte rendered with %08d so the LSM tree keeps
// symbols of one file in source order.
//
// The specification marks index values as empty. This implementation stores the
// primary key instead: it turns "index hit -> record" into a single O(1) Get
// rather than a prefix scan, and an index entry is worthless without it.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/dgraph-io/badger/v4"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

const (
	symPrefix     = "sym:"
	nameIdxPrefix = "idx:name:"
	typeIdxPrefix = "idx:type:"
	fileIdxPrefix = "idx:file:"

	// deleteBatch bounds a single prune transaction so a very large repository
	// can never exceed BadgerDB's maximum transaction size.
	deleteBatch = 512
)

// ErrNotFound is returned when a symbol is absent from the cache.
var ErrNotFound = errors.New("symbol not found in cache")

// Store is a concurrency-safe handle around BadgerDB.
type Store struct {
	db  *badger.DB
	log *slog.Logger
}

// Filter narrows a symbol lookup.
type Filter struct {
	// Repo restricts results to one repository ("" means every repository).
	Repo string
	// Types restricts results to the listed symbol kinds (nil means all kinds).
	Types []model.SymbolType
	// ExactOnly rejects prefix matches, keeping only identical identifiers.
	ExactOnly bool
	// Limit caps the number of returned records (<= 0 means DefaultLookupLimit).
	Limit int
}

// DefaultLookupLimit bounds an unfiltered lookup so a one-character prefix can
// never flood the LLM context window.
const DefaultLookupLimit = 25

// RepoStat aggregates what the cache knows about a single repository.
type RepoStat struct {
	RepoName    string         `json:"repo_name"`
	Symbols     int            `json:"symbol_count"`
	Functions   int            `json:"function_count"`
	Languages   map[string]int `json:"languages"`
	SymbolTypes map[string]int `json:"symbol_types"`
	Files       map[string]int `json:"-"`
	Samples     []string       `json:"sample_symbols"`
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

// esc neutralises the key separator inside a variable component. Only key
// construction is affected; the persisted record always holds the real value.
func esc(part string) string { return strings.ReplaceAll(part, ":", "%3A") }

// offsetToken renders a byte offset with the mandated 8-character zero padding
// so byte ranges sort lexicographically inside the LSM tree.
func offsetToken(off int) string { return fmt.Sprintf("%08d", off) }

// PrimaryKey builds sym:<repo>:<file_path>:<symbol_type>:<name>:<offset>.
func PrimaryKey(rec model.SymbolRecord) string {
	return symPrefix + esc(rec.RepoName) + ":" + esc(rec.FilePath) + ":" +
		esc(string(rec.SymbolType)) + ":" + esc(rec.Name) + ":" + offsetToken(rec.StartByte)
}

// nameIndexKey builds idx:name:<name>:<repo>:<file_path>:<offset>.
func nameIndexKey(name string, rec model.SymbolRecord) string {
	return nameIdxPrefix + esc(name) + ":" + esc(rec.RepoName) + ":" +
		esc(rec.FilePath) + ":" + offsetToken(rec.StartByte)
}

// typeIndexKey builds idx:type:<symbol_type>:<name>:<repo>:<file_path>:<offset>.
func typeIndexKey(name string, rec model.SymbolRecord) string {
	return typeIdxPrefix + esc(string(rec.SymbolType)) + ":" + esc(name) + ":" +
		esc(rec.RepoName) + ":" + esc(rec.FilePath) + ":" + offsetToken(rec.StartByte)
}

// fileIndexKey builds idx:file:<repo>:<file_path>:<offset>.
func fileIndexKey(rec model.SymbolRecord) string {
	return fileIdxPrefix + esc(rec.RepoName) + ":" + esc(rec.FilePath) + ":" + offsetToken(rec.StartByte)
}

// repoSymbolPrefix is the primary-record prefix used for repository-wide
// invalidation, i.e. "sym:<repo>:".
func repoSymbolPrefix(repo string) []byte { return []byte(symPrefix + esc(repo) + ":") }

// fileSymbolPrefix is "sym:<repo>:<file_path>:".
func fileSymbolPrefix(repo, file string) []byte {
	return []byte(symPrefix + esc(repo) + ":" + esc(file) + ":")
}

// indexKeys returns every secondary key that must accompany rec, covering the
// canonical name and each declared alias.
func indexKeys(rec model.SymbolRecord) []string {
	names := append([]string{rec.Name}, rec.Aliases...)
	keys := make([]string, 0, 2*len(names)+1)
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		keys = append(keys, nameIndexKey(n, rec), typeIndexKey(n, rec))
	}
	return append(keys, fileIndexKey(rec))
}

// PutSymbol writes a record only when it differs from the cached copy
// (structural delta check) and refreshes every secondary index. It returns the
// primary key together with a flag reporting whether a physical write occurred.
func (s *Store) PutSymbol(rec model.SymbolRecord) (string, bool, error) {
	if err := rec.Validate(); err != nil {
		return "", false, fmt.Errorf("invalid record for %q: %w", rec.Name, err)
	}

	primary := PrimaryKey(rec)
	payload, err := json.Marshal(rec)
	if err != nil {
		return "", false, fmt.Errorf("marshal record for %q: %w", rec.Name, err)
	}

	changed := false
	err = s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(primary))
		switch {
		case err == nil:
			var existing model.SymbolRecord
			verr := item.Value(func(val []byte) error { return json.Unmarshal(val, &existing) })
			if verr == nil && existing.Fingerprint() == rec.Fingerprint() {
				// No structural delta: refresh the indexes and skip the write.
				return writeIndexes(txn, rec, primary)
			}
		case errors.Is(err, badger.ErrKeyNotFound):
		default:
			return err
		}

		if err := txn.Set([]byte(primary), payload); err != nil {
			return err
		}
		if err := writeIndexes(txn, rec, primary); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return "", false, fmt.Errorf("cache symbol %q: %w", rec.Name, err)
	}
	return primary, changed, nil
}

func writeIndexes(txn *badger.Txn, rec model.SymbolRecord, primary string) error {
	for _, key := range indexKeys(rec) {
		if err := txn.Set([]byte(key), []byte(primary)); err != nil {
			return err
		}
	}
	return nil
}

// candidate pairs a primary key with the identifier that matched the query, so
// alias hits can still be classified as exact.
type candidate struct {
	primary string
	matched string
}

// Lookup resolves a symbol by exact name or name prefix. Supplying Filter.Types
// switches the scan to the type-partitioned index, which keeps a query such as
// "every struct called fsal_*" proportional to the result set.
func (s *Store) Lookup(name string, f Filter) ([]model.SymbolRecord, error) {
	query := strings.TrimSpace(name)
	if query == "" {
		return nil, errors.New("lookup requires a non-empty symbol name")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLookupLimit
	}

	prefixes := make([][]byte, 0, len(f.Types))
	if len(f.Types) == 0 {
		prefixes = append(prefixes, []byte(nameIdxPrefix+esc(query)))
	} else {
		for _, t := range f.Types {
			prefixes = append(prefixes, []byte(typeIdxPrefix+esc(string(t))+":"+esc(query)))
		}
	}

	var records []model.SymbolRecord
	err := s.db.View(func(txn *badger.Txn) error {
		seen := make(map[string]struct{})
		var cands []candidate

		for _, prefix := range prefixes {
			opts := badger.DefaultIteratorOptions
			opts.Prefix = prefix
			it := txn.NewIterator(opts)
			for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
				item := it.Item()
				matched, ok := matchedName(string(item.Key()), len(f.Types) > 0)
				if !ok {
					continue
				}
				if f.ExactOnly && matched != query {
					continue
				}
				primary, err := item.ValueCopy(nil)
				if err != nil {
					it.Close()
					return err
				}
				if _, dup := seen[string(primary)]; dup {
					continue
				}
				seen[string(primary)] = struct{}{}
				cands = append(cands, candidate{primary: string(primary), matched: matched})
			}
			it.Close()
		}

		for _, c := range cands {
			item, err := txn.Get([]byte(c.primary))
			if errors.Is(err, badger.ErrKeyNotFound) {
				continue // dangling index entry: the record was pruned
			}
			if err != nil {
				return err
			}
			var rec model.SymbolRecord
			if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &rec) }); err != nil {
				return err
			}
			if f.Repo != "" && rec.RepoName != f.Repo {
				continue
			}
			records = append(records, rec)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("lookup symbol %q: %w", query, err)
	}

	sortRecords(records, query)
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

// matchedName extracts the identifier component from a secondary index key.
// Name keys are idx:name:<name>:..., type keys are idx:type:<type>:<name>:...
func matchedName(key string, typed bool) (string, bool) {
	var rest string
	switch {
	case typed && strings.HasPrefix(key, typeIdxPrefix):
		rest = key[len(typeIdxPrefix):]
		sep := strings.IndexByte(rest, ':')
		if sep < 0 {
			return "", false
		}
		rest = rest[sep+1:]
	case !typed && strings.HasPrefix(key, nameIdxPrefix):
		rest = key[len(nameIdxPrefix):]
	default:
		return "", false
	}
	sep := strings.IndexByte(rest, ':')
	if sep < 0 {
		return "", false
	}
	return strings.ReplaceAll(rest[:sep], "%3A", ":"), true
}

// sortRecords puts exact identifier matches first, then orders deterministically
// so repeated tool calls return a stable document.
func sortRecords(records []model.SymbolRecord, query string) {
	rank := func(r model.SymbolRecord) int {
		if r.Name == query {
			return 0
		}
		for _, a := range r.Aliases {
			if a == query {
				return 1
			}
		}
		return 2
	}
	sort.SliceStable(records, func(i, j int) bool {
		if ri, rj := rank(records[i]), rank(records[j]); ri != rj {
			return ri < rj
		}
		if records[i].Name != records[j].Name {
			return records[i].Name < records[j].Name
		}
		if records[i].RepoName != records[j].RepoName {
			return records[i].RepoName < records[j].RepoName
		}
		if records[i].FilePath != records[j].FilePath {
			return records[i].FilePath < records[j].FilePath
		}
		return records[i].StartByte < records[j].StartByte
	})
}

// FileSymbols returns every symbol recorded for one file, in source order.
func (s *Store) FileSymbols(repo, file string) ([]model.SymbolRecord, error) {
	prefix := fileSymbolPrefix(repo, file)
	records, err := s.scanRecords(prefix)
	if err != nil {
		return nil, fmt.Errorf("list symbols for %s/%s: %w", repo, file, err)
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].StartByte < records[j].StartByte })
	return records, nil
}

// RepoSymbolKeys returns the primary key of every symbol currently attributed to
// repo. The indexer diffs this set against a freshly parsed one.
func (s *Store) RepoSymbolKeys(repo string) ([]string, error) {
	prefix := repoSymbolPrefix(repo)
	var keys []string
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			keys = append(keys, string(it.Item().KeyCopy(nil)))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list symbol keys for repo %q: %w", repo, err)
	}
	return keys, nil
}

// PruneRepo implements incremental invalidation: it scans every key under
// "sym:<repo>:" and removes each record (plus its secondary index entries) whose
// primary key is absent from keep. Deletions are batched so a large repository
// never exceeds the maximum transaction size.
func (s *Store) PruneRepo(repo string, keep map[string]struct{}) (int, error) {
	prefix := repoSymbolPrefix(repo)

	var stale []model.SymbolRecord
	var staleKeys []string
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			key := string(item.KeyCopy(nil))
			if _, live := keep[key]; live {
				continue
			}
			var rec model.SymbolRecord
			if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &rec) }); err != nil {
				// A corrupt payload still has to go; the key alone is enough.
				s.log.Warn("dropping unreadable cache record", "key", key, "error", err)
			}
			stale = append(stale, rec)
			staleKeys = append(staleKeys, key)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("scan repo %q for invalidation: %w", repo, err)
	}

	removed := 0
	for start := 0; start < len(staleKeys); start += deleteBatch {
		end := min(start+deleteBatch, len(staleKeys))
		batchKeys, batchRecs := staleKeys[start:end], stale[start:end]

		err := s.db.Update(func(txn *badger.Txn) error {
			for i, key := range batchKeys {
				if err := txn.Delete([]byte(key)); err != nil {
					return err
				}
				if batchRecs[i].Name == "" {
					continue
				}
				for _, idx := range indexKeys(batchRecs[i]) {
					if err := txn.Delete([]byte(idx)); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return removed, fmt.Errorf("invalidate stale records for repo %q: %w", repo, err)
		}
		removed += len(batchKeys)
	}
	return removed, nil
}

// scanRecords materialises every record stored under prefix.
func (s *Store) scanRecords(prefix []byte) ([]model.SymbolRecord, error) {
	var records []model.SymbolRecord
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			var rec model.SymbolRecord
			if err := it.Item().Value(func(val []byte) error { return json.Unmarshal(val, &rec) }); err != nil {
				return err
			}
			records = append(records, rec)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// Stats walks the primary keyspace and aggregates per-repository metrics used
// by the list_repo_capabilities tool.
func (s *Store) Stats() (map[string]RepoStat, error) {
	stats := make(map[string]RepoStat)
	prefix := []byte(symPrefix)

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			var rec model.SymbolRecord
			if err := it.Item().Value(func(val []byte) error { return json.Unmarshal(val, &rec) }); err != nil {
				return err
			}

			stat, ok := stats[rec.RepoName]
			if !ok {
				stat = RepoStat{
					RepoName:    rec.RepoName,
					Languages:   make(map[string]int),
					SymbolTypes: make(map[string]int),
					Files:       make(map[string]int),
				}
			}
			stat.Symbols++
			if rec.SymbolType == model.SymbolFunction || rec.SymbolType == model.SymbolMethod {
				stat.Functions++
			}
			stat.Languages[string(rec.Language)]++
			stat.SymbolTypes[string(rec.SymbolType)]++
			stat.Files[rec.FilePath]++
			if len(stat.Samples) < 12 {
				stat.Samples = append(stat.Samples, rec.Name)
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
