# fvs-v2-core

The engine behind [FVS (Fused Versioned Storage)](https://github.com/fvs-lab/fvs2):
a small, dependency-light Go core for content-addressed, deduplicated,
copy-on-write storage.

## What's inside

- **Content-addressed blocks (BLAKE3).** Every block is named by the hash of its
  content, so identical data is stored once (deduplication).
- **Disk and in-memory block stores (CAS).** Atomic writes, and reads are
  **re-verified** against the content hash, so tampering or bit-rot surfaces as
  `ErrBlockCorrupt` instead of returning bad data.
- **Copy-on-write file abstraction.** Fixed-size blocks mapped through a CoW
  map, with snapshot isolation. The building block for snapshots and the FUSE
  write path.

## Usage

```go
store, _ := core.NewDiskBlockStore(".fvs2/blocks")

id, _ := store.Put([]byte("hello"))   // content-addressed; same content, same id
data, _ := store.Get(id)              // re-hashed on read; ErrBlockCorrupt on tamper
```

`CoWFile` builds on a `BlockStore` plus a `CoWMap` to give a block-addressed,
copy-on-write file with `ReadAt` / `WriteAt` and immutable `Snapshot()` views.

## Platforms

The storage engine builds on Linux and macOS without FUSE, Swift or an Apple
SDK. Linux-only reflink and `syncfs` calls have separate build tags. macOS uses
the existing copy and per-file synchronization paths, POSIX advisory locks
and atomic renames. File synchronization errors are propagated; the port adds
no new power-loss guarantee or repository format. Native mounts are provided
by [fvs2d](https://github.com/fvs-lab/fvs2d), outside this storage module.

## Test

```bash
go test ./...
```

## License

MIT. See [LICENSE](LICENSE).
