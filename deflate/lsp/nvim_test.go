package lsp

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
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
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	integration := filepath.Join(repo, "contrib", "gzpls.lua")
	marker := filepath.Join(temp, "result")
	lua := fmt.Sprintf(`
local listing = %s
local server = %s
local repo = %s
local marker = %s
dofile(%s).setup({cmd = {server}, root_dir = repo})
vim.cmd("edit " .. vim.fn.fnameescape(listing))
local client_id = assert(vim.lsp.start({name = "gzpls-smoke", cmd = {server, "serve"}}))
assert(vim.wait(5000, function()
  local client = vim.lsp.get_client_by_id(client_id)
  return client and client.initialized
end), "LSP initialization timed out")
local client = assert(vim.lsp.get_client_by_id(client_id))
local position = {line = %d, character = %d}
local reached_rfc = false
local reached_lengths = false
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
  vim.api.nvim_win_set_cursor(0, {position.line + 1, position.character})
  if string.match(link.targetUri, "%%.gzsym$") then
    assert(vim.bo.filetype == "gzpls-symbols", "generated symbol filetype = " .. vim.bo.filetype)
    assert(vim.wo.number, "generated symbol gutter has no line numbers")
    assert(not vim.wo.relativenumber, "generated symbol gutter uses relative numbers")
    assert(vim.wo.statuscolumn == "%%=%%{v:lnum - 1} ", "generated symbol statuscolumn = " .. vim.wo.statuscolumn)
  end
  if string.match(link.targetUri, "litlen%%-lengths%%.gzsym$") then
    assert(vim.fn.line(".") - 1 == position.line, "litlen-lengths semantic line is not zero-based")
    local row = vim.api.nvim_get_current_line()
    assert(string.match(row, "^length %%d+$"),
      "broken litlen-lengths row: " .. row)
    reached_lengths = true
  end
end
assert(reached_rfc, "definition walk did not reach RFC 1951")
assert(reached_lengths, "definition walk did not reach litlen-lengths")
vim.fn.writefile({"ok"}, marker)
for _, running in ipairs(vim.lsp.get_clients()) do
  running:stop(true)
end
vim.cmd("qa!")
`, strconv.Quote(listingPath), strconv.Quote(server), strconv.Quote(repo), strconv.Quote(marker), strconv.Quote(integration), start.Line, start.Character)
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

// TestNeovimTarDirectoryNavigation covers the actual user path which the
// protocol-only tests used to miss: open a compressed archive directly, let the
// repo config render and attach it, and use Neovim's definition handler to step
// into a directory.
func TestNeovimTarDirectoryNavigation(t *testing.T) {
	if os.Getenv("GZPLS_NVIM_TEST") == "" {
		t.Skip("set GZPLS_NVIM_TEST=1 to run the headless Neovim smoke test")
	}
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("Neovim is not installed")
	}
	temp := t.TempDir()
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(repo, "tarfs", "testdata", "gsip.tar.gz")
	plainSource := filepath.Join(temp, "hello.txt.gz")
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte("hello from an ordinary gzip\nsecond line\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainSource, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	integration := filepath.Join(repo, "contrib", "gzpls.lua")
	server := filepath.Join(temp, "gzpls")
	build := exec.Command("go", "build", "-o", server, "./cmd/gzpls")
	build.Dir = repo
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gzpls: %v\n%s", err, output)
	}

	marker := filepath.Join(temp, "result")
	lua := fmt.Sprintf(`
local source = %s
local plain_source = %s
local server = %s
local repo = %s
local marker = %s
dofile(%s).setup({cmd = {server}, root_dir = repo})
vim.cmd("edit " .. vim.fn.fnameescape(source))
assert(vim.api.nvim_buf_get_lines(0, 0, 1, false)[1] == "gzpls", "archive did not show the immediate splash")
assert(vim.wait(5000, function()
  return #vim.lsp.get_clients({name = "gzpls", bufnr = 0}) == 1
end), "gzpls did not attach to archive")
local line = vim.fn.search("gsip/$", "W")
assert(line > 0, "archive root has no gsip directory")
local column = assert(string.find(vim.api.nvim_get_current_line(), "gsip/", 1, true)) - 1
vim.api.nvim_win_set_cursor(0, {line, column})
assert(vim.fn.maparg("gd", "n") ~= "", "gzpls did not install gd mapping")
vim.cmd("normal gd")
assert(vim.wait(5000, function()
  return string.match(vim.api.nvim_buf_get_name(0), "/%%.gzpls/tree/gsip%%.deflate$") ~= nil
end), "definition did not enter gsip directory: " .. vim.api.nvim_buf_get_name(0))
assert(vim.wait(5000, function()
  return #vim.lsp.get_clients({name = "gzpls", bufnr = 0}) == 1
end), "gzpls did not attach to generated directory")
assert(vim.fn.search("gsip.go", "nw") > 0, "generated directory has no gsip.go entry")
vim.cmd("edit " .. vim.fn.fnameescape(plain_source))
assert(vim.wait(5000, function()
  return #vim.lsp.get_clients({name = "gzpls", bufnr = 0}) == 1
end), "gzpls did not attach to ordinary gzip")
assert(vim.api.nvim_buf_get_lines(0, 0, 2, false)[1] == "hello from an ordinary gzip", "ordinary gzip did not open as plaintext")
vim.api.nvim_win_set_cursor(0, {1, 1})
assert(vim.fn.maparg("gri", "n") ~= "", "gzpls did not install gri mapping")
vim.cmd("normal gri")
assert(vim.wait(5000, function()
  return string.match(vim.api.nvim_buf_get_name(0), "/%%.gzpls/trace%%.deflate$") ~= nil
end), "implementation did not enter the trace: " .. vim.api.nvim_buf_get_name(0))
vim.fn.writefile({"ok"}, marker)
for _, client in ipairs(vim.lsp.get_clients({name = "gzpls"})) do
  client:stop(true)
end
vim.cmd("qa!")
`, strconv.Quote(source), strconv.Quote(plainSource), strconv.Quote(server), strconv.Quote(repo), strconv.Quote(marker), strconv.Quote(integration))
	script := filepath.Join(temp, "tar.lua")
	if err := os.WriteFile(script, []byte(lua), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(nvim, "--headless", "-u", "NONE", "-l", script)
	command.Dir = repo
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
