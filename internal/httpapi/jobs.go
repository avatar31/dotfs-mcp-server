package httpapi

import (
	"sync"
	"time"
)

// JobTracker guards the set of repositories currently being re-indexed.
//
// It is the resource-contention control mandated by the specification: a second
// update request for a repository already owned by a background worker is
// rejected instead of queued, protecting CPU and SSD from redundant work.
type JobTracker struct {
	mu     sync.Mutex
	active map[string]time.Time
}

// NewJobTracker returns an empty tracker.
func NewJobTracker() *JobTracker {
	return &JobTracker{active: make(map[string]time.Time)}
}

// TryStart claims repo for a worker. It returns false (and the start time of
// the running job) when the repository is already being processed.
func (t *JobTracker) TryStart(repo string) (bool, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if since, running := t.active[repo]; running {
		return false, since
	}
	started := time.Now()
	t.active[repo] = started
	return true, started
}

// Finish clears the execution flag. Callers must invoke it from a defer so the
// flag is released even if the parsing routine panics.
func (t *JobTracker) Finish(repo string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.active, repo)
}

// Active reports whether repo is currently being indexed.
func (t *JobTracker) Active(repo string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, running := t.active[repo]
	return running
}
