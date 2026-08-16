# dotfs-mcp-server

A Model Context Protocol (MCP) server that gives an LLM client surgical,
structural access to a multi-repository workspace of **C** and **Go**
microservices — without shell access and without shipping whole files into the
context window.

Source is parsed locally into ASTs (Tree-sitter for C, `go/parser` for Go),
reduced to function-level semantic blocks and cached in an embedded BadgerDB
key-value store for microsecond lookups. A concurrent REST API lets operators
and CI webhooks re-index a single repository on demand.

---

## 1. Architecture

| Component | Package | Responsibility |
|---|---|---|
| MCP bridge (stdio) | `internal/mcpserver` | Exposes `global_codebase_search` and `list_repo_capabilities` to the LLM |
| Management REST API | `internal/httpapi` | `POST /api/v1/{repo_name}/update` + mutex-guarded job tracking (HTTP 409 on contention) |
| Dual AST parser layer | `internal/parser` | `.go` → `go/token`, `go/parser`, `go/ast`; `.c` / `.h` → Tree-sitter C grammar |
| Filter-Then-Parse indexer | `internal/indexer` | Phase 1 `bytes.Contains` scan → Phase 2 extension-routed AST extraction |
| Cache | `internal/store` | Embedded BadgerDB (LSM) at `./agent_knowledge` |
| Capability matrix | `internal/capabilities` | Curated repo profiles merged with observed cache facts |

```
LLM client ──stdio(JSON-RPC)──► MCP tools ──► BadgerDB cache ◄── indexer ◄── workspace/*
operator/CI ──HTTP POST────────► job tracker ──► background worker ──┘
```

### Cache schema

| Key | Value |
|---|---|
| `func:<function_name>` | JSON `FunctionRecord` |
| `idx:<repo_name>:<function_name>` | empty (repository ownership index, used for pruning) |

```json
{
  "repo_name": "packet-router-c",
  "file_path": "packet-router-c/router.c",
  "language": "c",
  "documentation": "read_session_header copies the fixed 32-byte session header ...",
  "source_code": "int read_session_header(const unsigned char *frame, ...) { ... }"
}
```

`language` is always `"c"` or `"go"`, so the client can pick the right markdown
fence and linting rules. `file_path` is workspace-relative — absolute host paths
are never leaked to the model.

Go methods are indexed under both `Issue` and `Issuer.Issue`, so symbols taken
straight from a stack trace resolve.

---

## 2. Prerequisites

* Go **1.24+** (module targets `go 1.26.3`)
* A C toolchain (`gcc`/`clang`) — **cgo is mandatory**, the C engine links the
  Tree-sitter grammar
* Linux or macOS

```bash
go version && gcc --version
```

---

## 3. Build

```bash
git clone <this-repo> && cd dotfs-mcp-server
make build          # -> bin/dotfs-mcp-server
```

Other targets: `make test`, `make race`, `make vet`, `make fmt`, `make clean`.

> Cross-compiling requires a cross C toolchain; `CGO_ENABLED=0` builds will not
> compile.

---

## 4. Configure

### Step 1 — lay out the workspace

`DOTFS_WORKSPACE_ROOT` points at a **parent** directory whose immediate
sub-directories are the repositories. The directory name becomes `repo_name`.

```
/srv/workspace/
├── auth-service-go/      # repo_name = auth-service-go
│   └── ...*.go
└── packet-router-c/      # repo_name = packet-router-c
    └── ...*.c, *.h
```

A ready-made sample lives in `testdata/workspace/`:

```bash
mkdir -p /srv/workspace
cp -r testdata/workspace/* /srv/workspace/
```

### Step 2 — set the environment

| Variable | Default | Purpose |
|---|---|---|
| `DOTFS_WORKSPACE_ROOT` | `./workspace` | Parent directory of the repositories |
| `DOTFS_CACHE_DB` | `./agent_knowledge` | BadgerDB directory |
| `DOTFS_HTTP_ADDR` | `127.0.0.1:8080` | Management API listen address |
| `DOTFS_HTTP_ENABLED` | `true` | Set `false` to run stdio-only |
| `DOTFS_API_TOKEN` | *(empty)* | When set, requires `Authorization: Bearer <token>` |
| `DOTFS_CAPABILITIES_FILE` | *(empty)* | JSON repository capability matrix |
| `DOTFS_INDEX_ON_START` | `true` | Index the whole workspace at boot (async) |
| `DOTFS_MAX_FILE_SIZE` | `2097152` | Skip source files larger than this (bytes) |
| `DOTFS_SKIP_DIRS` | `.git,node_modules,vendor,...` | Comma-separated directory names to prune |
| `DOTFS_GC_INTERVAL` | `10m` | BadgerDB value-log GC cadence |
| `DOTFS_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `DOTFS_SERVER_NAME` / `DOTFS_SERVER_VERSION` | `dotfs-mcp-server` / `1.0.0` | Advertised during the MCP handshake |

All logs go to **stderr**; stdout is reserved for the MCP JSON-RPC framing.

### Step 3 — describe your services (optional but recommended)

`list_repo_capabilities` merges a curated profile with cache-derived facts.
Copy the template and edit it:

```bash
cp configs/capabilities.example.json configs/capabilities.json
export DOTFS_CAPABILITIES_FILE=$PWD/configs/capabilities.json
```

```json
[
  {
    "repo": "auth-service-go",
    "language": "Go 1.22 (net/http, HMAC session tokens)",
    "summary": "Issues and validates opaque session tokens ...",
    "features": ["Session token issuance and HMAC verification"],
    "interfaces": ["Binary: 32-byte session header shared with packet-router-c"],
    "owners": ["identity-platform"],
    "criticality": "tier-1"
  }
]
```

Repositories without a profile still work — the briefing is then derived purely
from the indexed cache.

### Step 4 — register the server with your LLM client

**Claude Desktop** (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "dotfs-codebase": {
      "command": "/opt/dotfs/bin/dotfs-mcp-server",
      "env": {
        "DOTFS_WORKSPACE_ROOT": "/srv/workspace",
        "DOTFS_CACHE_DB": "/var/lib/dotfs/agent_knowledge",
        "DOTFS_CAPABILITIES_FILE": "/opt/dotfs/capabilities.json",
        "DOTFS_HTTP_ADDR": "127.0.0.1:8080",
        "DOTFS_API_TOKEN": "change-me"
      }
    }
  }
}
```

**Cursor** (`.cursor/mcp.json`) and **VS Code** (`.vscode/mcp.json`) use the same
`command` / `env` shape with `"type": "stdio"`.

Restart the client, then confirm that `global_codebase_search` and
`list_repo_capabilities` appear in its tool list.

---

## 5. Tools exposed to the LLM

### `global_codebase_search(target_function_name)`

O(1) BadgerDB lookup. On a cache miss it falls back to a live, 30 s-bounded
Filter-Then-Parse scan of the workspace and back-fills the cache. Returns the
`FunctionRecord` JSON verbatim.

### `list_repo_capabilities(repo_name)`

Returns a markdown briefing: language stack, business responsibility,
implemented features, integration interfaces and the indexed structural
footprint (symbol counts plus representative entry points).

---

## 6. Management REST API

| Method | Path | Result |
|---|---|---|
| `POST` | `/api/v1/{repo_name}/update` | `202` job accepted, `409` already indexing, `400` invalid name, `404` unknown repo, `401` bad token |
| `GET` | `/api/v1/repos` | Repositories plus their live indexing state |
| `GET` | `/healthz` | Liveness probe |

```bash
curl -X POST -H "Authorization: Bearer change-me" \
     http://127.0.0.1:8080/api/v1/auth-service-go/update
# {"repo":"auth-service-go","started_at":"...","status":"accepted"}
```

Concurrency control: an in-memory map guarded by a `sync.Mutex` tracks active
repositories. A duplicate request is rejected immediately with `409 Conflict`
instead of queueing, and the execution flag is released by a `defer` once the
worker has flushed its BadgerDB writes. Reads from the LLM are never blocked by
an in-flight cycle.

Wire it to a CI post-merge hook to keep the cache hot without restarts.

---

## 7. How indexing works

**Phase 1 — fast string scan.** Files are read into memory and rejected with
`bytes.Contains` before any AST work: a live search rejects files that lack the
literal symbol, and a full index rejects files that cannot declare a function at
all.

**Phase 2 — routing-aware AST extraction.** The extension picks the engine:

* `.go` → `parser.ParseFile` with `ParseComments`; every `*ast.FuncDecl` yields
  its exact byte scope (`.Pos()`/`.End()`) and its `Doc` comment group, stripped
  of `//` boilerplate. The doc block is deliberately excluded from
  `source_code`.
* `.c` / `.h` → Tree-sitter; only nodes typed `function_definition` are
  harvested, so occurrences inside string literals, `printf` calls or macros can
  never be mistaken for a definition. Contiguous `comment` siblings directly
  above the node become the documentation (a blank line ends the block).

Writes are delta-checked with a SHA-256 fingerprint, so unchanged functions
cause no disk I/O. Functions deleted from the source tree are pruned from the
cache — and only when the record is still owned by the repository being indexed.

---

## 8. Security posture

* No shell execution and no arbitrary file reads: the model only ever receives
  parsed function records.
* `repo_name` is validated against a strict allowlist pattern and re-checked
  after symlink resolution, so it cannot escape the workspace root.
* Symlinked files are never followed during the walk.
* The management API binds to loopback by default and supports a bearer token
  compared in constant time.
* File paths returned to the model are workspace-relative.

---

## 9. Operating notes

* **Cold start:** the initial index runs asynchronously so the MCP handshake is
  never delayed; early lookups fall back to the live scan.
* **Same symbol in two repositories:** `func:<name>` is a single key — the last
  writer wins. Use receiver-qualified Go names or repository-scoped re-indexing
  when this matters.
* **Cache reset:** stop the server and delete `DOTFS_CACHE_DB`.
* **Debugging:** `DOTFS_LOG_LEVEL=debug` adds per-file filter decisions and
  BadgerDB internals on stderr.

---

## 10. Tests

```bash
make test     # unit + integration tests
make race     # race detector
```

Coverage includes AST extraction for both languages (including the
string-literal false-positive case), cache delta/prune semantics, path-traversal
rejection, live fallback search and the `202`/`409`/`401`/`404` behaviour of the
sync API.

---

## License

See [LICENSE](LICENSE).
