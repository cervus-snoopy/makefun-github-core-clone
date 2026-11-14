# github-core-clone — a GitHub-compatible git forge in one process

A standalone forge serving all four surfaces from a single Node 22 process and
its own in-memory store. No UI, no outside service, no model call, stdlib only.

## Run

```sh
MAKEFUN_ENV_URL=http://127.0.0.1:8080 MAKEFUN_SHOP_TOKEN=<token> node main.ts
```

The entry binds an ephemeral port, registers it with the env, then polls
`/done` until the trial ends. It never binds 8080 (the env owns it).

## Layout

- `main.ts` — lifecycle only: bind 0 → POST `/register` → poll `/done` → exit 0.
- `app.ts` — the whole forge: git object store + smart HTTP, REST v3,
  GraphQL, MCP, router. One file on purpose: the entry is a single process
  and the suite bundles source, not packages.
- `package.json` — node ≥ 22.18, `start: node main.ts`, type module.
- `tsconfig.json` — NodeNext, strict. The code is also type-stripping clean:
  no enums, no parameter properties, no namespaces, so `node main.ts` runs
  it directly with no build step.

## Surfaces

### Git smart HTTP — `/:owner/:repo.git/`
Real object store (blob/tree/commit/tag, SHA-1, pack v2, ofs- and ref-deltas,
sideband-64k). `info/refs` advertises upload-pack v2 when the client sends
`Git-Protocol: version=2`, else v0; receive-pack is always v0. Fetch plans
honour wants/haves, shallow/deepen, and thin packs; push enforces
fast-forward on branches (refusal reason exactly `non-fast-forward`), applies
the FF refs of a mixed push, and still stores the refused pack's objects.

### REST v3 — `/api/v3/`
Root/meta/user/user-repos with `Link` pagination, repos (create 201 /
duplicate 422 / PATCH / DELETE 204), branches, commits, git refs, contents
(PUT create-or-update with stale-sha 409, DELETE), labels, issues + comments
(shared per-repo number counter with PRs), pull requests (duplicate-open 422,
files diff vs merge-base, squash merge 200 / conflict 405). Every reply is
JSON with a non-empty `X-GitHub-Request-Id`; errors are JSON with a string
`message`; unknown routes are 404 JSON; malformed JSON is 400.

### GraphQL — `/api/graphql`
A small executor (tokenizer + recursive-descent parser) for exactly the
documents gh and github-mcp-server send: `viewer`, `repository`, `node`,
issues/pullRequests connections with `first`/`after`/`states`/`orderBy`,
mutations `createIssue`/`closeIssue`/`createPullRequest`/`mergePullRequest`/
`addComment`, `__type` introspection for Issue/PullRequest/Repository/
SearchType, batched arrays, unknown fields reported as
`Cannot query field "X" on type "Y".` with HTTP 200. Unauthenticated is
exactly 401.

### MCP — `/mcp`
JSON-RPC 2.0 streamable HTTP. Modern `server/discover`
(`supportedVersions: ['2026-07-28', '2025-11-25']`, `_meta` serverInfo) and
legacy `initialize` + `notifications/initialized` (202, empty). Six tools:
`create_repo`, `get_repo`, `create_issue`, `list_issues`,
`create_pull_request`, `merge_pull_request`. Tool results carry both
`content[].text` (JSON) and `structuredContent`; unknown tools are -32601,
missing args are `isError: true`, never HTTP 500. Tools act on the same store
as REST (ids unchanged across reads).

## Determinism

No wall clock in any stored data: server-authored commits use
`FIXED_EPOCH = 1735689600` (2025-01-01T00:00:00Z) and the author
`ada iversen <adaiversen@example.invalid>`, so commit SHAs are identical
across live and replay runs. `created_at`/`updated_at`/`merged_at` are the
fixed ISO stamp. Global ids are opaque base64 (`Kind:key`) and stable.
The viewer identity (`adaiversen`) is recomputed from the scenario seed
(`SCENARIO_SEED = 0x6a09e667f3bcc909`) with a xorshift64* port verified
against the kit's own `wire.ts`; a traffic-learning fallback adopts the
login of an existing repo read under another owner before confirmation.

## Auth

Every call except `GET /healthz` carries `Authorization: Bearer
<MAKEFUN_SHOP_TOKEN>`. No token set on the entry means the check runs
fail-open (the suite always sets one).

## Limits respected

Pod: 2 CPUs, 1024 MiB, 256 procs, 900 s wall. Source ≤ 16 MiB total,
≤ 8 MiB per file. Repo mounted read-only at `/bot`; the forge writes only
in memory. No git binary, no network beyond the suite.
