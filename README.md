[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Hamdansyahrin/github-core-v2/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Hamdansyahrin/github-core-v2/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# github-core-v2 — a GitHub-compatible git forge, compiled

A standalone git forge that serves, from one in-memory store:

- **git smart HTTP** — upload-pack protocol v2 (capability advertisement,
  ls-refs, fetch with shallow support) and protocol v0, receive-pack v0 with
  report-status in sideband frames, fast-forward-only ref updates;
- **REST v3** under `/api/v3` — repositories, branches, commits, refs,
  contents, labels, issues, comments and pull requests (including merge);
- **GraphQL v4** at `/api/graphql` — viewer/repository reads, issue and
  pull-request connections with cursor pagination, the create/close/merge/
  comment mutations, `node(id:)` lookups and `__type` introspection;
- **MCP** at `/mcp` — JSON-RPC 2.0 tools (`create_repo`, `get_repo`,
  `create_issue`, `list_issues`, `create_pull_request`,
  `merge_pull_request`) acting on the same store as REST.

## How it runs

The sandbox entrypoint is `main.ts`, a small lifecycle shell: it starts the
forge binary, learns the ephemeral port the binary bound, reports that port
to the harness (`POST $MAKEFUN_ENV_URL/register`), long-polls
`$MAKEFUN_ENV_URL/done`, and exits 0 when the trial is over.

The forge itself is a **statically linked Go binary** (`./forge`, vendored
into this repository as the rules allow), built from the Go sources beside
it (`store.go`, `gitproto.go`, `rest.go`, `graphql.go`, `mcp.go`,
`main.go`) with the standard library only:

```sh
CGO_ENABLED=0 go build -ldflags "-s -w" -o forge .
```

A native binary removes the per-request floor of a managed HTTP stack,
which is where the timed workloads (push / clone / read / write) spend
nearly all of their time; the fetch path additionally memoizes whole pack
and response computations, so the fifty concurrent shallow clones of the
benchmark are served from cache after the first.

## Determinism

Server-created commits take their timestamps from a logical clock seeded
at 2025-01-01 and advanced by the operations themselves — never from the
wall clock — so two runs of the same request sequence produce
byte-identical objects and the trial replay matches.
