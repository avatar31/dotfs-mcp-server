# Product Requirements Document: Multi-Repo C & Go Codebase MCP Server

## Product Requirements Document (PRD)

## 1. System Overview
The goal of this system is to build a high-performance, secure, and context-aware Model Context Protocol (MCP) server written in Go. The system will interface an LLM client (such as Claude Desktop or Cursor) with complex, multi-repository microservices written in both C and Go (Golang). Rather than forcing the LLM to read massive, unparsed text files or giving it dangerous raw shell access, this system indexes the codebase’s structural architecture locally using targeted static parsers and caches it in an embedded key-value database for real-time, low-latency surgical lookups across languages. Additionally, the server exposes a dedicated REST API endpoint allowing human operators or continuous integration hooks to manually trigger repository-specific parsing cycles dynamically.

## 2. Component Architecture & Responsibilities
The system is built on a clear separation of concerns across four major components:

| Component | Architecture Role | Core Responsibility |
|---|---|---|
| LLM (Large Language Model) | Cloud-Based Detective | High-level reasoning, intent parsing, step-by-step troubleshooting, and orchestration of structural tool calls based on multi-language error logs. Cannot access the local machine directly. |
| MCP Server (Go Binary) | Native Secure Bridge | Exposes specific API endpoints ("tools") to the LLM via standard Input/Output (stdio) transport, while concurrently running an HTTP router providing a management REST API for repository indexing control. Acts as a gatekeeper to control the blast radius, filtering and formatting local codebase context into clean tokens. |
| Dual AST Parser Layer | Structural Blueprint Reader | Statically parses raw code into an Abstract Syntax Tree (AST). It uses Tree-sitter for C files and Go's native standard library packages (go/parser, go/ast) for Go files to extract structural semantic blocks (functions, structs, docstrings). |
| Caching Layer (BadgerDB) | High-Speed Storage | Local embedded key-value database running inside the Go runtime process. Persists fully-parsed multi-language AST information, repository mapping, and semantic context between server reboots for microsecond execution. |

## 3. Core Technical Requirements## 3.1 Codebase Parsing & AST Isolation (Dual Engine Architecture)
The parser layer must inspect file extensions and dynamically route files to the appropriate AST compiler:

* C Parser Engine (.c, .h files):
* Must utilize Tree-sitter Go bindings (go-tree-sitter) linked with the C-language grammar.
   * Must explicitly locate nodes typed as function_definition and isolate their exact byte scope.
   * Must capture contiguous comment nodes immediately preceding a function_definition node.
* Go Parser Engine (.go files):
* Must utilize Go's built-in standard library packages: go/token, go/parser, and go/ast.
   * Must inspect the AST looking specifically for *ast.FuncDecl (Function Declaration) nodes.
   * Must extract the underlying node source via go/printer or direct slice indexing of the source file using the node's .Pos() and .End() markers.
   * Must capture the attached documentation group via the node's built-in Doc field (*ast.CommentGroup).

## 3.2 Caching Requirements (BadgerDB Data Layout)

* Storage Type: Embedded, LSM tree-based key-value store running on disk locally (./mcp_cache_db).
* Schema Design:
* Key Format: func:<function_name> (e.g., func:process_payment)
   * Value Schema: Structured JSON metadata blob updated to include a mandatory language classification tag.
* JSON Serialization Protocol:

{
  "repo_name": "string",
  "file_path": "string",
  "language": "string", 
  "documentation": "string",
  "source_code": "string"
}

(The language field must explicitly accept either "c" or "go" to allow the LLM client to apply proper syntax-specific markdown wrappers and linting rules).
------------------------------
## 4. Algorithmic Approach: "Filter-Then-Parse"
To handle a vast ecosystem of multi-language microservices without performance degradation, the MCP server must execute an explicit Two-Phase Caching & Search Algorithm.

[User Crash Report] ➡️ [Phase 1: Language-Agnostic Scanner] ➡️ (Match?) ➡️ [Phase 2: Routing-Aware AST Extraction]

## Phase 1: Fast String Scanner (The Filter)

* Action: When an indexing check or real-time fallback runs, the system reads files into memory as byte arrays.
* Execution: It runs a language-agnostic substring verification via Go's built-in, highly optimized bytes.Contains().
* Condition: If the literal function name token is absent from the file text, the file is immediately skipped, bypassing heavy AST processing for both languages.

## Phase 2: Routing-Aware AST Parser (The Precision Extraction)

* Action: If Phase 1 returns true, the file path extension determines the parser route.
* Execution (Go): The file bytes are passed to parser.ParseFile(). The system inspects function declaration nodes to verify if the match represents a real top-level function or method block, discarding partial matches or variable re-assignments.
* Execution (C): The file bytes are passed to Tree-sitter to build the syntax tree. The system walks the nodes to ensure instances of the term appearing inside plain string prints are filtered out.

## 5. Multi-Repository Routing Requirements
The system must seamlessly index and differentiate between distinct microservices developed across divergent language ecosystems.
## 5.1 Global Ecosystem Mapping

* The indexing loop must crawl a shared parent directory containing separate repository subdirectories (e.g., auth-service-go, packet-router-c).
* During iteration, the subdirectory name must be dynamically assigned to the repo_name field of the BadgerDB JSON record.

## 5.2 Required LLM Capabilities (Tools Framework)
The Go binary must implement and expose the following two mandatory tools under the MCP toolkit:

   1. global_codebase_search(target_function_name)
   * Input: string (The function name requested by the LLM detective).
      * Process: Constant-time O(1) lookup inside the BadgerDB key-value store.
      * Output: Returns the serialized JSON block string detailing the code body, specific file system path, the programming language runtime environment, and the definitive owner repository.
   2. list_repo_capabilities(repo_name)
   * Input: string (The service name, e.g., "auth-service-go").
      * Process: Evaluates a structural programmatic matrix mapping repositories to their operational profiles.
      * Output: A high-level description outlining what semantic business features that microservice implements and its respective engineering language stack.
   
## 5.3 On-Demand Sync API & Concurrency Control
To enable intentional, manual, or webhook-driven caching updates without requiring full server restarts or continuous disk-watching cycles, the Go binary must spin up a lightweight concurrent HTTP server.

* Endpoint Protocol: POST /api/v1/:repo_name/update
* Behavior:
* The route parameter :repo_name maps to a specific workspace directory matching the target repository name.
   * Upon receiving a valid request, the system spawns an isolated background worker thread to run the Two-Phase "Filter-Then-Parse" algorithm exclusively for files within that target directory.
   * Existing records in BadgerDB assigned to that repository are updated atomically or overwritten upon verification of a structural delta, ensuring zero disruption to active concurrent tool reads from the LLM client.
* Resource Contention & Safety Control:
* To prevent redundant processing, local CPU throttling, and SSD input/output degradation from simultaneous requests, the system must enforce strict state tracking on active parsing routines.
   * The HTTP handler must utilize an in-memory execution map guarded by a mutual exclusion lock (sync.Mutex) to monitor ongoing tasks.
   * If a request attempts to trigger an update cycle for a :repo_name that is already actively being processed by a background worker, the server must instantly reject the operation and return an HTTP 409 Conflict status code.
   * The execution flag must be automatically cleared using deferred teardown routines (defer) only when the parsing routine completes its BadgerDB write flush successfully.

## 6. Code Documentation Impact Requirements
Code documentation differences impact multi-language troubleshooting strategies:

* Go-Specific Tooling Leverage: Go idiomatic development relies heavily on uniform block comments directly over functions. Because the Go native parser automatically couples these comments into the *ast.FuncDecl.Doc pointer field, the indexing loop must extract this text completely boilerplate-free, enhancing the semantic search layer of the BadgerDB cache.
* Cross-Language Intent Diagnostics: When checking for microservice synchronization failures (e.g., a Go backend communicating with a C-based system process), the LLM must compare human-written documentation parameters across both languages to catch serialization errors, interface discrepancies, or macro mapping mismatches.
