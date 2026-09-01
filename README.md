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

Execution is fail-fast by default. Set `IX_KEEP_GOING=yes` to finish
independent graph branches after a node fails; dependent nodes are skipped.

Per-node `tmpfs` flag (planned) toggles the sandbox off for nodes
that need to write into real `/ix/store` (e.g. fetch nodes with
content-addressed predict outputs).

## Molot package cache

If the system environment defines `IX_PACKAGE_CACHE` as a comma-separated
list of `host:port` endpoints, assemble sends the complete graph uid list to
one available Molot cache backend before execution:

```sh
export IX_PACKAGE_CACHE=10.0.0.64:8054,10.0.0.68:8054,10.0.0.72:8054
```

Nodes reported by `POST /v1/resolve` are restored from
`GET /v1/blob/<uid>` before dependency traversal, so a cache hit prunes its
entire input subgraph. Downloads use the graph's `network` pool. Network and
5xx failures retry forever while cycling endpoints; a 404 removes that
endpoint for the uid, and 404 from every endpoint fails the node without a
local rebuild fallback. Tar+zstd extraction is implemented in-process.

IX consumes this binary as `assemble_ng` via the
`pkgs/bin/assemble/ng` package; old `pkgs/bin/assemble/` will be
retired once the new flow is proven.

## Build

```
go build -o assemble_ng .
```
