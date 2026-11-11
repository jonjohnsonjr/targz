package lsp

import (
	"bytes"
	"compress/flate"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jonjohnsonjr/targz/deflate"
)

// TestNeovimDefinitionWalk exercises the actual editor/LSP transport and
// generated-buffer lifecycle. It is opt-in because most CI images do not ship
// Neovim; run it with GZPLS_NVIM_TEST=1 go test ./deflate/lsp -run Neovim.
func TestNeovimDefinitionWalk(t *testing.T) {
	if os.Getenv("GZPLS_NVIM_TEST") == "" {
		t.Skip("set GZPLS_NVIM_TEST=1 to run the headless Neovim smoke test")
	}
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("Neovim is not installed")
	}
	temp := t.TempDir()

	input := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog; 0123456789\n"), 300)
	var compressed bytes.Buffer
	w, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(temp, "smoke.deflate")
	if err := os.WriteFile(source, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := DisassembleFileView(source, true, "trace")
	if err != nil {
		t.Fatal(err)
	}
	listingPath := filepath.Join(temp, "smoke.deflate.txt")
	if err := os.WriteFile(listingPath, listing, 0o600); err != nil {
		t.Fatal(err)
	}
	doc := openDocument(fileURI(listingPath), string(listing))
	if doc.loadErr != nil {
		t.Fatal(doc.loadErr)
	}
	var literal deflate.Op
	found := false
	for _, op := range doc.trace.Ops {
		if op.Kind == deflate.LiteralOp {
			literal, found = op, true
			break
		}
	}
	if !found {
		t.Fatal("fixture has no literal")
	}
	start := doc.tokens[literal.ID].Literal.Start

	server := filepath.Join(temp, "gzpls")
	build := exec.Command("go", "build", "-o", server, "../../cmd/gzpls")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gzpls: %v\n%s", err, output)
	}
	marker := filepath.Join(temp, "result")
	lua := fmt.Sprintf(`
local listing = %s
local server = %s
local marker = %s
vim.cmd("edit " .. vim.fn.fnameescape(listing))
local client_id = assert(vim.lsp.start({name = "gzpls-smoke", cmd = {server, "serve"}}))
assert(vim.wait(5000, function()
  local client = vim.lsp.get_client_by_id(client_id)
  return client and client.initialized
end), "LSP initialization timed out")
local client = assert(vim.lsp.get_client_by_id(client_id))
local position = {line = %d, character = %d}
local reached_rfc = false
for _ = 1, 8 do
  local params = {textDocument = {uri = vim.uri_from_bufnr(0)}, position = position}
  local response = assert(client:request_sync("textDocument/definition", params, 5000, 0))
  assert(not response.err, vim.inspect(response.err))
  assert(response.result and #response.result == 1, vim.inspect(response.result))
  local link = response.result[1]
  if string.match(link.targetUri, "/doc/rfc1951.txt$") then
    local path = vim.uri_to_fname(link.targetUri)
    vim.cmd("edit " .. vim.fn.fnameescape(path))
    vim.api.nvim_win_set_cursor(0, {link.targetSelectionRange.start.line + 1, link.targetSelectionRange.start.character})
    local selected = string.sub(vim.api.nvim_get_current_line(), link.targetSelectionRange.start.character + 1, link.targetSelectionRange["end"].character)
    assert(selected ~= "" and not string.match(selected, "^%%s+$"), "empty RFC selection")
    reached_rfc = true
    break
  end
  vim.cmd("edit " .. vim.fn.fnameescape(vim.uri_to_fname(link.targetUri)))
  assert(vim.lsp.buf_attach_client(0, client_id))
  position = link.targetSelectionRange.start
end
assert(reached_rfc, "definition walk did not reach RFC 1951")
vim.fn.writefile({"ok"}, marker)
client:stop(true)
vim.cmd("qa!")
`, strconv.Quote(listingPath), strconv.Quote(server), strconv.Quote(marker), start.Line, start.Character)
	script := filepath.Join(temp, "smoke.lua")
	if err := os.WriteFile(script, []byte(lua), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(nvim, "--headless", "-u", "NONE", "-l", script)
	command.Dir = filepath.Join("..", "..")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("headless Neovim: %v\n%s", err, output)
	}
	result, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("Neovim did not write result marker: %v\n%s", err, output)
	}
	if strings.TrimSpace(string(result)) != "ok" {
		t.Fatalf("Neovim result = %q, want ok", result)
	}
}
