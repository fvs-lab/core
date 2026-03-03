# fvs-v2-core

Go core library for FVS v2.

## What’s inside

- Content-addressed blocks (BLAKE3)
- Disk BlockStore (CAS) implementation
- CoW file abstraction used by higher layers (CLI snapshots, FUSE write path)

## Test

```bash
go test ./...
```
