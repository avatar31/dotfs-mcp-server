package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avatar31/dotfs-mcp-server/internal/indexer"
)

// stubIndexer blocks inside IndexRepo until release is closed so the test can
// deterministically observe the in-flight state.
type stubIndexer struct {
	started chan struct{}
	release chan struct{}
	calls   int
}

func (s *stubIndexer) IndexRepo(ctx context.Context, repo string) (indexer.Summary, error) {
	s.calls++
	if s.started != nil {
		s.started <- struct{}{}
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return indexer.Summary{}, ctx.Err()
		}
	}
	return indexer.Summary{Repo: repo, FilesParsed: 1}, nil
}

func (s *stubIndexer) ListRepos() ([]string, error) {
	return []string{"auth-service-go"}, nil
}

func newTestServer(t *testing.T, token string, stub *stubIndexer) (*Server, string) {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "auth-service-go"), 0o755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}

	srv, err := New(context.Background(), Config{
		Addr:          "127.0.0.1:0",
		APIToken:      token,
		WorkspaceRoot: root,
	}, stub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, root
}

func TestUpdateAcceptsAndRejectsConcurrentCycles(t *testing.T) {
	stub := &stubIndexer{started: make(chan struct{}, 1), release: make(chan struct{})}
	srv, _ := newTestServer(t, "", stub)

	first := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/api/v1/auth-service-go/update", nil))
	if first.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", first.Code, first.Body.String())
	}

	select {
	case <-stub.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background worker never started")
	}

	second := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/api/v1/auth-service-go/update", nil))
	if second.Code != http.StatusConflict {
		t.Fatalf("want 409 while a cycle is running, got %d: %s", second.Code, second.Body.String())
	}

	close(stub.release)

	// The deferred teardown must clear the flag once the worker finishes.
	deadline := time.Now().Add(5 * time.Second)
	for srv.jobs.Active("auth-service-go") {
		if time.Now().After(deadline) {
			t.Fatal("execution flag was never cleared")
		}
		time.Sleep(10 * time.Millisecond)
	}

	third := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(third, httptest.NewRequest(http.MethodPost, "/api/v1/auth-service-go/update", nil))
	if third.Code != http.StatusAccepted {
		t.Fatalf("want 202 after the previous cycle finished, got %d", third.Code)
	}
}

func TestUpdateRejectsUnknownAndUnsafeRepositories(t *testing.T) {
	srv, _ := newTestServer(t, "", &stubIndexer{})

	missing := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v1/no-such-service/update", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("want 404 for an unknown repository, got %d", missing.Code)
	}

	traversal := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(traversal, httptest.NewRequest(http.MethodPost, "/api/v1/..%2Fetc/update", nil))
	if traversal.Code != http.StatusBadRequest && traversal.Code != http.StatusNotFound {
		t.Fatalf("want a rejection for a traversal attempt, got %d", traversal.Code)
	}
}

func TestUpdateRequiresBearerTokenWhenConfigured(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret", &stubIndexer{})

	anonymous := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodPost, "/api/v1/auth-service-go/update", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a token, got %d", anonymous.Code)
	}

	wrong := httptest.NewRequest(http.MethodPost, "/api/v1/auth-service-go/update", nil)
	wrong.Header.Set("Authorization", "Bearer nope")
	wrongRec := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(wrongRec, wrong)
	if wrongRec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with a wrong token, got %d", wrongRec.Code)
	}

	authed := httptest.NewRequest(http.MethodPost, "/api/v1/auth-service-go/update", nil)
	authed.Header.Set("Authorization", "Bearer s3cret")
	authedRec := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(authedRec, authed)
	if authedRec.Code != http.StatusAccepted {
		t.Fatalf("want 202 with the correct token, got %d", authedRec.Code)
	}
}

func TestGetMethodIsNotRoutedToUpdate(t *testing.T) {
	srv, _ := newTestServer(t, "", &stubIndexer{})

	rec := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth-service-go/update", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405 for GET, got %d", rec.Code)
	}
}

func TestListReposReportsIndexingState(t *testing.T) {
	srv, _ := newTestServer(t, "", &stubIndexer{})

	rec := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	var payload struct {
		Repositories []struct {
			Name     string `json:"name"`
			Indexing bool   `json:"indexing"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Repositories) != 1 || payload.Repositories[0].Name != "auth-service-go" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if payload.Repositories[0].Indexing {
		t.Fatal("no cycle is running, indexing must be false")
	}
}

