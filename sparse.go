// Copyright 2026 soe1hom-arch
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
)

// ─── Android Sparse Image Format ───────────────────────────────────────────
// Based on AOSP system/core/libsparse/sparse_format.h
//
// SparseHeader: fixed 28-byte header at offset 0.
// ChunkHeader:  12-byte header before each chunk's payload.

const (
	// SparseHeaderMagic is the expected magic value: 0xED26FF3A
	// (little-endian: 3A FF 26 ED)
	SparseHeaderMagic = 0xED26FF3A

	// SparseHeaderSize is the fixed size of the file header (v1.0).
	SparseHeaderSize = 28
	// ChunkHeaderSize is the fixed size of each chunk header (v1.0).
	ChunkHeaderSize = 12

	// Chunk type constants.
	ChunkTypeRaw      = 0xCAC1
	ChunkTypeFill     = 0xCAC2
	ChunkTypeDontCare = 0xCAC3
	ChunkTypeCRC32    = 0xCAC4
)

// SparseHeader represents the Android sparse image file header (28 bytes).
//
//	struct sparse_header {
//	    __le32 magic;           // 0xED26FF3A
//	    __le16 major_version;   // 1
//	    __le16 minor_version;   // 0
//	    __le16 file_hdr_sz;     // 28
//	    __le16 chunk_hdr_sz;    // 12
//	    __le32 blk_sz;          // block size (4096)
//	    __le32 total_blks;      // total blocks in raw output
//	    __le32 total_chunks;    // total chunks in sparse input
//	    __le32 image_checksum;  // optional CRC32 (0 if unused)
//	}; // 28 bytes
type SparseHeader struct {
	Magic         uint32
	MajorVersion  uint16
	MinorVersion  uint16
	FileHeaderSz  uint16
	ChunkHeaderSz uint16
	BlockSz       uint32
	TotalBlocks   uint32
	TotalChunks   uint32
	ImageChecksum uint32
}

// ChunkHeader represents a single chunk header (12 bytes).
//
//	struct chunk_header {
//	    __le16 chunk_type;      // RAW(0xCAC1), FILL(0xCAC2),
//	                            // DONTCARE(0xCAC3), CRC32(0xCAC4)
//	    __le16 reserved1;       // 0
//	    __le32 chunk_sz;        // blocks covered in output
//	    __le32 total_sz;        // total bytes (header + payload)
//	}; // 12 bytes
type ChunkHeader struct {
	ChunkType uint16
	Reserved  uint16
	ChunkSz   uint32 // number of blocks in output
	TotalSz   uint32 // total size in bytes of this chunk
}

// chunkEntry is the internal index entry for one chunk, built during
// initialisation so that ReadAt can jump directly to the correct data.
type chunkEntry struct {
	rawOffset int64   // starting offset in the raw (de-sparsed) image
	rawSize   int64   // size this chunk contributes to the raw image
	chunkType uint16
	fileOff   int64   // offset in the sparse file where the payload begins

	// For FILL chunks: the 4-byte fill pattern repeated across the raw range.
	fillBytes [4]byte
}

// SparseReaderAt implements io.ReaderAt by translating reads through the
// sparse chunk index, giving the illusion of a contiguous raw image without
// materialising the entire de-sparsed output on disk.
type SparseReaderAt struct {
	file      *os.File
	header    SparseHeader
	chunks    []chunkEntry
	totalSize int64
}

// ─── Header parsers ────────────────────────────────────────────────────────

// ReadSparseHeader reads and validates the sparse image header from r.
// It returns an error if the magic, major version or block-size invariants
// are violated.
func ReadSparseHeader(r io.Reader) (SparseHeader, error) {
	buf := make([]byte, SparseHeaderSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return SparseHeader{}, fmt.Errorf("reading sparse header: %w", err)
	}

	h := SparseHeader{
		Magic:         binary.LittleEndian.Uint32(buf[0:4]),
		MajorVersion:  binary.LittleEndian.Uint16(buf[4:6]),
		MinorVersion:  binary.LittleEndian.Uint16(buf[6:8]),
		FileHeaderSz:  binary.LittleEndian.Uint16(buf[8:10]),
		ChunkHeaderSz: binary.LittleEndian.Uint16(buf[10:12]),
		BlockSz:       binary.LittleEndian.Uint32(buf[12:16]),
		TotalBlocks:   binary.LittleEndian.Uint32(buf[16:20]),
		TotalChunks:   binary.LittleEndian.Uint32(buf[20:24]),
		ImageChecksum: binary.LittleEndian.Uint32(buf[24:28]),
	}

	if h.Magic != SparseHeaderMagic {
		return SparseHeader{}, fmt.Errorf(
			"invalid sparse header magic: 0x%08X (expected 0x%08X)",
			h.Magic, SparseHeaderMagic,
		)
	}
	if h.MajorVersion != 1 {
		return SparseHeader{}, fmt.Errorf(
			"unsupported sparse major version: %d (expected 1)",
			h.MajorVersion,
		)
	}
	if h.BlockSz == 0 || h.BlockSz%4 != 0 {
		return SparseHeader{}, fmt.Errorf(
			"invalid sparse block size: %d (must be >0 and multiple of 4)",
			h.BlockSz,
		)
	}
	if int64(h.FileHeaderSz) < SparseHeaderSize {
		return SparseHeader{}, fmt.Errorf(
			"sparse file header too small: %d (minimum %d)",
			h.FileHeaderSz, SparseHeaderSize,
		)
	}
	return h, nil
}

// ReadChunkHeader reads a single chunk header from r.
func ReadChunkHeader(r io.Reader) (ChunkHeader, error) {
	buf := make([]byte, ChunkHeaderSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return ChunkHeader{}, fmt.Errorf("reading chunk header: %w", err)
	}

	ch := ChunkHeader{
		ChunkType: binary.LittleEndian.Uint16(buf[0:2]),
		Reserved:  binary.LittleEndian.Uint16(buf[2:4]),
		ChunkSz:   binary.LittleEndian.Uint32(buf[4:8]),
		TotalSz:   binary.LittleEndian.Uint32(buf[8:12]),
	}
	return ch, nil
}

// ─── SparseReaderAt construction ──────────────────────────────────────────

// NewSparseReaderAt opens and indexes a sparse image file so it can be used
// as a random-access raw image reader.
//
// The caller is responsible for closing the returned SparseReaderAt once done.
func NewSparseReaderAt(file *os.File) (*SparseReaderAt, error) {
	// Seek to the beginning.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to start: %w", err)
	}

	// Parse the file header.
	header, err := ReadSparseHeader(file)
	if err != nil {
		return nil, err
	}

	// If the file header is larger than the standard 28 bytes, skip the
	// extended portion (future-proofing).
	if extra := int64(header.FileHeaderSz) - SparseHeaderSize; extra > 0 {
		if _, err := file.Seek(extra, io.SeekCurrent); err != nil {
			return nil, fmt.Errorf("skipping extended sparse header: %w", err)
		}
	}

	s := &SparseReaderAt{
		file:      file,
		header:    header,
		chunks:    make([]chunkEntry, 0, header.TotalChunks),
		totalSize: int64(header.TotalBlocks) * int64(header.BlockSz),
	}

	blockSz := int64(header.BlockSz)
	var rawOffset int64

	for i := uint32(0); i < header.TotalChunks; i++ {
		ch, err := ReadChunkHeader(file)
		if err != nil {
			return nil, fmt.Errorf("chunk %d: %w", i, err)
		}

		chHdrSz := int64(header.ChunkHeaderSz)
		if int64(ch.TotalSz) < chHdrSz {
			return nil, fmt.Errorf(
				"chunk %d: total size %d < chunk header size %d",
				i, ch.TotalSz, header.ChunkHeaderSz,
			)
		}

		payloadSize := int64(ch.TotalSz) - chHdrSz
		rawSize := int64(ch.ChunkSz) * blockSz

		switch ch.ChunkType {
		case ChunkTypeRaw:
			if payloadSize != rawSize {
				return nil, fmt.Errorf(
					"chunk %d (RAW): payload %d bytes != expected raw size %d bytes",
					i, payloadSize, rawSize,
				)
			}
			// Record the file offset where this chunk's raw data starts.
			pos, err := file.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil, fmt.Errorf("chunk %d: tell: %w", i, err)
			}
			s.chunks = append(s.chunks, chunkEntry{
				rawOffset: rawOffset,
				rawSize:   rawSize,
				chunkType: ChunkTypeRaw,
				fileOff:   pos,
			})
			// Advance past the payload data.
			if _, err := file.Seek(payloadSize, io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("chunk %d: seek payload: %w", i, err)
			}

		case ChunkTypeFill:
			if payloadSize != 4 {
				return nil, fmt.Errorf(
					"chunk %d (FILL): payload size %d != 4",
					i, payloadSize,
				)
			}
			var fill [4]byte
			if _, err := io.ReadFull(file, fill[:]); err != nil {
				return nil, fmt.Errorf("chunk %d: read fill data: %w", i, err)
			}
			s.chunks = append(s.chunks, chunkEntry{
				rawOffset: rawOffset,
				rawSize:   rawSize,
				chunkType: ChunkTypeFill,
				fillBytes: fill,
			})

		case ChunkTypeDontCare:
			if payloadSize > 0 {
				// Some images include a zero-length DONTCARE payload,
				// but we skip it just in case.
				if _, err := file.Seek(payloadSize, io.SeekCurrent); err != nil {
					return nil, fmt.Errorf("chunk %d: seek dontcare: %w", i, err)
				}
			}
			s.chunks = append(s.chunks, chunkEntry{
				rawOffset: rawOffset,
				rawSize:   rawSize,
				chunkType: ChunkTypeDontCare,
			})

		case ChunkTypeCRC32:
			if payloadSize != 4 {
				return nil, fmt.Errorf(
					"chunk %d (CRC32): payload size %d != 4",
					i, payloadSize,
				)
			}
			// CRC32 chunks contribute no output data; just skip the
			// 4-byte checksum and move on.
			if _, err := file.Seek(payloadSize, io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("chunk %d: seek crc32: %w", i, err)
			}
			// Do NOT append to s.chunks — CRC32 has no raw counterpart.

		default:
			return nil, fmt.Errorf(
				"chunk %d: unknown type 0x%04X",
				i, ch.ChunkType,
			)
		}

		rawOffset += rawSize
	}

	// Sanity check: the sum of all chunk raw sizes must equal the total
	// size declared in the header.
	if rawOffset != s.totalSize {
		return nil, fmt.Errorf(
			"sparse: raw-size mismatch — chunks sum to %d, header declares %d",
			rawOffset, s.totalSize,
		)
	}

	return s, nil
}

// ─── io.ReaderAt implementation ───────────────────────────────────────────

// ReadAt reads len(p) bytes from the de-sparsed image starting at offset off.
//
// It implements io.ReaderAt so it can be used directly with io.SectionReader
// by the LP metadata parser and the partition extractor.
func (s *SparseReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("sparse: negative offset %d", off)
	}
	if off >= s.totalSize {
		return 0, io.EOF
	}

	// Clamp the read to the remaining image size.
	if maxEnd := s.totalSize; off+int64(len(p)) > maxEnd {
		p = p[:int(maxEnd-off)]
	}

	totalN := 0
	for len(p) > 0 {
		idx := s.findChunk(off)
		if idx < 0 {
			return totalN, fmt.Errorf(
				"sparse: no chunk covers raw offset %d", off,
			)
		}
		ch := &s.chunks[idx]

		chunkOff := off - ch.rawOffset
		remaining := ch.rawSize - chunkOff
		toRead := int64(len(p))
		if toRead > remaining {
			toRead = remaining
		}

		var n int
		var err error

		switch ch.chunkType {
		case ChunkTypeRaw:
			n, err = s.readRawAt(p[:toRead], ch.fileOff+chunkOff)
		case ChunkTypeFill:
			n, err = readFillAt(p[:toRead], ch.fillBytes, chunkOff)
		case ChunkTypeDontCare:
			n, err = readZeros(p[:toRead])
		default:
			return totalN, fmt.Errorf(
				"sparse: unexpected chunk type 0x%04X at raw offset %d",
				ch.chunkType, off,
			)
		}

		totalN += n
		if err != nil {
			return totalN, err
		}
		p = p[n:]
		off += int64(n)
	}

	return totalN, nil
}

// findChunk binary-searches the chunk index for the entry covering raw
// offset off.  Returns -1 if no such chunk exists.
func (s *SparseReaderAt) findChunk(off int64) int {
	i := sort.Search(len(s.chunks), func(i int) bool {
		return s.chunks[i].rawOffset > off
	})
	i--
	if i < 0 || off >= s.chunks[i].rawOffset+s.chunks[i].rawSize {
		return -1
	}
	return i
}

// readRawAt reads from the underlying sparse file at the given absolute
// offset (which points into a RAW chunk's payload).
func (s *SparseReaderAt) readRawAt(p []byte, fileOff int64) (int, error) {
	return s.file.ReadAt(p, fileOff)
}

// readFillAt fills p by repeating the 4-byte fill pattern, accounting for
// an intra-chunk offset so adjacent ReadAt calls are coherent.
func readFillAt(p []byte, fill [4]byte, chunkOff int64) (int, error) {
	start := int(chunkOff) % 4
	for i := range p {
		p[i] = fill[(start+i)%4]
	}
	return len(p), nil
}

// readZeros fills p with zero bytes (used for DONTCARE ranges).
func readZeros(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────

// Size returns the size of the raw (de-sparsed) image in bytes.
func (s *SparseReaderAt) Size() int64 {
	return s.totalSize
}

// Close closes the underlying sparse image file.
func (s *SparseReaderAt) Close() error {
	return s.file.Close()
}

// IsSparseImage reports whether the first four bytes of f look like a
// valid sparse image magic.  The file offset is not modified.
func IsSparseImage(f *os.File) (bool, error) {
	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 0); err != nil {
		return false, err
	}
	return binary.LittleEndian.Uint32(magic[:]) == SparseHeaderMagic, nil
}
