# lucky-arch

Extract partitions from Android sparse `super.img` **on the fly** — without writing an intermediate raw file to disk.

Combines `simg2img` + `lpunpack` into a single pure-Go binary with zero intermediate I/O.

## Usage

```bash
lucky-arch [-v] [-l] [-o output_dir] super.img
```

### Examples

```bash
# Extract all partitions
lucky-arch super.img

# Verbose mode
lucky-arch -v super.img

# Custom output directory
lucky-arch -v -o partitions/ super.img

# List partitions only
lucky-arch -l super.img
```

## How it works

1. Detects if the input is sparse or raw automatically
2. For sparse images, builds an in-memory chunk index (no full decompression)
3. Parses LP metadata (AOSP liblp format) to find partitions and extents
4. Extracts partitions directly using `io.SectionReader` — reads only the needed bytes from the sparse file

### Supported formats

- **Input**: Android sparse `super.img` (raw or sparse)
- **Output**: Individual `.img` files per partition (system, vendor, product, etc.)

## Build

```bash
git clone https://github.com/soe1hom-arch/lucky-arch.git
cd lucky-arch
go build -o lucky-arch .
```

## License

Apache License 2.0
