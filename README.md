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

The call hierarchy deliberately stays compact for overlapping copies. Instead
of constructing a thousand-node chain for `match len=1000 dist=1`, the match
has one outgoing edge to its one-byte seed and reports the repetition count in
the hierarchy detail.

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
