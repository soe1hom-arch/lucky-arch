// Copyright 2026 soe1hom-arch
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ─── AOSP LP Metadata (liblp) Constants ───────────────────────────────────
//
// Based on AOSP system/core/fs_mgr/liblp/ — metadata format for logical
// partitions on Android 10+ super images.

const (
	// LPMetadataGeometryMagic = "gDla" stored as LE uint32.
	LPMetadataGeometryMagic = 0x616c4467
	// LPMetadataHeaderMagic = "ALP0" stored as LE uint32.
	LPMetadataHeaderMagic      = 0x414C5030
	LPMetadataMajorVersion     = 10
	LPMetadataMinorVersionMin  = 0
	LPMetadataMinorVersionMax  = 2
	LPMetadataGeometrySize     = 4096
	LPSectorSize               = 512
	LPPartitionReservedBytes   = 4096
	LPTargetTypeLinear         = 0
	LPTargetTypeZero           = 1
	LPPartitionAttrSlotSuffixed = 1 << 1

	// Scan parameters for geometry search fallback.
	scanChunkSize  = 4 * 1024 * 1024 // 4 MiB
	scanLimitBytes = 256 * 1024 * 1024 // 256 MiB
)

// ─── AOSP Struct Definitions ──────────────────────────────────────────────

// LpMetadataGeometry matches the on-disk geometry struct (52 bytes).
//
//	struct LpMetadataGeometry {
//	    uint32_t    magic;              // 0x616c4467
//	    uint32_t    struct_size;        // 52
//	    uint8_t     checksum[32];       // SHA-256(zeroed struct)
//	    uint32_t    metadata_max_size;
//	    uint32_t    metadata_slot_count;
//	    uint32_t    logical_block_size;
//	}; // 52 bytes
type LpMetadataGeometry struct {
	Magic             uint32
	StructSize        uint32
	Checksum          [32]byte
	MetadataMaxSize   uint32
	MetadataSlotCount uint32
	LogicalBlockSize  uint32
}

// LpMetadataTableDescriptor describes a sub-table within the metadata region.
type LpMetadataTableDescriptor struct {
	Offset      uint32
	NumElements uint32
	ElementSize uint32
}

// LpMetadataHeader matches the on-disk metadata header.
//
//	struct LpMetadataHeader {
//	    uint32_t                magic;           // 0x414C5030
//	    uint16_t                major_version;
//	    uint16_t                minor_version;
//	    uint32_t                header_size;
//	    uint8_t                 header_checksum[32];
//	    uint32_t                tables_size;
//	    uint8_t                 tables_checksum[32];
//	    LpMetadataTableDescriptor partitions;
//	    LpMetadataTableDescriptor extents;
//	    LpMetadataTableDescriptor groups;
//	    LpMetadataTableDescriptor block_devices;
//	    uint32_t                flags;           // added in v10.2
//	}; // variable size; header_size holds the actual struct size
type LpMetadataHeader struct {
	Magic          uint32
	MajorVersion   uint16
	MinorVersion   uint16
	HeaderSize     uint32
	HeaderChecksum [32]byte
	TablesSize     uint32
	TablesChecksum [32]byte
	Partitions     LpMetadataTableDescriptor
	Extents        LpMetadataTableDescriptor
	Groups         LpMetadataTableDescriptor
	BlockDevices   LpMetadataTableDescriptor
	Flags          uint32
}

// LpMetadataExtent matches the on-disk extent entry (24 bytes).
//
//	struct LpMetadataExtent {
//	    uint64_t    num_sectors;
//	    uint32_t    target_type;    // 0=linear, 1=zero
//	    uint64_t    target_data;    // sector offset (for linear)
//	    uint32_t    target_source;  // block device index
//	}; // 24 bytes
type LpMetadataExtent struct {
	NumSectors   uint64
	TargetType   uint32
	TargetData   uint64
	TargetSource uint32
}

// LpMetadataPartition matches the on-disk partition entry (48 bytes).
//
//	struct LpMetadataPartition {
//	    char        name[36];       // null-terminated
//	    uint32_t    attributes;
//	    uint32_t    first_extent_index;
//	    uint32_t    num_extents;
//	}; // 48 bytes
type LpMetadataPartition struct {
	Name             [36]byte
	Attributes       uint32
	FirstExtentIndex uint32
	NumExtents       uint32
}

// PartitionInfo is the parsed, user-friendly representation of a partition.
type PartitionInfo struct {
	Name    string
	Size    uint64
	Extents []LpMetadataExtent
}

// ─── SuperImage ───────────────────────────────────────────────────────────

// SuperImage represents a parsed Android super image backed by an io.ReaderAt.
// Unlike the original lpunpack it does NOT require a raw image on disk – it
// can read through a SparseReaderAt that decompresses chunks on the fly.
type SuperImage struct {
	reader     io.ReaderAt
	fileSize   int64
	Partitions []PartitionInfo
	verbose    bool
}

// OpenSuperImage opens and parses a super image from the given reader.
// fileSize must be the size of the raw (de-sparsed) image.
func OpenSuperImage(reader io.ReaderAt, fileSize int64, verbose bool) (*SuperImage, error) {
	sp := &SuperImage{
		reader:   reader,
		fileSize: fileSize,
		verbose:  verbose,
	}
	if err := sp.parse(); err != nil {
		return nil, err
	}
	return sp, nil
}

// ─── Geometry ─────────────────────────────────────────────────────────────

// parseGeometry parses the 52-byte LpMetadataGeometry buffer.
func parseGeometry(buf []byte) (*LpMetadataGeometry, error) {
	if len(buf) < 52 {
		return nil, fmt.Errorf("geometry buffer too small: %d", len(buf))
	}

	g := &LpMetadataGeometry{
		Magic:      binary.LittleEndian.Uint32(buf[0:4]),
		StructSize: binary.LittleEndian.Uint32(buf[4:8]),
	}
	copy(g.Checksum[:], buf[8:40])
	g.MetadataMaxSize = binary.LittleEndian.Uint32(buf[40:44])
	g.MetadataSlotCount = binary.LittleEndian.Uint32(buf[44:48])
	g.LogicalBlockSize = binary.LittleEndian.Uint32(buf[48:52])

	if g.Magic != LPMetadataGeometryMagic {
		return nil, fmt.Errorf(
			"invalid LP geometry magic: 0x%08X (expected 0x%08X)",
			g.Magic, LPMetadataGeometryMagic,
		)
	}
	if g.StructSize != 52 {
		return nil, fmt.Errorf(
			"unexpected LP geometry struct size: %d (expected 52)",
			g.StructSize,
		)
	}

	// Verify SHA-256 checksum over the struct with checksum field zeroed.
	tmp := make([]byte, 52)
	copy(tmp, buf[:52])
	for i := 8; i < 40; i++ {
		tmp[i] = 0
	}
	h := sha256.Sum256(tmp)
	if h != g.Checksum {
		return nil, fmt.Errorf("LP geometry checksum mismatch")
	}

	if g.MetadataSlotCount == 0 {
		return nil, fmt.Errorf("LP geometry slot count is 0")
	}
	if g.MetadataMaxSize == 0 || g.MetadataMaxSize%LPSectorSize != 0 {
		return nil, fmt.Errorf(
			"invalid LP metadata max size: %d",
			g.MetadataMaxSize,
		)
	}
	return g, nil
}

// readGeometryAt reads and parses the geometry struct at the given offset.
func (sp *SuperImage) readGeometryAt(offset int64) (*LpMetadataGeometry, error) {
	buf := make([]byte, 52)
	if _, err := sp.reader.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("reading geometry at %d: %w", offset, err)
	}
	return parseGeometry(buf)
}

// ─── Geometry scan fallback ───────────────────────────────────────────────

var geomMagicPattern = []byte{0x67, 0x44, 0x6c, 0x61} // LE bytes of 0x616c4467

// indexBytes finds the first occurrence of pattern in data, or -1.
func indexBytes(data, pattern []byte) int {
	if len(data) < len(pattern) {
		return -1
	}
	for i := 0; i <= len(data)-len(pattern); i++ {
		match := true
		for j := 0; j < len(pattern); j++ {
			if data[i+j] != pattern[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// scanRegion linearly scans [start, start+length) for the geometry magic,
// returning the first valid geometry found.
func (sp *SuperImage) scanRegion(start, length int64) (*LpMetadataGeometry, int64, error) {
	end := start + length
	if end > sp.fileSize {
		end = sp.fileSize
	}
	pos := start
	overlap := make([]byte, 0, 4)

	for pos < end {
		chunkSize := int64(scanChunkSize)
		if pos+chunkSize > end {
			chunkSize = end - pos
		}
		if chunkSize < 4 {
			break
		}

		buf := make([]byte, chunkSize)
		n, err := sp.reader.ReadAt(buf, pos)
		if err != nil || int64(n) < 4 {
			break
		}
		data := buf[:n]

		searchData := data
		if len(overlap) > 0 {
			searchData = append(overlap, data...)
		}

		idx := 0
		for {
			hit := indexBytes(searchData[idx:], geomMagicPattern)
			if hit < 0 {
				break
			}
			relOff := idx + hit
			absOff := pos + int64(relOff)

			// We need at least 52 readable bytes from here.
			if absOff+52 > sp.fileSize {
				idx = relOff + 1
				continue
			}

			g, err := sp.readGeometryAt(absOff)
			if err == nil {
				return g, absOff, nil
			}
			idx = relOff + 1
		}

		// Keep the last 3 bytes as overlap for the next iteration.
		overlap = overlap[:0]
		if len(data) >= 3 {
			overlap = append(overlap, data[len(data)-3:]...)
		}
		pos += int64(len(data)) - int64(len(overlap))
	}

	return nil, 0, fmt.Errorf("no valid LP geometry found in scan region")
}

// geometryOffset returns the standard offset for the geometry struct
// (either at 4096 or at the end of the first 4096-byte block).
func geometryOffset() int64 {
	return LPMetadataGeometrySize // 4096 bytes from start
}

// findGeometry locates the geometry struct, trying the standard offset first
// and scanning a larger region if that fails.
func (sp *SuperImage) findGeometry() (*LpMetadataGeometry, int64, error) {
	// Try the standard offset first.
	off := geometryOffset()
	g, err := sp.readGeometryAt(off)
	if err == nil {
		if sp.verbose {
			fmt.Fprintf(os.Stderr, "  Geometry at offset %d\n", off)
		}
		return g, off, nil
	}

	// Fall back to scanning.
	if sp.verbose {
		fmt.Fprintf(os.Stderr, "  Geometry not at %d, scanning... (%v)\n", off, err)
	}
	g, off, err = sp.scanRegion(0, scanLimitBytes)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot locate LP geometry: %w", err)
	}
	if sp.verbose {
		fmt.Fprintf(os.Stderr, "  Geometry found at offset %d (scan)\n", off)
	}
	return g, off, nil
}

// ─── Metadata helper ──────────────────────────────────────────────────────

func primaryMetadataOffset(g *LpMetadataGeometry, geoOff int64, slot uint32) int64 {
	start := geoOff + int64(LPMetadataGeometrySize)
	return start + int64(slot)*int64(g.MetadataMaxSize)
}

func backupMetadataOffset(g *LpMetadataGeometry, geoOff int64, slot uint32) int64 {
	start := geoOff + int64(LPMetadataGeometrySize)
	totalSlots := int64(g.MetadataSlotCount)
	return start + totalSlots*int64(g.MetadataMaxSize) + int64(slot)*int64(g.MetadataMaxSize)
}

// ─── Metadata header & table parsing ──────────────────────────────────────

// parseHeader parses the metadata header from buf. The header is variable-size
// (HeaderSize field indicates actual length); we parse the fixed fields and
// use HeaderSize to locate the tables region.
func parseHeader(buf []byte) (*LpMetadataHeader, error) {
	if len(buf) < 64 {
		return nil, fmt.Errorf("metadata buffer too small: %d", len(buf))
	}

	h := &LpMetadataHeader{
		Magic:        binary.LittleEndian.Uint32(buf[0:4]),
		MajorVersion: binary.LittleEndian.Uint16(buf[4:6]),
		MinorVersion: binary.LittleEndian.Uint16(buf[6:8]),
		HeaderSize:   binary.LittleEndian.Uint32(buf[8:12]),
	}
	copy(h.HeaderChecksum[:], buf[12:44])
	h.TablesSize = binary.LittleEndian.Uint32(buf[44:48])
	copy(h.TablesChecksum[:], buf[48:80])
	h.Partitions = LpMetadataTableDescriptor{
		Offset:      binary.LittleEndian.Uint32(buf[80:84]),
		NumElements: binary.LittleEndian.Uint32(buf[84:88]),
		ElementSize: binary.LittleEndian.Uint32(buf[88:92]),
	}
	h.Extents = LpMetadataTableDescriptor{
		Offset:      binary.LittleEndian.Uint32(buf[92:96]),
		NumElements: binary.LittleEndian.Uint32(buf[96:100]),
		ElementSize: binary.LittleEndian.Uint32(buf[100:104]),
	}
	h.Groups = LpMetadataTableDescriptor{
		Offset:      binary.LittleEndian.Uint32(buf[104:108]),
		NumElements: binary.LittleEndian.Uint32(buf[108:112]),
		ElementSize: binary.LittleEndian.Uint32(buf[112:116]),
	}
	h.BlockDevices = LpMetadataTableDescriptor{
		Offset:      binary.LittleEndian.Uint32(buf[116:120]),
		NumElements: binary.LittleEndian.Uint32(buf[120:124]),
		ElementSize: binary.LittleEndian.Uint32(buf[124:128]),
	}

	// Flags field added in v10.2, located at offset 128.
	if h.MinorVersion >= 2 && len(buf) >= 132 {
		h.Flags = binary.LittleEndian.Uint32(buf[128:132])
	}

	if h.Magic != LPMetadataHeaderMagic {
		return nil, fmt.Errorf(
			"invalid LP header magic: 0x%08X (expected 0x%08X)",
			h.Magic, LPMetadataHeaderMagic,
		)
	}
	if h.MajorVersion != LPMetadataMajorVersion {
		return nil, fmt.Errorf(
			"unsupported LP major version: %d (expected %d)",
			h.MajorVersion, LPMetadataMajorVersion,
		)
	}
	if h.MinorVersion < LPMetadataMinorVersionMin || h.MinorVersion > LPMetadataMinorVersionMax {
		return nil, fmt.Errorf(
			"unsupported LP minor version: %d (expected %d–%d)",
			h.MinorVersion, LPMetadataMinorVersionMin, LPMetadataMinorVersionMax,
		)
	}
	if int64(h.HeaderSize) < 64 {
		return nil, fmt.Errorf("LP header size too small: %d", h.HeaderSize)
	}

	return h, nil
}

// readMetadataAt reads and parses partition metadata at the given offset.
func (sp *SuperImage) readMetadataAt(g *LpMetadataGeometry, offset int64) ([]PartitionInfo, error) {
	buf := make([]byte, g.MetadataMaxSize)
	if _, err := sp.reader.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("reading metadata at %d: %w", offset, err)
	}

	h, err := parseHeader(buf)
	if err != nil {
		return nil, err
	}

	// Tables start at HeaderSize within the metadata region.
	tblOff := int64(h.HeaderSize)
	if tblOff+int64(h.TablesSize) > int64(g.MetadataMaxSize) {
		return nil, fmt.Errorf(
			"LP tables (%d bytes at %d) exceed metadata max size (%d)",
			h.TablesSize, tblOff, g.MetadataMaxSize,
		)
	}
	td := buf[tblOff : tblOff+int64(h.TablesSize)]

	// Verify tables checksum.
	sum := sha256.Sum256(td)
	if sum != h.TablesChecksum {
		return nil, fmt.Errorf("LP tables checksum mismatch")
	}

	if sp.verbose {
		fmt.Fprintf(os.Stderr, "  Metadata v%d.%d: %d partitions, %d extents\n",
			h.MajorVersion, h.MinorVersion,
			h.Partitions.NumElements, h.Extents.NumElements)
	}

	if h.BlockDevices.NumElements == 0 {
		return nil, fmt.Errorf("LP metadata has no block devices")
	}

	// Parse extents.
	extents := make([]LpMetadataExtent, h.Extents.NumElements)
	for i := uint32(0); i < h.Extents.NumElements; i++ {
		eo := int64(h.Extents.Offset) + int64(i)*int64(h.Extents.ElementSize)
		if eo+24 > int64(len(td)) {
			return nil, fmt.Errorf("LP extent %d truncated", i)
		}
		extents[i] = LpMetadataExtent{
			NumSectors:   binary.LittleEndian.Uint64(td[eo : eo+8]),
			TargetType:   binary.LittleEndian.Uint32(td[eo+8 : eo+12]),
			TargetData:   binary.LittleEndian.Uint64(td[eo+12 : eo+20]),
			TargetSource: binary.LittleEndian.Uint32(td[eo+20 : eo+24]),
		}
		if extents[i].TargetType == LPTargetTypeLinear &&
			extents[i].TargetSource >= h.BlockDevices.NumElements {
			return nil, fmt.Errorf(
				"LP extent %d references invalid block device %d",
				i, extents[i].TargetSource,
			)
		}
	}

	// Parse partitions.
	var parts []PartitionInfo
	for i := uint32(0); i < h.Partitions.NumElements; i++ {
		po := int64(h.Partitions.Offset) + int64(i)*int64(h.Partitions.ElementSize)
		if po+48 > int64(len(td)) {
			return nil, fmt.Errorf("LP partition %d truncated", i)
		}

		var part LpMetadataPartition
		copy(part.Name[:], td[po:po+36])
		part.Attributes = binary.LittleEndian.Uint32(td[po+36 : po+40])
		part.FirstExtentIndex = binary.LittleEndian.Uint32(td[po+40 : po+44])
		part.NumExtents = binary.LittleEndian.Uint32(td[po+44 : po+48])

		name := cString(part.Name[:])
		if name == "" {
			continue
		}
		if int(part.FirstExtentIndex+part.NumExtents) > len(extents) {
			return nil, fmt.Errorf(
				"partition %s: extent range exceeds extent table", name,
			)
		}
		if part.NumExtents == 0 {
			continue
		}

		var pExts []LpMetadataExtent
		var pSize uint64
		for j := part.FirstExtentIndex; j < part.FirstExtentIndex+part.NumExtents; j++ {
			e := extents[j]
			if e.TargetType == LPTargetTypeLinear {
				pExts = append(pExts, e)
				pSize += e.NumSectors * LPSectorSize
			}
		}

		if part.Attributes&LPPartitionAttrSlotSuffixed != 0 {
			name += "_a"
		}

		if pSize > 0 {
			parts = append(parts, PartitionInfo{
				Name:    name,
				Size:    pSize,
				Extents: pExts,
			})
		}
	}

	if len(parts) == 0 {
		return nil, fmt.Errorf("no valid partitions found in LP metadata")
	}
	return parts, nil
}

// findMetadata tries to load the partition table from primary metadata,
// falling back to the backup slot.
func (sp *SuperImage) findMetadata(g *LpMetadataGeometry, geoOff int64) ([]PartitionInfo, error) {
	slot := uint32(0)

	// Primary.
	off := primaryMetadataOffset(g, geoOff, slot)
	if sp.verbose {
		fmt.Fprintf(os.Stderr, "  Primary metadata offset: %d (%.2f MB)\n",
			off, float64(off)/1048576)
	}
	parts, err := sp.readMetadataAt(g, off)
	if err == nil {
		return parts, nil
	}
	if sp.verbose {
		fmt.Fprintf(os.Stderr, "  Primary metadata failed: %v\n", err)
	}

	// Backup.
	off = backupMetadataOffset(g, geoOff, slot)
	if sp.verbose {
		fmt.Fprintf(os.Stderr, "  Backup metadata offset: %d (%.2f MB)\n",
			off, float64(off)/1048576)
	}
	parts, err = sp.readMetadataAt(g, off)
	if err == nil {
		return parts, nil
	}
	return nil, fmt.Errorf(
		"no valid partition table in primary or backup metadata: %v", err,
	)
}

// ─── Top-level parse ───────────────────────────────────────────────────────

func (sp *SuperImage) parse() error {
	g, geoOff, err := sp.findGeometry()
	if err != nil {
		return err
	}
	if sp.verbose {
		fmt.Fprintf(os.Stderr, "  Geometry offset: %d, maxSize=%d slots=%d blockSize=%d\n",
			geoOff, g.MetadataMaxSize, g.MetadataSlotCount, g.LogicalBlockSize)
	}
	parts, err := sp.findMetadata(g, geoOff)
	if err != nil {
		return err
	}
	sp.Partitions = parts
	return nil
}

// ─── Partition Extraction ──────────────────────────────────────────────────

// ExtractPartitions writes every partition in the super image to outDir as
// individual .img files.
func (sp *SuperImage) ExtractPartitions(outDir string) error {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	extracted := 0
	for _, part := range sp.Partitions {
		if sp.verbose {
			fmt.Printf("Extracting %s (%s)...\n", part.Name, formatSize(int64(part.Size)))
		}
		if err := sp.extractOne(part, outDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", part.Name, err)
			continue
		}
		extracted++
	}

	if sp.verbose {
		fmt.Printf("\nExtracted %d partition(s) to %s\n", extracted, outDir)
	}
	return nil
}

// extractOne writes a single partition to a .img file in outDir.
func (sp *SuperImage) extractOne(part PartitionInfo, outDir string) error {
	outPath := filepath.Join(outDir, part.Name+".img")
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("cannot create %s: %w", outPath, err)
	}
	defer out.Close()

	if part.Size > 0 {
		_ = out.Truncate(int64(part.Size))
	}

	var written int64
	for idx, ext := range part.Extents {
		if ext.TargetType != LPTargetTypeLinear {
			continue
		}
		physOff := int64(ext.TargetData * LPSectorSize)
		dataSize := int64(ext.NumSectors * LPSectorSize)
		if dataSize == 0 {
			continue
		}

		// Seek to the correct write position (supports sparse output for
		// non-contiguous extents).
		if _, err := out.Seek(written, io.SeekStart); err != nil {
			return fmt.Errorf("extent %d: seek output: %w", idx, err)
		}

		// io.SectionReader handles the read from our ReaderAt, which may be
		// a SparseReaderAt doing on-the-fly de-sparsing.  No intermediate
		// raw file is needed.
		n, err := io.CopyN(out, io.NewSectionReader(sp.reader, physOff, dataSize), dataSize)
		if err != nil {
			return fmt.Errorf("extent %d: copy: %w", idx, err)
		}
		if n != dataSize {
			return fmt.Errorf("extent %d: short write (%d != %d)", idx, n, dataSize)
		}
		written += n
	}

	return out.Sync()
}

// ListPartitions prints a summary of all partitions found in the super image.
func (sp *SuperImage) ListPartitions() {
	fmt.Println("Partitions in super image:")
	for _, p := range sp.Partitions {
		fmt.Printf("  %-40s %s\n", p.Name+":", formatSize(int64(p.Size)))
	}
	fmt.Printf("Total: %d partition(s)\n", len(sp.Partitions))
}

// ─── Utilities ─────────────────────────────────────────────────────────────

// cString converts a null-padded byte slice to a trimmed Go string.
func cString(buf []byte) string {
	n := len(buf)
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return strings.TrimRight(string(buf[:i]), "\x00")
		}
	}
	return strings.TrimRight(string(buf), "\x00")
}

// formatSize returns a human-readable representation of a byte count.
func formatSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
