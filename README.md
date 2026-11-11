# targz

[![GoDoc](https://godoc.org/github.com/jonjohnsonjr/targz?status.svg)](https://pkg.go.dev/github.com/jonjohnsonjr/targz)

This module is a collection of packages relating to gzipped tarballs.

Taken together, they allow for fast random access of a filesystem within a gzipped tarball.

This is primarily a proof of concept of the interfaces and how they compose:

<img src="./doc/viz.svg">

## Packages

### deflate debugger

`deflate` is an experimental, bit-exact DEFLATE trace engine. Unlike a normal
decompressor, its primary output is a queryable instruction stream: every
literal, stored run, and match records both its compressed-bit span and its
uncompressed output range. Match provenance can be followed through
overlapping back-references to the literal or stored byte which originated a
value.

Print a trace and follow one output byte back to its origin:

```console
go run ./cmd/deflate-debug -origin 100 archive.tar.gz
```

Or launch the interactive debugger:

```console
go run ./cmd/deflate-debug-web archive.tar.gz
```

The web debugger provides block navigation, a paged instruction trace,
clickable output bytes, normalized match seeds, origin chains, and reverse-use
queries. It binds to a random localhost port by default.

#### `gzpls`: IDE-grade language intelligence for gzip

`gzpls` is a small LSP 3.17 server which maps DEFLATE provenance onto an IDE's
call hierarchy. A match "calls" the earlier instructions which produced its
normalized source seed. Its incoming calls are later matches which consume the
bytes it produced. Literals are constants and matches are functions, obviously.

First produce the textual document the server operates on:

```console
go run ./cmd/gzpls disassemble archive.tar.gz > archive.tar.gz.deflate
```

Then configure an LSP client to associate `*.deflate` with this command:

```console
go run ./cmd/gzpls serve
```

The generated document starts with the absolute path of its compressed source.
The server reparses that source when the document opens, so it can answer
`textDocument/prepareCallHierarchy`, `callHierarchy/incomingCalls`,
`callHierarchy/outgoingCalls`, and hover requests using the bit-exact trace. It
also maps the editor's most important navigation commands onto provenance:

- **Go to Definition** jumps from a match to the operations which immediately
  produce its normalized source seed. Definition results are `LocationLink`s:
  the origin selects the match's exact `seed[...]` token, the destination
  selects the producer's `out[...]` token, and the complete instruction remains
  available as target context.
- **Find References** finds later matches which directly consume this
  operation's output.
- **Go to Implementation** follows the dependency graph through matches to the
  literal or stored operations which ultimately introduced the bytes. These
  results use the same token-precise links.
- **Rename** is available on literal tokens. Entering a replacement byte follows
  every transitive copy, atomically rewrites the compressed source, recomputes
  affected gzip trailers, and returns a workspace edit containing the refreshed
  disassembly. Exact gzip headers and unaffected members are retained byte for
  byte; affected members receive fresh DEFLATE bodies, so their block layout may
  change. `gzpls/renameImpact` remains a non-mutating structured preview for
  editor integrations.

Raw streams can be generated with `disassemble -raw`.

The default `auto` view recognizes decoded tar streams by parsing their headers
and checksums rather than trusting the filename. Tarballs open as a compact
archive manifest with type, mode, exact size, modification time, decoded data
range, path, link target, hover metadata, and document-outline symbols. Use
`disassemble -view trace` to force the instruction listing, or `-view tar` to
require a valid tar archive.

When opening `.tar.gz` directly in Neovim, disable its built-in gzip and tar
read handlers before registering the `gzpls` `BufReadCmd`:

```lua
vim.g.loaded_gzip = 1
vim.g.loaded_tar = 1
vim.g.loaded_tarPlugin = 1
```

The call hierarchy deliberately stays compact for overlapping copies. Instead
of constructing a thousand-node chain for `match len=1000 dist=1`, the match
has one outgoing edge to its one-byte seed and reports the repetition count in
the hierarchy detail.

Dynamic Huffman literals also have a navigable encoding ancestry. Repeated
**Go to Definition** walks from the literal/length table entry, to the expanded
code-length assignment, to the compact code-length program instruction which
produced it, to the code-length Huffman table, to its raw three-bit HCLEN field.
Each document is a semantic address space: the literal/length and distance
assignment documents use the affected data symbol as their line, the compact
program uses its instruction index, the code-length table uses its decoded
symbol from 0 through 18, and the HCLEN document uses the field's transmission
index in the RFC-mandated permutation. Repeat codes 16–18 occur once in the
compact program; finding references on that instruction returns every expanded
slot it assigns. The final definition opens the vendored
[`doc/rfc1951.txt`](doc/rfc1951.txt) at the governing table in §3.2.7.
Fixed-Huffman literals take the shorter path from their generated table entry
to the appropriate row of the §3.2.6 table.

Match operands preserve the boundary between the Huffman symbol and its extra
bits as `symbol·extra`. Go to Definition before the dot follows the Huffman
ancestry described above; Go to Definition after the dot jumps directly to
that symbol's `Extra Bits` cell in the length or distance table in §3.2.5.
This also applies to fixed-Huffman blocks: their symbol path ends in §3.2.6,
while their extra bits still end in §3.2.5.

Huffman tables, expanded length assignments, the compact length program, and
the HCLEN vector open as generated `*.gzsym` documents. Their zero-based LSP
line is the semantic address: literal/length symbol 257 and its assignment are
line 257, program instruction 12 is line 12, code-length symbol 18 is line 18,
and HCLEN transmission slot 17 is line 17. Neovim normally presents buffer
lines as one-based, so give only this generated document type a semantic gutter:

```lua
vim.filetype.add({
  extension = { gzsym = "gzpls-symbols" },
})

vim.api.nvim_create_autocmd("FileType", {
  pattern = "gzpls-symbols",
  callback = function()
    vim.opt_local.number = true
    vim.opt_local.relativenumber = false
    vim.opt_local.statuscolumn = "%=%{v:lnum - 1} "
  end,
})
```

Include `gzpls-symbols` in the `filetypes` for the `gzpls` LSP configuration.
The generated files live only for the lifetime of the language server. The
trace, tar manifest, and RFC buffers retain their ordinary line-number display.

RFC destinations use a static semantic anchor index. The link retains the
governing paragraph as context while selecting the exact token: an HCLEN field
lands on its symbol in the §3.2.7 transmission-order list, and fixed-Huffman
symbols land on their precise literal range or the `Distance codes 0-31`
phrase in §3.2.6. The RFC is itself navigable: Go to Definition on `BFINAL`,
`BTYPE`, `HLIT`, `HDIST`, `HCLEN`, the three Huffman-alphabet names, or a
section reference from `3.2.1` through `3.2.7` jumps to its canonical
definition. Canonical occurrences intentionally have no self-link. Tests pin
every table cell and named anchor to its selected RFC text so a changed
vendored copy fails loudly instead of sending the cursor somewhere plausible.

### gsip

`gsip` is very similar to [`compress/gzip`](https://pkg.go.dev/compress/gzip) but for an `io.ReaderAt` instead of an `io.Reader`.

Internally, a `gsip.Reader` maintains a set of 32KB checkpoints that allow it to start decoding from the start of a DEFLATE block.
It also exposes an `Encode` method for saving those offsets to an `io.Writer`.
There is a `Decode` function that will restore a `gsip.Reader` by reading those checkpoints from an `io.Reader`.

The exact format of the checkpoints is currently not optimal (at all), but demonstrates the proof of concept.

### tarfs

`tarfs` implements an [`fs.FS`](https://pkg.go.dev/io/fs#FS) given an `io.ReaderAt` for a tar stream.

Similarly to `gsip.Reader`, `tarfs.FS` maintains an internal Table of Contents of tar metadata, which can be saved and restored with `Encode` and `Decode`.

The exact format of the TOC is currently not optimal (at all), but demonstrates the proof of concept.

### ranger

`ranger` implements an `io.ReaderAt` using [HTTP range requests](https://developer.mozilla.org/en-US/docs/Web/HTTP/Range_requests).

This needs some work to be more efficient, but demonstrates the proof of concept.

## TODO

* Add tests.
* Optimize formats for gsip.Index and tarfs.TOC.
* Allow incremental indexing of both gsip and tarfs metadata.
* Make concurrent tarfs access safe.
* Allow recycling of flate readers.
* Implement better checkpointing heuristics.

## See Also

For more mature implementations of similar ideas, see:

* https://github.com/madler/zlib/blob/develop/examples/zran.c
* https://github.com/circulosmeos/gztool
* https://github.com/awslabs/soci-snapshotter
