# targz

[![GoDoc](https://godoc.org/github.com/jonjohnsonjr/targz?status.svg)](https://pkg.go.dev/github.com/jonjohnsonjr/targz)

This module is a collection of packages relating to gzipped tarballs.

Taken together, they allow for fast random access of a filesystem within a gzipped tarball.

This is primarily a proof of concept of the interfaces and how they compose:

<img src="./doc/viz.svg">

## Packages

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
