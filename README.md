# assemble

IX build-graph executor (next generation). Started as a copy of
`ix/pkgs/bin/assemble/as.go`; will absorb the per-node sandbox
(tmpfs / overlayfs / namespaces) that today lives in the
`tmpfs` / `confine` / `jail` wrappers, so build commands stop
carrying isolation in their argv (which polluted node uid hashes
and made the same package hash differently when built locally
vs. via `assemble` vs. via `molot`).

Reads the same JSON graph shape as the old `assemble`:

```
{
    "nodes": [{"in_dir": [...], "out_dir": [...], "cmd": [...], "pool": "..."}, ...],
    "targets": [...],
    "pools": {"<pool>": <slots>, ...}
}
```

Per-node `tmpfs` flag (planned) toggles the sandbox off for nodes
that need to write into real `/ix/store` (e.g. fetch nodes with
content-addressed predict outputs).

IX consumes this binary as `assemble_ng` via the
`pkgs/bin/assemble/ng` package; old `pkgs/bin/assemble/` will be
retired once the new flow is proven.

## Build

```
go build -o assemble_ng .
```
