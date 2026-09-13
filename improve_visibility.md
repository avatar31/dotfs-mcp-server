To ensure that AI assistants automatically prioritize your `dotfs-mcp-server` tools (like `lookup_symbol`, `get_type_definition`, and `find_references`) over generic file searches, here are the most effective improvements you can make:

---

### 1. Add Workflow Rules to `.github/copilot-instructions.md`

Add explicit tool-selection instructions into your workspace's `.github/copilot-instructions.md` file. AI assistants read this on every session start.

Add a section like this:

```markdown
## Code Search & Navigation Guidelines

- **Always use `mcp_dotfs-mcp-ser_lookup_symbol` first** when searching for functions, structs, interfaces, enums, or macro definitions across `dotfs`, `omashu`, `nfs-ganesha`, and `samba`.
- Use `mcp_dotfs-mcp-ser_get_type_definition` when analyzing struct memory layout, fields, or v-tables.
- Use `mcp_dotfs-mcp-ser_find_references` to track down call sites instead of text search.
- Only fall back to workspace text search (`grep_search` / `read_file`) for non-code files (like `CMakeLists.txt`, `wscript`, or markdown docs) or un-indexed text string matching.
```

---

### 2. Strengthen MCP Tool Descriptions in your Server Code

The model decides which tool to call based on the `description` string returned by your MCP server tool definitions. If the description specifies *when* to use it, the model will pick it proactively.

**Example for `lookup_symbol`:**
> *"PRIMARY TOOL FOR CODE SEARCH. Constant-time retrieval of any indexed C/Go declaration (struct, function, interface, macro, enum). ALWAYS prefer this over workspace grep/file reading when searching for code symbols, types, or function definitions."*

**Example for `find_references`:**
> *"PRIMARY TOOL FOR CALL SITES. Uses compiler AST (clangd/gopls) to resolve all call sites and usages of a symbol. Use this instead of text search to find who calls a function or references a struct."*

---

### 3. Ensure Index Coverage & Warm Startup

For instant symbol access when customer issues arise:

1. **Pre-build AST Cache**: Ensure `dotfs-mcp-server` generates and saves its AST cache (`clangd` / `gopls` indices) ahead of time rather than building on-demand when a user asks a question.
2. **Support All Workspace Repos**: Confirm all 5 workspace repositories (`dotfs`, `omashu`, `nfs-ganesha`, `samba`, `halmidi`) are indexed and listed in the server's repository registry.
