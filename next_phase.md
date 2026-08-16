# Product Requirements Document: Phase 2 — Comprehensive Symbol & Custom Type Indexing

## 1. Executive Summary & Objective

Phase 1 established basic function-level AST extraction and sub-millisecond retrieval via BadgerDB. However, enterprise storage and distributed systems (such as NFS-Ganesha and Go microservices) rely heavily on preprocessor `#define` macros, struct function tables (v-tables), custom typedefs, interfaces, and constant enumerations (`iota` / `enum`).

The objective of **Phase 2** is to expand the static AST extraction pipeline to capture, index, and persist all non-function language constructs into BadgerDB. This enables the LLM agent to inspect memory layouts, serialization structs, error constants, and preprocessor definitions in constant time ($O(1)$) without invoking heavier compiler analysis or reading whole files.

---

## 2. Scope & Target Constructs

```
┌──────────────────────────────────────────────────────────────────────────┐
│                           Phase 2 Scope Matrix                           │
├────────────────────────────────────┬─────────────────────────────────────┤
│ C / C++ Language Constructs        │ Go Language Constructs              │
├────────────────────────────────────┼─────────────────────────────────────┤
│ • Preprocessor Macros (#define)    │ • Struct Declarations & Tagged Types│
│ • Macro Functions (#define X(y))   │ • Interface Specifications          │
│ • Struct & Union Definitions       │ • Type Aliases & Custom Type Decls  │
│ • Enum Specifiers & Enumerators    │ • Constant Blocks (const / iota)    │
│ • Typedefs & Function Pointer Types│ • Exported Global Variables (var)   │
│ • Header Function Prototypes (.h)  │                                     │
└────────────────────────────────────┴─────────────────────────────────────┘

```

---

## 3. Storage Layer: BadgerDB Schema & Key Design

To support multiple symbol types without key collisions, Phase 2 deprecates single-dimension function keys and establishes a unified, typed namespace.

### 3.1 Key Layout Specifications

| Key Type | Key Pattern | Value | Purpose |
| --- | --- | --- | --- |
| **Primary Record** | `sym:<repo>:<file_path>:<symbol_type>:<name>:<offset>` | Serialized JSON Payload | Authoritative storage of symbol body, docstring, and metadata. |
| **Global Name Index** | `idx:name:<name>:<repo>:<file_path>:<offset>` | *(empty)* | Fast point lookup by symbol name regardless of type. |
| **Type Filter Index** | `idx:type:<symbol_type>:<name>:<repo>:<file_path>:<offset>` | *(empty)* | Selective lookup (e.g., retrieve only structs or only macros). |
| **File Scoped Index** | `idx:file:<repo>:<file_path>:<offset>` | *(empty)* | Bulk retrieval or invalidation of symbols during file updates. |

*Note: All integer byte offsets (`<offset>`) MUST be formatted with 8-character zero-padding (`%08d`) to ensure correct lexicographical ordering in the LSM tree.*

### 3.2 Value Schema (JSON Payload)

```json
{
  "repo_name": "nfs-ganesha",
  "file_path": "src/include/fsal_types.h",
  "language": "c",
  "symbol_type": "struct",
  "name": "fsal_obj_handle",
  "parent_scope": "",
  "start_byte": 1420,
  "end_byte": 2890,
  "start_line": 64,
  "end_line": 120,
  "documentation": "Core object handle representing a file or directory in FSAL.",
  "signature": "struct fsal_obj_handle",
  "source_code": "struct fsal_obj_handle {\n    struct fsal_filesystem *fs;\n    object_file_type_t obj_type;\n    fsal_vfs_context_t *ctx;\n};"
}

```

#### Allowed Values for `symbol_type`:

* `"function"` | `"method"` | `"struct"` | `"interface"` | `"macro"` | `"macro_function"` | `"enum"` | `"typedef"` | `"constant"` | `"type_alias"`

---

## 4. AST Extraction Engine Specifications

### 4.1 Go Parser Engine (`go/parser` + `go/ast`)

The Go parser must inspect all top-level declarations (`file.Decls`) using type switches over both `*ast.FuncDecl` and `*ast.GenDecl`:

1. **`token.TYPE` (`*ast.TypeSpec`):**
* If `TypeSpec.Type` is `*ast.StructType`: Record as `symbol_type = "struct"`. Extract embedded fields and JSON/YAML struct tags.
* If `TypeSpec.Type` is `*ast.InterfaceType`: Record as `symbol_type = "interface"`. Extract method signatures and embedded interface names.
* If `TypeSpec.Assign != 0`: Record as `symbol_type = "type_alias"`.


2. **`token.CONST` (`*ast.ValueSpec`):**
* Extract constant identifier names.
* Associate attached docstrings and extract the full `const (...)` declaration block source for contextual understanding of grouped `iota` enumerations.


3. **Comment Attachment:**
* Extract doc comments directly from `GenDecl.Doc` or individual `ValueSpec.Doc`.



### 4.2 C Parser Engine (`go-tree-sitter` with C Grammar)

Tree-sitter must execute explicit Tree-sitter query captures or recursive AST walks covering both `.c` source files and `.h` header files:

```scheme
;; Tree-sitter Query Patterns for Phase 2 C Syntax

;; 1. Preprocessor Object-like Macros
(preproc_def
  name: (identifier) @macro.name
  value: (_)? @macro.value) @macro.def

;; 2. Preprocessor Function-like Macros
(preproc_function_def
  name: (identifier) @macro_fn.name
  parameters: (preproc_params)
  value: (_)? @macro_fn.value) @macro_fn.def

;; 3. Struct & Union Definitions
(struct_specifier
  name: (type_identifier) @struct.name
  body: (field_declaration_list)) @struct.def

(union_specifier
  name: (type_identifier) @union.name
  body: (field_declaration_list)) @union.def

;; 4. Typedefs
(type_definition
  type: (_)
  declarator: (type_identifier) @typedef.name) @typedef.def

;; 5. Enum Specifiers & Constants
(enum_specifier
  name: (type_identifier)? @enum.name
  body: (enumerator_list
    (enumerator
      name: (identifier) @enum_const.name)*)) @enum.def

;; 6. Function Prototypes (Header Declarations)
(declaration
  type: (_)
  declarator: (function_declarator
    declarator: (identifier) @func_proto.name)) @func_proto.def

```

---

## 5. MCP Tool Framework Additions

Phase 2 enhances the MCP server by exposing specialized retrieval tools alongside the existing function search:

### Tool 1: `lookup_symbol` (Generalized)

* **Description:** Fast $O(1)$ retrieval of any symbol (function, struct, macro, constant, interface) across all repositories.
* **Input Parameters:**
* `name` (string, required): Exact name or substring prefix of the symbol.
* `repo_name` (string, optional): Restrict search to a specific repository.
* `symbol_type` (string, optional): Filter by type (`"struct"`, `"macro"`, `"interface"`, `"function"`).


* **Output:** JSON array of matching records with source snippets and documentation.

### Tool 2: `get_type_definition`

* **Description:** Retrieves the exact declaration layout of a struct, union, interface, or typedef.
* **Input Parameters:**
* `type_name` (string, required): Name of the struct, interface, or typedef (e.g., `"fsal_obj_handle"` or `"SessionManager"`).
* `repo_name` (string, optional): Filter by target repository.


* **Output:** The full type definition block, including field comments, alignments, and struct tags.

### Tool 3: `lookup_macro_or_const`

* **Description:** Resolves preprocessor `#define` values, error codes, and Go `const` definitions.
* **Input Parameters:**
* `name` (string, required): Identifier name (e.g., `"ERR_FSAL_NO_QUOTA"` or `"StatusPending"`).


* **Output:** Macro/const replacement value, definition location, and surrounding documentation.

### Tool 4: `read_code_snippet`

* **Description:** Reads contiguous lines of code around an identified symbol for context verification.
* **Input Parameters:**
* `repo_name` (string, required): Repository identifier.
* `file_path` (string, required): Relative file path.
* `start_line` (integer, required): Beginning line number.
* `end_line` (integer, required): Ending line number (capped at 200 lines per call).


* **Output:** Formatted raw text snippet with line numbering.

---

## 6. Non-Functional & Operational Requirements

1. **Incremental Invalidation:** When the REST endpoint `/api/v1/:repo_name/update` is triggered, the worker must scan all existing keys with prefix `sym:<repo_name>:` and remove obsolete records before writing newly parsed declarations.
2. **Memory Footprint:** AST tree objects must be allocated and garbage-collected per file; the server MUST NOT retain in-memory AST pointer graphs for the entire codebase simultaneously.
3. **Standard Stream Isolation:** All logs generated by tree-sitter bindings and BadgerDB routines MUST strictly target `os.Stderr`. `os.Stdout` remains dedicated to the MCP JSON-RPC protocol.

---

---

# Product Requirements Document: Phase 3 — On-Demand Deep Cross-Reference & LSP Engine

## 1. Executive Summary & Objective

Phases 1 and 2 deliver high-speed, token-efficient point lookups for all static declarations. However, static ASTs cannot resolve dynamic cross-file relationships—such as function caller hierarchies, dynamic interface implementations in Go, C function pointer tables (`fsal_ops`), or macro-expanded type references.

The objective of **Phase 3** is to integrate **on-demand Language Server Protocol (LSP)** daemons (`clangd` for C, `gopls` for Go) directly into the Go MCP server. By managing these language servers as background worker subprocesses, the MCP server can execute deep relational queries deterministically without needing to store or maintain brittle multi-gigabyte call-graph databases.

---

## 2. Architecture & Subprocess Supervision Model

```
 ┌─────────────────────────────────────────────────────────────┐
 │                  AI Agent (Claude / Cursor)                 │
 └──────────────────────────────┬──────────────────────────────┘
                                │ MCP stdio (JSON-RPC)
                                ▼
 ┌─────────────────────────────────────────────────────────────┐
 │              Your Go MCP Server Orchestrator                │
 │  ┌───────────────────────────────────────────────────────┐  │
 │  │ Phase 1/2: BadgerDB ($O(1)$ Fast Symbol Lookups)      │  │
 │  └───────────────────────────────────────────────────────┘  │
 │  ┌───────────────────────────────────────────────────────┐  │
 │  │ Phase 3: LSP Subprocess Manager (Lifecycle & IPC)     │  │
 │  └───────┬───────────────────────────────────────┬───────┘  │
 └──────────┼───────────────────────────────────────┼──────────┘
            │ stdio (LSP JSON-RPC)                  │ stdio (LSP JSON-RPC)
            ▼                                       ▼
 ┌───────────────────────────┐           ┌───────────────────────────┐
 │      clangd Daemon        │           │       gopls Daemon        │
 │  (C / NFS-Ganesha Engine) │           │    (Go Services Engine)   │
 │  Reads: compile_commands  │           │    Reads: go.mod / cache  │
 └───────────────────────────┘           └───────────────────────────┘

```

### 2.1 Subprocess Lifecycle Rules

1. **Lazy Initialization:** `clangd` and `gopls` subprocesses are not spawned at server startup. They are initialized only upon the first MCP tool call requiring language-specific cross-referencing.
2. **IPC Communication:** The Go MCP server interacts with child daemons over bidirectional standard streams (`stdin`/`stdout`) using the standard Language Server Protocol (LSP) framing:
```
Content-Length: <byte_count>\r\n
\r\n
<JSON-RPC-2.0-Payload>

```


3. **Process Supervision & Teardown:**
* If a child language server terminates unexpectedly, the MCP server must restart it on the next request.
* On MCP server shutdown, all child process trees must receive `SIGTERM` followed by a 2-second fallback `SIGKILL`.



---

## 3. LSP Capabilities Exposed as MCP Tools

Phase 3 introduces four relational MCP tools designed for deep root cause analysis:

```
┌──────────────────────────────────────────────────────────────────────────┐
│                           Phase 3 Tools Matrix                           │
├────────────────────────────────┬─────────────────────────────────────────┤
│ Tool Name                      │ Core Functional Capability              │
├────────────────────────────────┼─────────────────────────────────────────┤
│ find_references                │ Locates all invocation sites and usages │
│ get_call_hierarchy             │ Computes incoming / outgoing call trees │
│ find_interface_implementations │ Maps Go interfaces to concrete structs  │
│ get_type_hierarchy             │ Resolves C struct/type inheritance & def│
└────────────────────────────────┴─────────────────────────────────────────┘

```

### Tool 1: `find_references`

* **Description:** Resolves all call-sites, usages, and references of a symbol across files and repositories using compiler type graphs.
* **Input Parameters:**
* `repo_name` (string, required): Target repository.
* `file_path` (string, required): File path containing the definition.
* `line` (integer, required): 1-based line number of the symbol.
* `character` (integer, required): 1-based column offset.
* `include_declaration` (boolean, default false): Whether to include the declaration itself.


* **Backend LSP Call:** `textDocument/references`

### Tool 2: `get_call_hierarchy`

* **Description:** Identifies who calls a specific function (incoming) or what functions are called by it (outgoing).
* **Input Parameters:**
* `repo_name` (string, required): Target repository.
* `file_path` (string, required): File path containing the function.
* `line` (integer, required): Line number inside the function.
* `character` (integer, required): Column index.
* `direction` (string, required): `"incoming"` (callers) or `"outgoing"` (callees).


* **Backend LSP Calls:**
1. `textDocument/prepareCallHierarchy`
2. `callHierarchy/incomingCalls` OR `callHierarchy/outgoingCalls`



### Tool 3: `find_interface_implementations`

* **Description:** Identifies all concrete Go structs that satisfy a specified interface or C structs matching a generic v-table signature.
* **Input Parameters:**
* `repo_name` (string, required): Target repository.
* `file_path` (string, required): Location of the interface definition.
* `line` (integer, required): Line number of the interface type name.
* `character` (integer, required): Column index.


* **Backend LSP Call:** `textDocument/implementation`

---

## 4. Context Optimization & Token Guardrails

LSP responses contain raw AST coordinates that can consume thousands of tokens if passed directly to the LLM. The MCP server must filter and condense raw LSP responses before sending them back.

### 4.1 Token Truncation & Compaction Pipeline

```
 Raw LSP Response (100s of URI/Ranges)
                 │
                 ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ 1. Dedup & Limit: Cap results at maximum 20 references      │
 └──────────────────────────────┬──────────────────────────────┘
                                │
                                ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ 2. Snippet Extraction: Read target file lines [Line-1:Line+1]│
 └──────────────────────────────┬──────────────────────────────┘
                                │
                                ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ 3. Minified JSON Output: Return compact payload to LLM      │
 └─────────────────────────────────────────────────────────────┘

```

### 4.2 Compact JSON Output Example

```json
{
  "symbol": "fsal_open",
  "direction": "incoming",
  "total_callers": 2,
  "calls": [
    {
      "caller_name": "nfs4_op_open",
      "repo": "nfs-ganesha",
      "file_path": "src/Protocols/NFS/nfs4_op_open.c",
      "line": 142,
      "snippet": "status = obj->fsal->fsal_open(obj, &open_flags, &fsal_cookie);"
    },
    {
      "caller_name": "vfs_create",
      "repo": "nfs-ganesha",
      "file_path": "src/FSAL/FSAL_VFS/file.c",
      "line": 88,
      "snippet": "return sub_fsal->fsal_open(sub_obj, flags, cookie);"
    }
  ]
}

```

---

## 5. Prerequisites, Error Handling & Fallbacks

### 5.1 Environment Prerequisites

* **C Repositories:** Requires `compile_commands.json` located at the repository root or pointed to via the `compile-commands-dir` flag in `clangd`.
* **Go Repositories:** Requires standard `go.mod` files; `gopls` must be executable in `$PATH`.

### 5.2 Error Handling & Fallback Matrix

| Failure Mode | Detection Scenario | Fallback / Recovery Strategy |
| --- | --- | --- |
| **Missing `compile_commands.json**` | `clangd` cannot resolve header dependencies. | Return clear error advising user to run `cmake -DCMAKE_EXPORT_COMPILE_COMMANDS=ON`. Fall back to BadgerDB symbol lookups. |
| **LSP Daemon Timeout** | JSON-RPC call takes longer than 5.0 seconds. | Abort query context, return `HTTP 504 / Tool Execution Timeout`, and keep MCP server responsive. |
| **Unresolved Symbol** | LSP returns empty reference array. | Advise LLM agent to fall back to `lookup_symbol` via BadgerDB text search. |
| **Daemon Crash (Panic/OOM)** | Subprocess pipe returns `EOF`. | Log crash trace to `os.Stderr`, invalidate client reference, and spawn a fresh daemon instance on the next request. |

---

## 6. End-to-End Hybrid System SLA

| Query Type | Primary Engine | Target Latency SLA | Token Cost Overhead |
| --- | --- | --- | --- |
| **Exact Function / Struct Lookup** | Phase 1/2 (BadgerDB) | $< 5\text{ ms}$ | $< 300\text{ tokens}$ |
| **Macro / Constant Evaluation** | Phase 2 (BadgerDB) | $< 5\text{ ms}$ | $< 100\text{ tokens}$ |
| **Cross-File Caller Hierarchy** | Phase 3 (`clangd` / `gopls`) | $< 1.5\text{ s}$ | $< 800\text{ tokens}$ |
| **Interface Implementations** | Phase 3 (`gopls`) | $< 1.0\text{ s}$ | $< 500\text{ tokens}$ |
