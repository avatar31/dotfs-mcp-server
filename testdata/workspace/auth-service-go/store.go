package auth

import "sync"

// memoryStore is the in-process SessionStore used by tests and local runs.
type memoryStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
}

// Load implements Loader.
func (m *memoryStore) Load(id string) (Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[id]
	if !ok {
		return Session{}, ErrExpired
	}
	return session, nil
}

// Save implements SessionStore.
func (m *memoryStore) Save(s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[string]Session)
	}
	m.sessions[s.ID] = s
	return nil
}

// Delete implements SessionStore.
func (m *memoryStore) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}
