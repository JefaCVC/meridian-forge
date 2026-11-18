# Meridian Forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/JefaCVC/meridian-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/JefaCVC/meridian-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single process: smart-HTTP git
(upload-pack protocol v2, receive-pack v0), REST v3, GraphQL v4, and an MCP
server — all backed by one deterministic in-memory store.

Built for the [make.fun](https://make.fun) `github-core-clone-01` bounty.
The entrypoint is `main.ts`, a thin Node 22 launcher that spawns the vendored
statically linked Go binary (`forge-linux-amd64`).

## Run it

```sh
# standalone (local testing): binds an ephemeral port and serves until killed
PORT=8080 MAKEFUN_SHOP_TOKEN=secret ./forge-linux-amd64

# or through the launcher, exactly as the trial runs it
node main.ts
```

In a make.fun trial the binary detects `MAKEFUN_ENV_URL`, binds port 0,
registers its port at `$MAKEFUN_ENV_URL/register`, serves, and exits 0 when
`$MAKEFUN_ENV_URL/done` reports done.

Health probe: `GET /healthz` → `200 {"ok": true}` (no auth).

## Surfaces

- **Git smart HTTP** — `/{owner}/{repo}.git/info/refs`,
  `git-upload-pack` (protocol v2: `ls-refs`, `fetch` with shallow/deepen,
  plus the v0 fetch path) and `git-receive-pack` (v0, report-status,
  side-band-64k, fast-forward enforcement, `non-fast-forward` refusals).
  Packfiles are read with full OFS_DELTA/REF_DELTA resolution and written as
  deterministic undeltified PACK v2 (sorted by oid).
- **REST v3** — `/api/v3`: repos (CRUD, paginated `Link` headers), branches,
  commits, git refs, contents (create 201 / update 200 / stale-sha 409),
  labels, issues (with comments), pull requests (squash merge, 405 on
  conflicts). Every reply is JSON with `X-GitHub-Request-Id`.
- **GraphQL v4** — `/api/graphql`: a small parser + executor for the query
  and mutation shapes `gh` and github-mcp-server send, including `node(id:)`,
  `__type` introspection, connections with cursor pagination, and batch
  requests.
- **MCP** — `/mcp`: JSON-RPC 2.0, era `2026-07-28` (`server/discover`,
  `initialize`, `tools/list`, `tools/call`) with six tools: `create_repo`,
  `get_repo`, `create_issue`, `list_issues`, `create_pull_request`,
  `merge_pull_request`.

## Design notes

- **In-memory only.** No disk state; the whole store is one `sync.RWMutex`
  guarding repos, objects, refs, issues and pulls. Reads share the read lock;
  the clone path serves cached fetch/ls-refs/advertisement bytes per repo
  generation, and issue reads serve pre-serialized JSON.
- **Deterministic.** No wall clock in any state or response: timestamps come
  from `MAKEFUN_TRIAL_NOW` (or a fixed epoch) plus a monotonic counter. Ids
  are sequential counters; map iteration is always sorted before serializing,
  so replaying a request sequence reproduces state byte for byte.
- **Auth.** Every request except `/healthz` needs
  `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>` (401 otherwise). A repo under
  another owner is 404, never 403.

## Build

```sh
cd go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ../forge-linux-amd64 .
```

The binary is statically linked, ~6 MiB, linux/amd64.

## Layout

```
main.ts            entrypoint launcher (Node 22, stdlib only)
forge-linux-amd64  compiled static binary (built from go/)
go/
  main.go          server, routing, git HTTP handlers, lifecycle
  store.go         in-memory store, repos, issues, pulls
  gitobj.go        objects, packfile read/write, delta application
  gitproto.go      pkt-lines, advertisements, ls-refs, fetch, receive-pack
  rest.go          REST v3
  graphql.go       GraphQL parser + executor
  mcp.go           MCP JSON-RPC
```

## License

MIT — see [LICENSE](LICENSE).
