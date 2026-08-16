package auth

import "time"

// SessionState enumerates the lifecycle of a session record.
type SessionState int

// Session lifecycle states shared with the audit pipeline.
const (
	// StatusPending marks a session that has been issued but not yet used.
	StatusPending SessionState = iota
	// StatusActive marks a session currently authorising requests.
	StatusActive
	// StatusRevoked marks a session invalidated ahead of its TTL.
	StatusRevoked
)

// DefaultTTL bounds how long an issued session stays valid.
const DefaultTTL = 15 * time.Minute

// Metadata is embedded into every persisted record.
type Metadata struct {
	CreatedAt time.Time `json:"created_at"`
}

// Session is the serialized session record shared with the audit pipeline.
type Session struct {
	Metadata
	ID      string        `json:"id" yaml:"id"`
	Account string        `json:"account"`
	State   SessionState  `json:"state"`
	TTL     time.Duration `json:"ttl"`
}

// Loader reads sessions by identifier.
type Loader interface {
	Load(id string) (Session, error)
}

// SessionStore abstracts the persistence backend for sessions.
type SessionStore interface {
	Loader
	Save(s Session) error
	Delete(id string) error
}

// AccountID is an alias kept for backwards compatibility with the v1 API.
type AccountID = string
