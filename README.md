# targz

This module is a collection of packages relating to gzipped tarballs.

Taken together, they allow for fast random access of a filesystem within a gzipped tarball.

This is primarily a proof of concept of the interfaces and how they compose:

<img src="./doc/viz.svg">

For more mature implementations of similar ideas, see:

* https://github.com/circulosmeos/gztool
* https://github.com/awslabs/soci-snapshotter

## Packages

### gsip

`gsip` is very similar to [`compress/gzip`](https://pkg.go.dev/compress/gzip) but for an `io.ReaderAt` instead of an `io.Reader`.

### tarfs

`tarfs` implements an [`fs.FS`](https://pkg.go.dev/io/fs#FS) given an `io.ReaderAt` for a tar stream.

### ranger

`ranger` doesn't exist yet, but it will implement an `io.ReaderAt` using [HTTP range requests](https://developer.mozilla.org/en-US/docs/Web/HTTP/Range_requests).

## TODO

* Implement `ranger`.
* Allow save/restore of tarfs table of contents.
* Allow save/restore of gsip checkpoints.
* Allow incremental indexing of both gsip and tarfs metadata.
* Make concurrent tarfs access safe.
* Allow recycling of flate readers.
* Implement better checkpointing heuristics.