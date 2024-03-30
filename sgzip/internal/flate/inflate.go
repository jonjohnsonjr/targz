// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package flate implements the DEFLATE compressed data format, described in
// RFC 1951.  The gzip and zlib packages implement access to DEFLATE-based file
// formats.
package flate

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"math/bits"
	"strconv"
	"sync"
	"time"
)

const (
	// The largest offset code.
	offsetCodeCount = 30

	// The special code used to mark the end of a block.
	endBlockMarker = 256

	// The first length code.
	lengthCodesStart = 257

	// The number of codegen codes.
	codegenCodeCount = 19
	badCode          = 255

	// bufferFlushSize indicates the buffer size
	// after which bytes are flushed to the writer.
	// Should preferably be a multiple of 6, since
	// we accumulate 6 bytes between writes to the buffer.
	bufferFlushSize = 240

	// bufferSize is the actual output byte buffer size.
	// It must have additional headroom for a flush
	// which can contain up to 8 bytes.
	bufferSize = bufferFlushSize + 8
)

const (
	NoCompression      = 0
	BestSpeed          = 1
	BestCompression    = 9
	DefaultCompression = -1

	// HuffmanOnly disables Lempel-Ziv match searching and only performs Huffman
	// entropy encoding. This mode is useful in compressing data that has
	// already been compressed with an LZ style algorithm (e.g. Snappy or LZ4)
	// that lacks an entropy encoder. Compression gains are achieved when
	// certain bytes in the input stream occur more frequently than others.
	//
	// Note that HuffmanOnly produces a compressed output that is
	// RFC 1951 compliant. That is, any valid DEFLATE decompressor will
	// continue to be able to decompress this output.
	HuffmanOnly = -2
)

const (
	logWindowSize = 15
	windowSize    = 1 << logWindowSize
	windowMask    = windowSize - 1

	// The LZ77 step produces a sequence of literal tokens and <length, offset>
	// pair tokens. The offset is also known as distance. The underlying wire
	// format limits the range of lengths and offsets. For example, there are
	// 256 legitimate lengths: those in the range [3, 258]. This package's
	// compressor uses a higher minimum match length, enabling optimizations
	// such as finding matches via 32-bit loads and compares.
	baseMatchLength = 3       // The smallest match length per the RFC section 3.2.5
	minMatchLength  = 4       // The smallest match length that the compressor actually emits
	maxMatchLength  = 258     // The largest match length
	baseMatchOffset = 1       // The smallest match offset
	maxMatchOffset  = 1 << 15 // The largest match offset

	// The maximum number of tokens we put into a single flate block, just to
	// stop things from getting too large.
	maxFlateBlockTokens = 1 << 14
	maxStoreBlockSize   = 65535
	hashBits            = 17 // After 17 performance degrades
	hashSize            = 1 << hashBits
	hashMask            = (1 << hashBits) - 1
	maxHashOffset       = 1 << 24

	skipNever = math.MaxInt32
)

const (
	maxCodeLen = 16 // max length of Huffman code
	// The next three numbers come from the RFC section 3.2.7, with the
	// additional proviso in section 3.2.5 which implies that distance codes
	// 30 and 31 should never occur in compressed data.
	maxNumLit  = 286
	maxNumDist = 30
	numCodes   = 19 // number of codes in Huffman meta-code
)

// Initialize the fixedHuffmanDecoder only once upon first use.
var fixedOnce sync.Once
var fixedHuffmanDecoder huffmanDecoder

// A CorruptInputError reports the presence of corrupt input at a given offset.
type CorruptInputError int64

func (e CorruptInputError) Error() string {
	return "flate: corrupt input before offset " + strconv.FormatInt(int64(e), 10)
}

// An InternalError reports an error in the flate code itself.
type InternalError string

func (e InternalError) Error() string { return "flate: internal error: " + string(e) }

// A ReadError reports an error encountered while reading input.
//
// Deprecated: No longer returned.
type ReadError struct {
	Offset int64 // byte offset where error occurred
	Err    error // error returned by underlying Read
}

func (e *ReadError) Error() string {
	return "flate: read error at offset " + strconv.FormatInt(e.Offset, 10) + ": " + e.Err.Error()
}

// A WriteError reports an error encountered while writing output.
//
// Deprecated: No longer returned.
type WriteError struct {
	Offset int64 // byte offset where error occurred
	Err    error // error returned by underlying Write
}

func (e *WriteError) Error() string {
	return "flate: write error at offset " + strconv.FormatInt(e.Offset, 10) + ": " + e.Err.Error()
}

// Resetter resets a ReadCloser returned by NewReader or NewReaderDict
// to switch to a new underlying Reader. This permits reusing a ReadCloser
// instead of allocating a new one.
type Resetter interface {
	// Reset discards any buffered data and resets the Resetter as if it was
	// newly initialized with the given reader.
	Reset(r io.Reader, dict []byte, roffset int64) error
}

// Gross, sorry.
type Woffseter interface {
	Woffset() int64
}

// The data structure for decoding Huffman tables is based on that of
// zlib. There is a lookup table of a fixed bit width (huffmanChunkBits),
// For codes smaller than the table width, there are multiple entries
// (each combination of trailing bits has the same value). For codes
// larger than the table width, the table contains a link to an overflow
// table. The width of each entry in the link table is the maximum code
// size minus the chunk width.
//
// Note that you can do a lookup in the table even without all bits
// filled. Since the extra bits are zero, and the DEFLATE Huffman codes
// have the property that shorter codes come before longer ones, the
// bit length estimate in the result is a lower bound on the actual
// number of bits.
//
// See the following:
//	https://github.com/madler/zlib/raw/master/doc/algorithm.txt

// chunk & 15 is number of bits
// chunk >> 4 is value, including table link

const (
	huffmanChunkBits  = 9
	huffmanNumChunks  = 1 << huffmanChunkBits
	huffmanCountMask  = 15
	huffmanValueShift = 4
)

type huffmanDecoder struct {
	min      int                      // the minimum code length
	chunks   [huffmanNumChunks]uint32 // chunks as described above
	links    [][]uint32               // overflow links
	linkMask uint32                   // mask the width of the link table
}

// Initialize Huffman decoding tables from array of code lengths.
// Following this function, h is guaranteed to be initialized into a complete
// tree (i.e., neither over-subscribed nor under-subscribed). The exception is a
// degenerate case where the tree has only a single symbol with length 1. Empty
// trees are permitted.
func (h *huffmanDecoder) init(lengths []int) bool {
	// Sanity enables additional runtime tests during Huffman
	// table construction. It's intended to be used during
	// development to supplement the currently ad-hoc unit tests.
	const sanity = true

	if h.min != 0 {
		*h = huffmanDecoder{}
	}

	// Count number of codes of each length,
	// compute min and max length.
	var count [maxCodeLen]int
	var min, max int
	for _, n := range lengths {
		if n == 0 {
			continue
		}
		if min == 0 || n < min {
			min = n
		}
		if n > max {
			max = n
		}
		count[n]++
	}

	// Empty tree. The decompressor.huffSym function will fail later if the tree
	// is used. Technically, an empty tree is only valid for the HDIST tree and
	// not the HCLEN and HLIT tree. However, a stream with an empty HCLEN tree
	// is guaranteed to fail since it will attempt to use the tree to decode the
	// codes for the HLIT and HDIST trees. Similarly, an empty HLIT tree is
	// guaranteed to fail later since the compressed data section must be
	// composed of at least one symbol (the end-of-block marker).
	if max == 0 {
		return true
	}

	code := 0
	var nextcode [maxCodeLen]int
	for i := min; i <= max; i++ {
		code <<= 1
		nextcode[i] = code
		code += count[i]
	}

	// Check that the coding is complete (i.e., that we've
	// assigned all 2-to-the-max possible bit sequences).
	// Exception: To be compatible with zlib, we also need to
	// accept degenerate single-code codings. See also
	// TestDegenerateHuffmanCoding.
	if code != 1<<uint(max) && !(code == 1 && max == 1) {
		if sanity {
			panic("coding incomplete")
		}
		return false
	}

	h.min = min
	if max > huffmanChunkBits {
		numLinks := 1 << (uint(max) - huffmanChunkBits)
		h.linkMask = uint32(numLinks - 1)

		// create link tables
		link := nextcode[huffmanChunkBits+1] >> 1
		h.links = make([][]uint32, huffmanNumChunks-link)
		for j := uint(link); j < huffmanNumChunks; j++ {
			reverse := int(bits.Reverse16(uint16(j)))
			reverse >>= uint(16 - huffmanChunkBits)
			off := j - uint(link)
			if sanity && h.chunks[reverse] != 0 {
				panic("impossible: overwriting existing chunk")
			}
			h.chunks[reverse] = uint32(off<<huffmanValueShift | (huffmanChunkBits + 1))
			h.links[off] = make([]uint32, numLinks)
		}
	}

	for i, n := range lengths {
		if n == 0 {
			continue
		}
		code := nextcode[n]
		nextcode[n]++
		chunk := uint32(i<<huffmanValueShift | n)
		reverse := int(bits.Reverse16(uint16(code)))
		reverse >>= uint(16 - n)
		if n <= huffmanChunkBits {
			for off := reverse; off < len(h.chunks); off += 1 << uint(n) {
				// We should never need to overwrite
				// an existing chunk. Also, 0 is
				// never a valid chunk, because the
				// lower 4 "count" bits should be
				// between 1 and 15.
				if sanity && h.chunks[off] != 0 {
					panic("impossible: overwriting existing chunk")
				}
				h.chunks[off] = chunk
			}
		} else {
			j := reverse & (huffmanNumChunks - 1)
			if sanity && h.chunks[j]&huffmanCountMask != huffmanChunkBits+1 {
				// Longer codes should have been
				// associated with a link table above.
				panic("impossible: not an indirect chunk")
			}
			value := h.chunks[j] >> huffmanValueShift
			linktab := h.links[value]
			reverse >>= huffmanChunkBits
			for off := reverse; off < len(linktab); off += 1 << uint(n-huffmanChunkBits) {
				if sanity && linktab[off] != 0 {
					panic("impossible: overwriting existing chunk")
				}
				linktab[off] = chunk
			}
		}
	}

	if sanity {
		// Above we've sanity checked that we never overwrote
		// an existing entry. Here we additionally check that
		// we filled the tables completely.
		for i, chunk := range h.chunks {
			if chunk == 0 {
				// As an exception, in the degenerate
				// single-code case, we allow odd
				// chunks to be missing.
				if code == 1 && i%2 == 1 {
					continue
				}
				panic("impossible: missing chunk")
			}
		}
		for _, linktab := range h.links {
			for _, chunk := range linktab {
				if chunk == 0 {
					panic("impossible: missing chunk")
				}
			}
		}
	}

	return true
}

// The actual read interface needed by NewReader.
// If the passed in io.Reader does not also have ReadByte,
// the NewReader will introduce its own buffering.
type Reader interface {
	io.Reader
	io.ByteReader
}

// Decompress state.
type Decompressor struct {
	// Input source.
	r       Reader
	roffset int64
	woffset int64

	// Input bits, in top of b.
	b  uint32
	nb uint

	// Huffman decoders for literal/length, distance.
	h1, h2 huffmanDecoder

	// Length arrays used to define Huffman codes.
	bits     *[maxNumLit + maxNumDist]int
	codebits *[numCodes]int

	// Output history, buffer.
	dict dictDecoder

	// Temporary buffer (avoids repeated allocation).
	buf [4]byte

	// Next step in the decompression,
	// and decompression state.
	step      func(*Decompressor)
	stepState int
	final     bool
	err       error
	toRead    []byte
	hl, hd    *huffmanDecoder
	copyLen   int
	copyDist  int

	// Jon's hacking
	span    int64
	last    int64
	updates chan<- *Checkpoint
}

func (f *Decompressor) nextBlock() {
	for f.nb < 1+2 {
		if f.err = f.moreBits(); f.err != nil {
			return
		}
	}
	f.final = f.b&1 == 1
	f.b >>= 1
	typ := f.b & 3
	f.b >>= 2
	f.nb -= 1 + 2
	switch typ {
	case 0:
		f.dataBlock()
	case 1:
		// compressed, fixed Huffman tables
		f.hl = &fixedHuffmanDecoder
		f.hd = nil
		f.huffmanBlock()
	case 2:
		// compressed, dynamic Huffman tables
		if f.err = f.readHuffman(); f.err != nil {
			break
		}
		f.hl = &f.h1
		f.hd = &f.h2
		f.huffmanBlock()
	default:
		// 3 is reserved.
		f.err = CorruptInputError(f.roffset)
	}
}

func (f *Decompressor) Read(b []byte) (int, error) {
	for {
		if len(f.toRead) > 0 {
			n := copy(b, f.toRead)
			f.toRead = f.toRead[n:]
			if len(f.toRead) == 0 {
				return n, f.err
			}
			return n, nil
		}
		if f.err != nil {
			return 0, f.err
		}
		f.step(f)
		f.woffset += int64(len(f.toRead))
		if f.err != nil && len(f.toRead) == 0 {
			f.toRead = f.dict.readFlush() // Flush what's left in case of error
			f.woffset += int64(len(f.toRead))
		}
	}
}

func (f *Decompressor) Close() error {
	if f.err == io.EOF {
		return nil
	}
	return f.err
}

// RFC 1951 section 3.2.7.
// Compression with dynamic Huffman codes

var codeOrder = [...]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

func (f *Decompressor) readHuffman() error {
	// HLIT[5], HDIST[5], HCLEN[4].
	for f.nb < 5+5+4 {
		if err := f.moreBits(); err != nil {
			return err
		}
	}
	nlit := int(f.b&0x1F) + 257
	if nlit > maxNumLit {
		return CorruptInputError(f.roffset)
	}
	f.b >>= 5
	ndist := int(f.b&0x1F) + 1
	if ndist > maxNumDist {
		return CorruptInputError(f.roffset)
	}
	f.b >>= 5
	nclen := int(f.b&0xF) + 4
	// numCodes is 19, so nclen is always valid.
	f.b >>= 4
	f.nb -= 5 + 5 + 4

	// (HCLEN+4)*3 bits: code lengths in the magic codeOrder order.
	for i := 0; i < nclen; i++ {
		for f.nb < 3 {
			if err := f.moreBits(); err != nil {
				return err
			}
		}
		f.codebits[codeOrder[i]] = int(f.b & 0x7)
		f.b >>= 3
		f.nb -= 3
	}
	for i := nclen; i < len(codeOrder); i++ {
		f.codebits[codeOrder[i]] = 0
	}
	if !f.h1.init(f.codebits[0:]) {
		return CorruptInputError(f.roffset)
	}

	// HLIT + 257 code lengths, HDIST + 1 code lengths,
	// using the code length Huffman code.
	for i, n := 0, nlit+ndist; i < n; {
		x, err := f.huffSym(&f.h1)
		if err != nil {
			return err
		}
		if x < 16 {
			// Actual length.
			f.bits[i] = x
			i++
			continue
		}
		// Repeat previous length or zero.
		var rep int
		var nb uint
		var b int
		switch x {
		default:
			return InternalError("unexpected length code")
		case 16:
			rep = 3
			nb = 2
			if i == 0 {
				return CorruptInputError(f.roffset)
			}
			b = f.bits[i-1]
		case 17:
			rep = 3
			nb = 3
			b = 0
		case 18:
			rep = 11
			nb = 7
			b = 0
		}
		for f.nb < nb {
			if err := f.moreBits(); err != nil {
				return err
			}
		}
		rep += int(f.b & uint32(1<<nb-1))
		f.b >>= nb
		f.nb -= nb
		if i+rep > n {
			return CorruptInputError(f.roffset)
		}
		for j := 0; j < rep; j++ {
			f.bits[i] = b
			i++
		}
	}

	if !f.h1.init(f.bits[0:nlit]) || !f.h2.init(f.bits[nlit:nlit+ndist]) {
		return CorruptInputError(f.roffset)
	}

	// As an optimization, we can initialize the min bits to read at a time
	// for the HLIT tree to the length of the EOB marker since we know that
	// every block must terminate with one. This preserves the property that
	// we never read any extra bytes after the end of the DEFLATE stream.
	if f.h1.min < f.bits[endBlockMarker] {
		f.h1.min = f.bits[endBlockMarker]
	}

	return nil
}

// Decode a single Huffman block from f.
// hl and hd are the Huffman states for the lit/length values
// and the distance values, respectively. If hd == nil, using the
// fixed distance encoding associated with fixed Huffman blocks.
func (f *Decompressor) huffmanBlock() {
	const (
		stateInit = iota // Zero value must be stateInit
		stateDict
	)

	switch f.stepState {
	case stateInit:
		goto readLiteral
	case stateDict:
		goto copyHistory
	}

readLiteral:
	// Read literal and/or (length, distance) according to RFC section 3.2.3.
	{
		v, err := f.huffSym(f.hl)
		if err != nil {
			f.err = err
			return
		}
		var n uint // number of bits extra
		var length int
		switch {
		case v < 256:
			f.dict.writeByte(byte(v))
			if f.dict.availWrite() == 0 {
				f.toRead = f.dict.readFlush()
				f.step = (*Decompressor).huffmanBlock
				f.stepState = stateInit
				return
			}
			goto readLiteral
		case v == 256:
			f.finishBlock()
			return
		// otherwise, reference to older data
		case v < 265:
			length = v - (257 - 3)
			n = 0
		case v < 269:
			length = v*2 - (265*2 - 11)
			n = 1
		case v < 273:
			length = v*4 - (269*4 - 19)
			n = 2
		case v < 277:
			length = v*8 - (273*8 - 35)
			n = 3
		case v < 281:
			length = v*16 - (277*16 - 67)
			n = 4
		case v < 285:
			length = v*32 - (281*32 - 131)
			n = 5
		case v < maxNumLit:
			length = 258
			n = 0
		default:
			f.err = CorruptInputError(f.roffset)
			return
		}
		if n > 0 {
			for f.nb < n {
				if err = f.moreBits(); err != nil {
					f.err = err
					return
				}
			}
			length += int(f.b & uint32(1<<n-1))
			f.b >>= n
			f.nb -= n
		}

		var dist int
		if f.hd == nil {
			for f.nb < 5 {
				if err = f.moreBits(); err != nil {
					f.err = err
					return
				}
			}
			dist = int(bits.Reverse8(uint8(f.b & 0x1F << 3)))
			f.b >>= 5
			f.nb -= 5
		} else {
			if dist, err = f.huffSym(f.hd); err != nil {
				f.err = err
				return
			}
		}

		switch {
		case dist < 4:
			dist++
		case dist < maxNumDist:
			nb := uint(dist-2) >> 1
			// have 1 bit in bottom of dist, need nb more.
			extra := (dist & 1) << nb
			for f.nb < nb {
				if err = f.moreBits(); err != nil {
					f.err = err
					return
				}
			}
			extra |= int(f.b & uint32(1<<nb-1))
			f.b >>= nb
			f.nb -= nb
			dist = 1<<(nb+1) + 1 + extra
		default:
			f.err = CorruptInputError(f.roffset)
			return
		}

		// No check on length; encoding can be prescient.
		if dist > f.dict.histSize() {
			f.err = CorruptInputError(f.roffset)
			return
		}

		f.copyLen, f.copyDist = length, dist
		goto copyHistory
	}

copyHistory:
	// Perform a backwards copy according to RFC section 3.2.3.
	{
		cnt := f.dict.tryWriteCopy(f.copyDist, f.copyLen)
		if cnt == 0 {
			cnt = f.dict.writeCopy(f.copyDist, f.copyLen)
		}
		f.copyLen -= cnt

		if f.dict.availWrite() == 0 || f.copyLen > 0 {
			f.toRead = f.dict.readFlush()
			f.step = (*Decompressor).huffmanBlock // We need to continue this work
			f.stepState = stateDict
			return
		}
		goto readLiteral
	}
}

// Copy a single uncompressed data block from input to output.
func (f *Decompressor) dataBlock() {
	// Uncompressed.
	// Discard current half-byte.
	f.nb = 0
	f.b = 0

	// Length then ones-complement of length.
	nr, err := io.ReadFull(f.r, f.buf[0:4])
	f.roffset += int64(nr)
	if err != nil {
		f.err = noEOF(err)
		return
	}
	n := int(f.buf[0]) | int(f.buf[1])<<8
	nn := int(f.buf[2]) | int(f.buf[3])<<8
	if uint16(nn) != uint16(^n) {
		f.err = CorruptInputError(f.roffset)
		return
	}

	if n == 0 {
		f.toRead = f.dict.readFlush()
		f.finishBlock()
		return
	}

	f.copyLen = n
	f.copyData()
}

// copyData copies f.copyLen bytes from the underlying reader into f.hist.
// It pauses for reads when f.hist is full.
func (f *Decompressor) copyData() {
	buf := f.dict.writeSlice()
	if len(buf) > f.copyLen {
		buf = buf[:f.copyLen]
	}

	cnt, err := io.ReadFull(f.r, buf)
	f.roffset += int64(cnt)
	f.copyLen -= cnt
	f.dict.writeMark(cnt)
	if err != nil {
		f.err = noEOF(err)
		return
	}

	if f.dict.availWrite() == 0 || f.copyLen > 0 {
		f.toRead = f.dict.readFlush()
		f.step = (*Decompressor).copyData
		return
	}
	f.finishBlock()
}

func (f *Decompressor) finishBlock() {
	woffset := f.woffset

	if f.final {
		// TODO(jon): What does final span look like.
		if f.dict.availRead() > 0 {
			f.toRead = f.dict.readFlush()
			woffset += int64(len(f.toRead))
		}
		f.err = io.EOF
	}
	if f.updates != nil && (woffset-f.last > f.span) {
		checkpoint := &Checkpoint{
			Hist:  make([]byte, len(f.dict.hist)),
			In:    f.roffset,
			Out:   woffset,
			B:     f.b,
			NB:    f.nb,
			WrPos: f.dict.wrPos,
			RdPos: f.dict.rdPos,
			Full:  f.dict.full,
		}
		copy(checkpoint.Hist, f.dict.hist)

		f.updates <- checkpoint
		f.last = checkpoint.Out
	}
	f.step = (*Decompressor).nextBlock
}

// noEOF returns err, unless err == io.EOF, in which case it returns io.ErrUnexpectedEOF.
func noEOF(e error) error {
	if e == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return e
}

func (f *Decompressor) moreBits() error {
	c, err := f.r.ReadByte()
	if err != nil {
		return noEOF(err)
	}
	f.roffset++
	f.b |= uint32(c) << f.nb
	f.nb += 8
	return nil
}

// Read the next Huffman-encoded symbol from f according to h.
func (f *Decompressor) huffSym(h *huffmanDecoder) (int, error) {
	// Since a huffmanDecoder can be empty or be composed of a degenerate tree
	// with single element, huffSym must error on these two edge cases. In both
	// cases, the chunks slice will be 0 for the invalid sequence, leading it
	// satisfy the n == 0 check below.
	n := uint(h.min)
	// Optimization. Compiler isn't smart enough to keep f.b,f.nb in registers,
	// but is smart enough to keep local variables in registers, so use nb and b,
	// inline call to moreBits and reassign b,nb back to f on return.
	nb, b := f.nb, f.b
	for {
		for nb < n {
			c, err := f.r.ReadByte()
			if err != nil {
				f.b = b
				f.nb = nb
				return 0, noEOF(err)
			}
			f.roffset++
			b |= uint32(c) << (nb & 31)
			nb += 8
		}
		chunk := h.chunks[b&(huffmanNumChunks-1)]
		n = uint(chunk & huffmanCountMask)
		if n > huffmanChunkBits {
			chunk = h.links[chunk>>huffmanValueShift][(b>>huffmanChunkBits)&h.linkMask]
			n = uint(chunk & huffmanCountMask)
		}
		if n <= nb {
			if n == 0 {
				f.b = b
				f.nb = nb
				f.err = CorruptInputError(f.roffset)
				return 0, f.err
			}
			f.b = b >> (n & 31)
			f.nb = nb - n
			return int(chunk >> huffmanValueShift), nil
		}
	}
}

func makeReader(r io.Reader) Reader {
	if rr, ok := r.(Reader); ok {
		return rr
	}
	return bufio.NewReader(r)
}

func fixedHuffmanDecoderInit() {
	fixedOnce.Do(func() {
		// These come from the RFC section 3.2.6.
		var bits [288]int
		for i := 0; i < 144; i++ {
			bits[i] = 8
		}
		for i := 144; i < 256; i++ {
			bits[i] = 9
		}
		for i := 256; i < 280; i++ {
			bits[i] = 7
		}
		for i := 280; i < 288; i++ {
			bits[i] = 8
		}
		fixedHuffmanDecoder.init(bits[:])
	})
}

func (f *Decompressor) Woffset() int64 {
	return f.woffset
}

func (f *Decompressor) Reset(r io.Reader, dict []byte, roffset int64) error {
	*f = Decompressor{
		r:        makeReader(r),
		bits:     new([maxNumLit + maxNumDist]int),
		codebits: new([numCodes]int),
		step:     (*Decompressor).nextBlock,
		last:     f.woffset, // Requires that ungzip send a checkpoint before Reset
		span:     f.span,
		updates:  f.updates,
		woffset:  f.woffset,
		roffset:  roffset,
	}
	f.dict.init(maxMatchOffset, dict)
	return nil
}

// NewReader returns a new ReadCloser that can be used
// to read the uncompressed version of r.
// If r does not also implement io.ByteReader,
// the decompressor may read more data than necessary from r.
// The reader returns io.EOF after the final block in the DEFLATE stream has
// been encountered. Any trailing data after the final block is ignored.
//
// The ReadCloser returned by NewReader also implements Resetter.
func NewReader(r io.Reader) io.ReadCloser {
	fixedHuffmanDecoderInit()

	var f Decompressor
	f.r = makeReader(r)
	f.bits = new([maxNumLit + maxNumDist]int)
	f.codebits = new([numCodes]int)
	f.step = (*Decompressor).nextBlock
	f.dict.init(maxMatchOffset, nil)
	return &f
}

// NewReaderDict is like NewReader but initializes the reader
// with a preset dictionary. The returned Reader behaves as if
// the uncompressed data stream started with the given dictionary,
// which has already been read. NewReaderDict is typically used
// to read data compressed by NewWriterDict.
//
// The ReadCloser returned by NewReader also implements Resetter.
func NewReaderDict(r io.Reader, dict []byte) io.ReadCloser {
	fixedHuffmanDecoderInit()

	var f Decompressor
	f.r = makeReader(r)
	f.bits = new([maxNumLit + maxNumDist]int)
	f.codebits = new([numCodes]int)
	f.step = (*Decompressor).nextBlock
	f.dict.init(maxMatchOffset, dict)
	return &f
}

type Header struct {
	Comment string     `json:"comment,omitempty"`
	Extra   []byte     `json:"extra,omitempty"`
	ModTime *time.Time `json:"modtime,omitempty"`
	Name    string     `json:"name,omitempty"`
	OS      *byte      `json:"os,omitempty"`
}

type Trailer struct {
	Digest uint32 `json:"crc32,omitempty"`
	Size   uint32 `json:"isize,omitempty"`
}

type Checkpoint struct {
	// TODO: separate these from the rest
	In  int64 `json:"in,omitempty"`
	Out int64 `json:"out,omitempty"`

	// bits?
	B    uint32 `json:"b,omitempty"`
	NB   uint   `json:"nb,omitempty"`
	Hist []byte `json:"hist,omitempty"`

	// Trying random stuff...
	WrPos int  `json:"wrpos,omitempty"`
	RdPos int  `json:"rdpos,omitempty"`
	Full  bool `json:"full,omitempty"`

	// If there is no Hist, we can avoid writing the file.
	Empty bool `json:"empty,omitempty"`

	// Optional gzip header.
	GzipHeader *Header `json:"header,omitempty"`
}

func (c *Checkpoint) History() []byte {
	return c.Hist
}

func (c *Checkpoint) SetHistory(b []byte) {
	c.Hist = b
}

func (c *Checkpoint) IsEmpty() bool {
	return c.Empty
}

func (c *Checkpoint) BytesRead() int64 {
	return c.In
}

func (c *Checkpoint) BytesWritten() int64 {
	return c.Out
}

// NewReaderWithSpans is a hack.
func NewReaderWithSpans(r io.Reader, span int64, start int64, updates chan<- *Checkpoint) *Decompressor {
	fixedHuffmanDecoderInit()

	var f Decompressor
	f.r = makeReader(r)
	f.bits = new([maxNumLit + maxNumDist]int)
	f.codebits = new([numCodes]int)
	f.step = (*Decompressor).nextBlock
	f.dict.init(maxMatchOffset, nil)
	f.roffset = start
	f.last = start
	f.span = span
	f.updates = updates
	return &f
}

func Continue(r io.Reader, from *Checkpoint, span int64, updates chan<- *Checkpoint) *Decompressor {
	fixedHuffmanDecoderInit()

	var f Decompressor
	f.r = makeReader(r)
	f.bits = new([maxNumLit + maxNumDist]int)
	f.codebits = new([numCodes]int)
	f.step = (*Decompressor).nextBlock

	f.dict = dictDecoder{}
	f.dict.hist = make([]byte, maxMatchOffset)
	copy(f.dict.hist, from.Hist)
	f.dict.wrPos = from.WrPos
	f.dict.rdPos = from.RdPos
	f.dict.full = from.Full

	f.b = from.B
	f.nb = from.NB
	f.roffset = from.In
	f.woffset = from.Out

	f.last = from.Out // TODO: This was from.In but I think that was a bug.
	f.updates = updates
	f.span = span

	return &f
}

func (f *Decompressor) ResetTo(from *Checkpoint) (int64, error) {
	// Like Continue and Reset but we want to keep our high watermark to avoid sending redundant checkpoints.
	return 0, fmt.Errorf("implement ResetTo")
}
