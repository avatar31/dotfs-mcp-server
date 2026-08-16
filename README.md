# dotfs-mcp-server

A Model Context Protocol (MCP) server that gives an LLM client surgical,
structural access to a multi-repository workspace of **C** and **Go**
microservices — without shell access and without shipping whole files into the
context window.

Source is parsed locally into ASTs (Tree-sitter for C, `go/parser` for Go),
reduced to a complete **symbol table** — functions, methods, structs, unions,
interfaces, enums, typedefs, type aliases, macros and constants — and cached in
an embedded BadgerDB key-value store for microsecond lookups. A concurrent REST API lets operators
and CI webhooks re-index a single repository on demand.

---

## 1. Architecture

| Component | Package | Responsibility |
|---|---|---|
| MCP bridge (stdio) | `internal/mcpserver` | Exposes six read-only tools (see §5) to the LLM |
| Management REST API | `internal/httpapi` | `POST /api/v1/{repo_name}/update` + mutex-guarded job tracking (HTTP 409 on contention) |
| Dual AST parser layer | `internal/parser` | `.go` → `go/token`, `go/parser`, `go/ast`; `.c` / `.h` → Tree-sitter C grammar |
| Filter-Then-Parse indexer | `internal/indexer` | Phase 1 `bytes.Contains` scan → Phase 2 extension-routed AST extraction |
| Cache | `internal/store` | Embedded BadgerDB (LSM) at `./agent_knowledge`, typed symbol namespace + three secondary indexes |
| Capability matrix | `internal/capabilities` | Curated repo profiles merged with observed cache facts |

```
LLM client ──stdio(JSON-RPC)──► MCP tools ──► BadgerDB cache ◄── indexer ◄── workspace/*
operator/CI ──HTTP POST────────► job tracker ──► background worker ──┘
```

### Symbol taxonomy

Every record carries one `symbol_type` drawn from a closed enumeration:

| `symbol_type` | C source | Go source |
|---|---|---|
| `function` | `function_definition`, prototypes in headers | `func Name(...)` |
| `method` | — | `func (r Recv) Name(...)` |
| `struct` | `struct` **and** `union` specifiers | `type T struct{...}` |
| `interface` | — | `type T interface{...}` |
| `enum` | `enum` specifiers | — |
| `typedef` | `type_definition` | `type T Underlying` |
| `type_alias` | — | `type T = Underlying` |
| `macro` | `#define NAME value` | — |
| `macro_function` | `#define NAME(a, b) ...` | — |
| `constant` | enumerators | `const` specs and exported package-level `var`s |

Two deliberate mapping decisions, made because the enumeration above is closed:

* A C `union` is stored as `struct` with the signature `union <name>`, so a
  client asking for "the definition of this type" gets it without needing to
  know which specifier the author used.
* A Go package-level `var` is stored as `constant` with a `var ...` signature.
  Only **exported** vars are indexed; unexported package state is noise for an
  API-oriented consumer.

### Cache schema

| Key | Value |
|---|---|
| `sym:<repo>:<file>:<type>:<name>:<offset>` | JSON `SymbolRecord` (the primary record) |
| `idx:name:<name>:<repo>:<file>:<offset>` | the primary key |
| `idx:type:<type>:<name>:<repo>:<file>:<offset>` | the primary key |
| `idx:file:<repo>:<file>:<offset>` | the primary key |

`<offset>` is `start_byte` rendered as a zero-padded 8-digit decimal so that
BadgerDB's lexicographic iteration returns symbols in source order. Any `:`
inside a component is escaped as `%3A`, so a key can never be ambiguous.

> **Design note.** The index values hold the primary key rather than being
> empty. It costs a few dozen bytes per symbol and removes an entire class of
> key-reconstruction bugs: a prefix scan reads the pointer and follows it, so
> the index and the record can never disagree about escaping or padding.

```json
{
  "repo_name": "packet-router-c",
  "file_path": "router.h",
  "language": "c",
  "symbol_type": "struct",
  "name": "router_ops",
  "parent_scope": "",
  "start_byte": 812,
  "end_byte": 1041,
  "start_line": 34,
  "end_line": 40,
  "documentation": "router_ops is the transport v-table...",
  "signature": "struct router_ops",
  "source_code": "struct router_ops {\n    int (*open)(...);\n};"
}
```

`language` is always `"c"` or `"go"`, so the client can pick the right markdown
fence and linting rules. `file_path` is **repository-relative** (`router.h`, not
`/srv/workspace/packet-router-c/router.h`) — absolute host paths are never
leaked to the model, and the value can be handed straight back to
`read_code_snippet`.

`aliases` lets one record answer to several names: Go methods are indexed under
both `Issue` and `Issuer.Issue`, so a symbol taken straight from a stack trace
resolves.

`parent_scope` carries the owning declaration for nested symbols — an enumerator
records its enum (or, for the anonymous `typedef enum { ... } name_t` idiom, the
typedef name), and a Go method records its receiver type.

For grouped declarations (a Go `const (...)` block, a C `enum`), `source_code`
is the **whole block** — an LLM reasoning about `iota` or implicit enumerator
values needs its siblings — while `start_byte`/`start_line` point at the
individual member so `read_code_snippet` can still zoom in.

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

All six tools are annotated read-only, idempotent and closed-world: the model is
told up front that nothing it calls here can mutate the workspace.

Every lookup follows the same two-tier resolution strategy — an O(1) BadgerDB
read first, and on a miss a live, 30 s-bounded Filter-Then-Parse scan of the
workspace that back-fills the cache. A cold cache therefore degrades latency,
never correctness.

### `global_codebase_search(target_function_name)`

Exact-match lookup restricted to `function` and `method`. Returns the
`SymbolRecord` JSON verbatim — a bare object when the name is unique, an array
when several repositories (or a C header and its `.c` file) declare it.

```json
{"target_function_name": "route_packet"}
```

### `lookup_symbol(name, repo_name?, symbol_type?)`

The general entry point. `name` is a **prefix** match, so `ERR_FSAL` enumerates
an entire error family in one call. Optional `symbol_type` narrows to a single
kind from the taxonomy in §1; optional `repo_name` narrows to one repository.
Results are exact matches first, then prefix matches, capped at 25 records.

```json
{"name": "ERR_FSAL", "symbol_type": "macro", "repo_name": "nfs-ganesha"}
```

### `get_type_definition(type_name, repo_name?)`

Exact-match lookup restricted to `struct`, `interface`, `enum`, `typedef` and
`type_alias`. Use this when the model has seen a type in a signature and needs
its members, not its call sites.

```json
{"type_name": "router_ops"}
```

### `lookup_macro_or_const(name, repo_name?)`

Exact-match lookup restricted to `macro`, `macro_function` and `constant` —
the answer to "what is the numeric value behind this flag?".

```json
{"name": "ROUTER_QUEUE_DEPTH"}
```

### `read_code_snippet(repo_name, file_path, start_line, end_line)`

Bounded, line-numbered escape hatch for the code *between* symbols. `file_path`
is repository-relative, exactly as returned in a `SymbolRecord`. The range is
clamped to **200 lines**; the path is rejected if it is absolute, contains `..`,
or resolves outside the repository after symlink evaluation.

```json
{"repo_name": "packet-router-c", "file_path": "router.c", "start_line": 40, "end_line": 96}
```

```
packet-router-c/router.c:40-96
    40 | int route_packet(const struct router_ops *ops, ...)
    41 | {
```

### `list_repo_capabilities(repo_name)`

Returns a markdown briefing: language stack, business responsibility,
implemented features, integration interfaces and the observed structural
footprint — symbol count, declaration mix by kind, and representative entry
points.

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
literal symbol, and a full index rejects files that contain no declaration token
for their language at all (`package ` for Go; `(`, `{`, `#define`, `struct`,
`enum` or `typedef` for C).

**Phase 2 — routing-aware AST extraction.** The extension picks the engine:

* `.go` → `parser.ParseFile` with `ParseComments`. `*ast.FuncDecl` yields
  functions and methods (signature truncated at the opening brace);
  `*ast.TypeSpec` yields structs, interfaces, typedefs and type aliases, with
  embedded fields and struct tags summarised into the signature; `*ast.ValueSpec`
  yields constants and exported vars. Every symbol records its exact byte scope
  (`.Pos()`/`.End()`) and its `Doc` comment group, stripped of `//` boilerplate.
  The doc block is deliberately excluded from `source_code`.
* `.c` / `.h` → Tree-sitter. The walk harvests `function_definition`,
  `preproc_def`, `preproc_function_def`, `struct_specifier`, `union_specifier`,
  `enum_specifier`, `type_definition` and bare function prototypes — and nothing
  else, so occurrences inside string literals, `printf` calls or macro bodies
  can never be mistaken for a definition. Contiguous `comment` siblings directly
  above the node become the documentation (a blank line ends the block); the
  search climbs up to three ancestor levels so a comment above `typedef struct`
  still attaches to the specifier nested inside it.

Writes are delta-checked with a SHA-256 fingerprint over every field, so
unchanged symbols cause no disk I/O. After a repository is walked the indexer
holds the set of primary keys it just proved live and prunes every other
`sym:<repo>:` key together with its three index entries — so symbols deleted
from a file, and files deleted from the tree, both disappear in the same pass.
Pruning is scoped to the repository being indexed and batched 512 keys at a time.

---

## 8. Security posture

* No shell execution and no arbitrary file reads: the model receives parsed
  symbol records, plus — through `read_code_snippet` only — a line range capped
  at 200 lines from a file that has been proven to live inside the requested
  repository after symlink resolution.
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
* **Same symbol in two repositories:** keys are namespaced by repository, file
  and byte offset, so nothing is overwritten — every declaration is returned and
  the caller can disambiguate with `repo_name`.
* **Upgrading from Phase 1:** the `func:` / `idx:<repo>:` namespace is gone.
  Delete `DOTFS_CACHE_DB` once; the Phase 2 namespace rebuilds on next boot.
* **Cache reset:** stop the server and delete `DOTFS_CACHE_DB`.
* **Debugging:** `DOTFS_LOG_LEVEL=debug` adds per-file filter decisions and
  BadgerDB internals on stderr.

---

## 10. Tests

```bash
make test     # unit + integration tests
make race     # race detector
```

Coverage includes AST extraction of every symbol kind in both languages
(including the string-literal false-positive case, anonymous `typedef enum`
scope propagation, `iota` const blocks and struct tags), the typed key schema
with its three secondary indexes, cache delta/prune semantics, symbol
invalidation after a file is deleted, snippet clamping and path-traversal
rejection, live fallback search, all six MCP tools and the
`202`/`409`/`401`/`404` behaviour of the sync API.

`testdata/workspace/` holds a two-repository fixture (`packet-router-c`,
`auth-service-go`) that exercises the full taxonomy end to end.

---

## License

See [LICENSE](LICENSE).
