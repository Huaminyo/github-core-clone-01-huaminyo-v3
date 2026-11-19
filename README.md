# github-core-clone v2

A standalone GitHub-compatible git forge in one Go process: smart-HTTP git
(protocol v2 + v0), REST API v3, GraphQL, and MCP — all over a single in-memory
store, Go stdlib only, no dependencies, no git binary, no network.

Built for the make.fun `github-core-clone-01` bounty. v2 is a full Go port of
the v1 TypeScript entry (same behavior, 120/120 conformance): the trial pods
run gVisor, which slows Node.js ~2x, while a native static binary keeps its
speed. The hot paths are allocation-light on top of that: pre-serialized
response caches with generation-based invalidation, per-request fetch-pack
caching, pooled zlib inflate/deflate, and a hand-rolled HTTP/1.1 server over
raw TCP.

## Run

```sh
# In a trial the harness runs the entrypoint; it registers with the env and exits when told.
node main.ts

# Locally, for the SDK runner (same binary, fixed port, no lifecycle):
MAKEFUN_SHOP_TOKEN=test-token PORT=8080 ./forge
```

`main.ts` is a thin launcher: it execs the vendored static binary `./forge`
(directly when /bot allows execution, otherwise from a temp copy — see the
fallback ladder in main.ts) and forwards stdio, environment and signals.
`go/main.go` holds the env lifecycle: bind an ephemeral port, POST it to
`$MAKEFUN_ENV_URL/register`, then poll `$MAKEFUN_ENV_URL/done` until the
trial ends.

## Layout

| File | What it is |
| --- | --- |
| `main.ts` | The entrypoint: launches the `forge` binary. |
| `forge` | Static Linux amd64 binary, built from `./go` (`CGO_ENABLED=0`, `-ldflags="-s -w"`). |
| `go/main.go` | Env lifecycle (bind, register, wait for done) + dev mode. |
| `go/app.go` | The HTTP server: routing, auth, and the whole REST v3 surface. |
| `go/store.go` | The forge: git objects, packfiles, trees, diffs and merges, plus repos, issues, pull requests, labels and comments. |
| `go/git.go` | Git smart-HTTP: v0/v2 advertisements, `ls-refs`, `fetch`, `receive-pack`. |
| `go/graphql.go` | The GraphQL endpoint (schema, queries, mutations). |
| `go/mcp.go` | The MCP endpoint (tools/list, tools/call). |
| `go/json.go` | Hand-rolled JSON writers for the hot REST views. |
| `go/scenario.go` | The deterministic scenario RNG (viewer identity, seed data). |

## Build

```sh
cd go
CGO_ENABLED=0 go build -ldflags="-s -w" -o ../forge .
```

No Go module dependencies; `go.mod` exists only to name the module.
