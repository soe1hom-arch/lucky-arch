// Copyright 2026 soe1hom-arch
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

const version = "1.0.0"

func main() {
	var (
		verbose  bool
		listOnly bool
		outDir   string
		showVer  bool
		toRaw    bool
	)

	flag.BoolVar(&verbose, "v", false, "Verbose output (shorthand)")
	flag.BoolVar(&verbose, "verbose", false, "Verbose output")
	flag.BoolVar(&listOnly, "l", false, "List partitions only, do not extract")
	flag.BoolVar(&listOnly, "list", false, "List partitions only, do not extract")
	flag.StringVar(&outDir, "o", "", "Output directory (default: input filename without extension)")
	flag.StringVar(&outDir, "output", "", "Output directory")
	flag.BoolVar(&showVer, "version", false, "Show version")
	flag.BoolVar(&toRaw, "to-raw", false, "Convert sparse image to raw (simg2img mode)")
	flag.BoolVar(&toRaw, "raw", false, "Shorthand for --to-raw")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `lucky-arch v%s — Extract partitions from Android sparse super.img on the fly.

Usage:
  lucky-arch [options] <super_image>
  lucky-arch --to-raw <sparse_image> [output_raw]

Converts a sparse Android super image to raw in memory and extracts all
logical partitions (system, vendor, product, etc.) without writing an
intermediate raw file to disk.

Options:
  -o, -output <dir>   Output directory (default: <name>.parts/)
  -v, -verbose        Show detailed progress and metadata info
  -l, -list           List partitions and exit without extracting
  -version            Show version and exit
  --to-raw, --raw     Convert sparse image to raw (simg2img mode)

Examples:
  lucky-arch super.img
  lucky-arch -v -o my_parts/ super.img
  lucky-arch -l super.img
  lucky-arch --to-raw system.img system_raw.img
`, version)
	}

	flag.Parse()

	if showVer {
		fmt.Printf("lucky-arch v%s\n", version)
		return
	}

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(1)
	}

	input := flag.Arg(0)

	// ── Sparse-to-raw mode ───────────────────────────────────────────

	if toRaw {
		output := deriveRawOutput(input)
		if err := sparseToRaw(input, output, verbose); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if !verbose {
			fmt.Println(output)
		}
		return
	}

	// ── Open input file ──────────────────────────────────────────────

	f, err := os.Open(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot open %s: %v\n", input, err)
		os.Exit(1)
	}

	// ── Detect format & create reader ────────────────────────────────

	var (
		reader      io.ReaderAt
		rawSize     int64
		sparseOwned bool // true if a SparseReaderAt owns the file
	)

	isSparse, err := IsSparseImage(f)
	if err != nil {
		f.Close()
		fmt.Fprintf(os.Stderr, "Error: cannot read %s: %v\n", input, err)
		os.Exit(1)
	}

	if isSparse {
		if verbose {
			fmt.Fprintf(os.Stderr, "Detected sparse image; building chunk index...\n")
		}
		sr, err := NewSparseReaderAt(f)
		if err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "Error: sparse parse failed: %v\n", err)
			os.Exit(1)
		}
		// SparseReaderAt now owns f — it will close it via sr.Close().
		sparseOwned = true
		reader = sr
		rawSize = sr.Size()

		if verbose {
			fmt.Fprintf(os.Stderr, "  Raw image size: %d bytes (%s)\n",
				rawSize, formatSize(rawSize))
			fmt.Fprintf(os.Stderr, "  Chunks indexed: %d\n", len(sr.chunks))
		}
	} else {
		// Already raw — use the file directly.
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "Error: cannot stat %s: %v\n", input, err)
			os.Exit(1)
		}
		reader = f
		rawSize = fi.Size()

		if verbose {
			fmt.Fprintf(os.Stderr, "Detected raw image (%s)\n", formatSize(rawSize))
		}
	}

	// At this point, if sparseOwned is false, we still need to close f
	// ourselves when done.  Use a finaliser.
	if !sparseOwned {
		defer f.Close()
	}

	// ── Parse LP metadata ───────────────────────────────────────────

	if verbose {
		fmt.Fprintf(os.Stderr, "\nParsing LP metadata...\n")
	}

	super, err := OpenSuperImage(reader, rawSize, verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: LP metadata parse failed: %v\n", err)
		os.Exit(1)
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "\n")
	}

	// ── List or extract ──────────────────────────────────────────────

	if listOnly {
		super.ListPartitions()
		return
	}

	if outDir == "" {
		// Derive output directory from input filename.
		outDir = stripExt(input) + ".parts"
	}

	if verbose {
		fmt.Printf("Super Image: %s (%s)\n", input, formatSize(rawSize))
		fmt.Printf("  Partitions: %d\n\n", len(super.Partitions))
	}

	if err := super.ExtractPartitions(outDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error: extraction failed: %v\n", err)
		os.Exit(1)
	}

	if !verbose {
		fmt.Printf("Extracted %d partition(s) to %s/\n", len(super.Partitions), outDir)
	}
}

// destripExt returns the file path without its last extension.
// e.g. "super.img" → "super", "path/to/super.raw.img" → "path/to/super.raw"
func stripExt(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[:i]
		}
		if path[i] == os.PathSeparator {
			break
		}
	}
	return path
}

// deriveRawOutput determines the output raw file path.
// If a second positional arg is given, use it.
// Otherwise, default to <input>.raw
func deriveRawOutput(input string) string {
	if flag.NArg() >= 2 {
		return flag.Arg(1)
	}
	return input + ".raw"
}

// sparseToRaw converts a sparse Android image to raw format.
// Works for any sparse image (super.img, system.img, vendor.img, etc.),
// not just super images with LP metadata.
func sparseToRaw(input, output string, verbose bool) error {
	f, err := os.Open(input)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", input, err)
	}
	defer f.Close()

	isSparse, err := IsSparseImage(f)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", input, err)
	}

	if !isSparse {
		if verbose {
			fmt.Fprintf(os.Stderr, "Not a sparse image; copying directly...\n")
		}
		// Raw image — just copy.
		out, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("cannot create %s: %w", output, err)
		}
		defer out.Close()
		_, err = io.Copy(out, f)
		return err
	}

	// Sparse image — use SparseReaderAt to de-sparse on the fly.
	if verbose {
		fmt.Fprintf(os.Stderr, "Detected sparse image; building chunk index...\n")
	}

	sr, err := NewSparseReaderAt(f)
	if err != nil {
		return fmt.Errorf("sparse parse failed: %w", err)
	}
	defer sr.Close()

	rawSize := sr.Size()
	if verbose {
		fmt.Fprintf(os.Stderr, "  Raw image size: %d bytes (%s)\n",
			rawSize, formatSize(rawSize))
		fmt.Fprintf(os.Stderr, "  Writing raw image to %s...\n", output)
	}

	out, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("cannot create %s: %w", output, err)
	}
	defer out.Close()

	// Pre-allocate for performance.
	_ = out.Truncate(rawSize)

	// Copy via io.Copy using a SectionReader over the full range.
	_, err = io.Copy(out, io.NewSectionReader(sr, 0, rawSize))
	if err != nil {
		return fmt.Errorf("conversion failed: %w", err)
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "Output: %s (%s)\n", output, formatSize(rawSize))
	}
	return nil
}
