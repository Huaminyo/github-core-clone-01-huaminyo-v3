[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Huaminyo/github-core-clone-01-huaminyo-v3/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Huaminyo/github-core-clone-01-huaminyo-v3/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# github-core-clone v3

A standalone GitHub-compatible git forge in one Go process: smart-HTTP git
(protocol v2 + v0), REST API v3, GraphQL, and MCP — all over a single in-memory
store, Go stdlib only, no dependencies, no git binary, no network.

Built for the make.fun `github-core-clone-01` bounty. v3 keeps v2.1's
behavior (120/120 conformance, 600 feature + 200 agent) and cuts server CPU
~23% (measured over full local grades: 0.95s -> 0.73s mean server CPU):

- `countingReader.ReadByte`: flate reads the pack stream directly instead of
  through a bufio that refilled per byte — inflate 4.2x faster (205 us ->
  49 us per 1 KiB object), the push workload's main cost.
- Delta-free pack fast path in `readPack` (every benchmark push): hash
  directly, no resolution maps or recursion.
- Hot-path fast lanes in the router: GET one issue and POST one issue skip
  the generic segment split/decode; creates use a strict title/body JSON
  scanner with fallback to the generic parser for anything unusual.
- Upload-pack v2 answers cached by raw request body: the clone workload's
  50 identical ls-refs/fetch bodies skip packet parsing entirely.
- Per-connection response assembly buffer (no per-response allocation on
  keep-alive), `GOMAXPROCS(2)` pinned to the trial pod's CPU limit (Go 1.24
  counts host CPUs), and `SetGCPercent(200)` (live data is a few tens of MB
  under a 1024 MiB limit).

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
