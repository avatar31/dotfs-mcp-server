package mcpserver

// CrossReference is the relational query surface backed by clangd and gopls.
// It is optional: when the LSP engine is disabled the four tools below are not
// advertised at all, so the model never calls something that cannot answer.
type CrossReference interface {}
