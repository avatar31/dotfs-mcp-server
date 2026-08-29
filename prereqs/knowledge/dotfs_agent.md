# Multi-Repo Engineering & Agent Orchestration Rules

Read [WORKSPACE](WORKSPACE.md) file to understand what are these multi-repos and how they will work together.

## 1. Engineering Methodology (Superpowers)
When tasked with debugging, bug fixes, or feature development:
- **Systematic Debugging:** For any bug/incident, follow the workflow defined in `.superpowers/skills/systematic-debugging/SKILL.md`. Form a hypothesis and trace the root cause before editing code.
- **TDD Requirement:** Follow `.superpowers/skills/test-driven-development/SKILL.md`. Write a minimal reproducing test (Red) in C (assert harness) or Go (`testing` package), implement the fix (Green), and verify.

## 2. High-Level Architecture (Graphify)
- Consult `graphify-out/GRAPH_REPORT.md` or `graphify-out/graph.json` to understand subsystem boundaries, design RFCs, and dependency blast radius before making multi-service changes.

## 3. Surgical Code Intelligence (code-intel MCP Server)
- **Symbol Retrieval ($O(1)$):**
    1. `global_codebase_search` for symbol search across all repos.
    2. `lookup_symbol` for symbol search within a single repo.
    3. `get_type_definition` for type definition retrieval.
    4. `lookup_macro_or_const` for macro or constant retrieval.
    5. `read_code_snippet` for code snippet retrieval.
    6. `list_repo_capabilities` for repo capabilities retrieval.
- **Compiler Cross-References:** via live `clangd` (C) and `gopls` (Go)
    1. `find_references` for symbol references.
    2. `get_call_hierarchy` for call hierarchy.
    3. `find_interface_implementations` for interface implementations.
    4. `get_type_hierarchy` for type hierarchy.
