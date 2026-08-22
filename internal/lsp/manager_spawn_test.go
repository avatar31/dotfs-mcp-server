package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// fakeDaemonEnv makes the test binary re-execute itself as a minimal language
// server. A compiled stand-in is used instead of a shell script because a shell
// block-buffers its stdout when it is a pipe, which makes the handshake race.
const fakeDaemonEnv = "DOTFS_TEST_FAKE_LSP"

func TestMain(m *testing.M) {
	if counter := os.Getenv(fakeDaemonEnv); counter != "" {
		runFakeDaemon(counter)
		return
	}
	os.Exit(m.Run())
}

// runFakeDaemon records the start, then answers every request with an empty
// result until stdin closes. Notifications are swallowed.
func runFakeDaemon(counter string) {
	if file, err := os.OpenFile(counter, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(file, "start")
		_ = file.Close()
	}

	stdio := newStdio(os.Stdout, os.Stdin, discardLogger())
	for {
		payload, err := stdio.read()
		if err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil || len(msg.ID) == 0 {
			continue
		}
		reply := serverReply{baseJsonRpc: baseJsonRpc{JSONRPC: JsonRpcVer}, ID: msg.ID, Result: json.RawMessage(`{"capabilities":{}}`)}
		if err := stdio.write(reply); err != nil {
			return
		}
	}
}

func fakeDaemonManager(t *testing.T) (*Manager, string, string) {
	t.Helper()

	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/x\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	counter := filepath.Join(dir, "starts.log")
	t.Setenv(fakeDaemonEnv, counter)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	mgr := NewManager(Config{Enabled: true, GoplsPath: self, InitTimeout: 10 * time.Second}, discardLogger())
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	return mgr, repoDir, counter
}

func starts(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return len(data) / len("start\n")
}

func TestManagerColdStartsOneDaemonForConcurrentCallers(t *testing.T) {
	mgr, repoDir, counter := fakeDaemonManager(t)

	const callers = 8
	var wg sync.WaitGroup
	clients := make([]*Client, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			clients[i], errs[i] = mgr.ClientSession(context.Background(), "repo", repoDir, model.LanguageGo)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if clients[i] != clients[0] {
			t.Errorf("caller %d got a different session", i)
		}
	}
	if got := starts(t, counter); got != 1 {
		t.Errorf("%d daemons were started for one repository, want exactly 1", got)
	}

	// A later request must reuse the warm session rather than pay the cold start.
	again, err := mgr.ClientSession(context.Background(), "repo", repoDir, model.LanguageGo)
	if err != nil {
		t.Fatalf("warm call: %v", err)
	}
	if again != clients[0] {
		t.Error("the warm session was not reused")
	}
	if got := starts(t, counter); got != 1 {
		t.Errorf("a warm call spawned another daemon: %d starts", got)
	}
}

func TestManagerRespawnsAfterTheDaemonDies(t *testing.T) {
	mgr, repoDir, counter := fakeDaemonManager(t)

	first, err := mgr.ClientSession(context.Background(), "repo", repoDir, model.LanguageGo)
	if err != nil {
		t.Fatalf("cold start: %v", err)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	second, err := mgr.ClientSession(context.Background(), "repo", repoDir, model.LanguageGo)
	if err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if second == first {
		t.Fatal("a dead session was handed out again")
	}
	if !second.Alive() {
		t.Error("the replacement session is not alive")
	}
	if got := starts(t, counter); got != 2 {
		t.Errorf("expected exactly one respawn, saw %d starts", got)
	}
}

func TestManagerRefusesToSpawnAfterClose(t *testing.T) {
	mgr, repoDir, counter := fakeDaemonManager(t)

	if err := mgr.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := mgr.ClientSession(context.Background(), "repo", repoDir, model.LanguageGo); err != ErrClosed {
		t.Fatalf("client after close = %v, want ErrClosed", err)
	}
	if got := starts(t, counter); got != 0 {
		t.Errorf("a closed pool started %d daemons", got)
	}
}

func TestManagerClosesEverySessionOnce(t *testing.T) {
	mgr, repoDir, _ := fakeDaemonManager(t)

	client, err := mgr.ClientSession(context.Background(), "repo", repoDir, model.LanguageGo)
	if err != nil {
		t.Fatalf("cold start: %v", err)
	}
	if err := mgr.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if client.Alive() {
		t.Error("the session outlived the pool")
	}
	if err := mgr.Close(context.Background()); err != nil {
		t.Errorf("close must be idempotent, got %v", err)
	}
}
