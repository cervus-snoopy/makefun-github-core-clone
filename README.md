[![make.fun](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/cervus-snoopy/makefun-github-core-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/cervus-snoopy/makefun-github-core-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# github-core-clone-01

A single-process, single-binary GitHub-compatible git forge, written
from scratch in Go with zero dependencies. All state lives in
memory; there is no on-disk repository, no database and no git
binary involved.

Bounty: https://make.fun/bounty/github-core-clone-01

## What it serves

- **Git smart HTTP** — `git clone` / `git fetch` / `git push` over
  HTTP, protocol v0 advertisements and protocol v2 `ls-refs` /
  `fetch`, plus `receive-pack`. Packs are read and written as byte
  streams (plain packs, no deltas on write; OFS/REF deltas and
  thin packs supported on read).
- **REST API v3** under `/api/v3` — repositories, branches, git
  refs, commits, file contents, labels, issues + comments, pull
  requests + squash merge.
- **GraphQL** at `/api/graphql` — a hand-written parser and
  executor for the Repository / Issue / PullRequest surface, with
  connections, cursor pagination, introspection and mutations.
- **MCP** at `/mcp` — JSON-RPC 2.0 with six tools
  (`create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`).
- **Health** at `GET /healthz`.

Every stored timestamp is the fixed epoch `2025-01-01T00:00:00Z`,
so replaying the same operations produces byte-identical state.

## Build and run

```sh
npm run build   # CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
npm start       # node --experimental-strip-types main.ts
```

`main.ts` is a thin Node launcher: it copies `./forge` to a fresh
file in the OS temp directory, chmods it `0755`, and spawns the
copy. That survives hosts that strip the execute bit from uploaded
files. If the copy fails it falls back to spawning `./forge`
directly. Stdio, environment, exit code and signals are forwarded.

The forge binds an ephemeral port, registers it with
`POST $MAKEFUN_ENV_URL/register`, serves until
`GET $MAKEFUN_ENV_URL/done` answers `{"done": true}`, then exits 0.
`MAKEFUN_ENV_URL` is required; `MAKEFUN_SHOP_TOKEN`, when set,
is the Bearer token every route except `GET /healthz` requires.

## Layout

| File | Contents |
|---|---|
| `main.go` | Lifecycle, raw TCP HTTP/1.1 server, dispatch pipeline |
| `gitobj.go` | Git objects, pack read/write, deltas, pkt-lines, sideband |
| `git.go` | Smart HTTP: advertisements, ls-refs, fetch, receive-pack |
| `store.go` | In-memory store, graph algorithms, merge, JSON builders |
| `rest.go` | REST v3: repos, branches, refs, commits, labels |
| `rest2.go` | REST v3: contents, issues, pull requests |
| `graphql.go` | GraphQL parser, executor, mutations |
| `json.go` | Minimal JSON parser and encoder |
| `mcp.go` | MCP handler and tools |
| `main.ts` | Node launcher (temp-copy execute-bit fix) |
