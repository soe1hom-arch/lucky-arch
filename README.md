# lucky-arch

Extract partitions from Android sparse `super.img` **on the fly** — without writing an intermediate raw file to disk.

Combines `simg2img` + `lpunpack` into a single pure-Go binary with zero intermediate I/O.

Download pre-built binaries from [GitHub Releases](https://github.com/soe1hom-arch/lucky-arch/releases).

## Usage

```
lucky-arch [options] <super_image>
```

### Options

| Flag | Description |
|------|-------------|
| `-o`, `-output <dir>` | Output directory (default: `<name>.parts/`) |
| `-v`, `-verbose` | Show detailed progress and metadata info |
| `-l`, `-list` | List partitions and exit without extracting |
| `-version` | Show version and exit |

### Examples

```bash
# Extract all partitions to super.parts/
lucky-arch super.img

# Verbose mode
lucky-arch -v super.img

# Custom output directory
lucky-arch -v -o partitions/ super.img

# List partitions only (no extraction)
lucky-arch -l super.img
```

## How it works

1. **Auto-detect** — checks if input is sparse (`0xED26FF3A`) or raw
2. **On-the-fly de-sparse** — for sparse images, builds an in-memory chunk index (no full decompression to disk)
3. **Parse LP metadata** — reads geometry + partition table (AOSP liblp v10.x format)
4. **Direct extraction** — copies only the needed bytes using `io.SectionReader`, handling both RAW/FILL/DONTCARE sparse chunks transparently

### Supported formats

- **Input**: Android `super.img` (raw or sparse)
- **Output**: Individual `.img` files per partition (e.g. `system_a.img`, `vendor_a.img`, `product_a.img`)

> Only slot 0 (typically the active `_a` slot) is extracted by default.
> Partitions with the `slot_suffixed` attribute automatically get `_a` appended.

### Output directory

By default the output directory is the input filename without extension + `.parts/`:
- `super.img` → `super.parts/`
- `super.raw.img` → `super.raw.parts/`

Use `-o dir/` to override.

## Troubleshooting

| Error | Cause & Solution |
|-------|------------------|
| `invalid LP header magic: 0x00000000` | Metadata offset mismatch. Update to v1.0.1+ which fixes this. |
| `no valid geometry found` | File is not a valid super image, or is in an unsupported format. |
| `sparse: raw-size mismatch` | Corrupted sparse image. Try `simg2img` first to validate. |
| `permission denied` | Make sure output directory is writable. |

## Build

```bash
git clone https://github.com/soe1hom-arch/lucky-arch.git
cd lucky-arch
go build -o lucky-arch .

# Cross-compile for ARM64 (Android Termux)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o lucky-arch .
```

## Related

- [simg2img](https://github.com/soe1hom-arch/simg2img) — sparse to raw converter (standalone)
- [lpunpack](https://github.com/soe1hom-arch/lpunpack) — LP partition extractor (standalone)

## License

Apache License 2.0
