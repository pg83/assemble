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

## Source fetcher

```sh
assemble fetch -mirrors 'https://mirror.example/{two}/{sha}' \
    -socks5 '127.0.0.1:1082;127.0.0.1:1083' URL PATH SHA
```

`-mirrors` takes newline-separated URL templates (`{sha}`, `{two}`, `{one}`);
`-socks5` takes semicolon-separated proxies. IX generates a small `fetcher`
shell wrapper that supplies these settings and keeps the old `URL PATH SHA`
interface. Downloads, TLS and SOCKS run in-process using the Go standard
library, with no curl or Python dependency. SOCKS uses **remote DNS**.

The retry policy is ported from IX's Python fetcher without changes:

- Shuffle mirrors once, then construct an insertion-ordered, deduplicated URL
  set with the origin last (an origin/mirror collision keeps the first position
  but is treated as the origin). Each URL cycle snapshots the remaining set.
- Independently cycle configured proxies in order, then the direct route.
  Zip this stream with URLs; do not try every proxy for each URL or reset the
  proxy position when removing a source.
- Start at 60 seconds; multiply by 1.5 after every attempt, capped at 10000
  **before** jitter. Multiply by `0.5 + random()` and truncate to whole seconds.
  The connection budget is `int(0.1 * timeout)` seconds, including DNS, TCP,
  proxy and TLS. The total timeout covers the download, not the subsequent SHA
  check. No retry limit or extra sleep is added.
- Remove sources on 404, and mirrors on checksum mismatch. An origin checksum
  mismatch is fatal. All other failures keep cycling; exhaustion is fatal.
- Preserve the `sha:` prefix, `__skip__` marker, all-ones hash behavior, GHCR
  bearer header, curl's 50-redirect limit and `-k` TLS policy. Disable transparent
  HTTP decompression so checksums apply to the original bytes.

As before, a path with a parent directory designates a disposable fetch output
directory, cleaned before each attempt. A bare filename does not clean the cwd.
Root/current-directory cleanup and directory symlinks are rejected.

IX retains the original Python implementation only in `bld/fetch/bootstrap`
for hosts without `/bin/fetcher`, avoiding an assemble -> Go vendoring ->
fetcher -> assemble bootstrap dependency cycle.

## Build

```
CGO_ENABLED=0 go build -o assemble_ng .
```

Run tests without inheriting the host's production package-cache endpoints:

```sh
env -u IX_PACKAGE_CACHE go test ./...
```
