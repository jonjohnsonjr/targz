# DEFLATE trace engine

This package treats DEFLATE as an instruction stream rather than an opaque
decompression implementation.

The central invariant is that every output byte has two independently
queryable forms of provenance:

1. **Control provenance**: the exact compressed bits encoding the literal,
   stored byte, or length/distance instruction.
2. **Data provenance**: the earlier output byte, literal, or stored input byte
   from which its value originates.

## Model

`ParseRaw` and `ParseGzip` produce an immutable `Trace` containing:

- gzip members;
- DEFLATE blocks and their Huffman tables;
- output-producing operations;
- exact compressed bit ranges;
- decompressed output; and
- indexes sufficient for producer and provenance queries.

For a match beginning at output `D` with distance `d`, an output byte `p` has
the direct source `p-d`. If the copy overlaps itself, the source is normalized
into the pre-match seed with:

```text
D - d + ((p - D) mod d)
```

This represents a long repeating match without constructing one graph edge per
output byte.

## Queries

```go
trace, err := deflate.ParseGzip(compressed)

producer, ok := trace.Producer(100)
origin, err := trace.Origin(100)
uses := trace.Uses(deflate.OutRange{Start: 82, End: 83})
```

`Origin` terminates at a literal or stored input byte. Each match step reports
both the machine's direct source and the equivalent normalized seed source.

`Uses` performs the reverse query and returns match instructions whose
normalized seeds intersect the selected output range.

## Intentional constraints

- The parser is a readable reference implementation, not the production fast
  path.
- Input is currently held in memory so the prototype can concentrate on the
  semantic model. A paged `ReaderAt` source is the next natural boundary.
- Reverse-use queries currently scan operations. A persistent interval index
  can replace that implementation without changing the public model.
- Gzip headers, trailers, CRC32, and concatenated members are validated.
- Raw DEFLATE parsing permits trailing bytes and exposes the logical end through
  `Trace.ConsumedBits`.
