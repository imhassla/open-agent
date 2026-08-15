# Publishing checklist

This branch (`public`) is a clean, squashed history prepared for open-sourcing. Before
pushing to a real remote, do the naming/module rewrite (it needs the actual repo URL).

## 1. Pick the repo and set the module path

The Go module is currently the bare name `open-agent`, which works locally but not for
`go install`. Set it to your repo path and rewrite the imports:

```sh
OWNER=your-user-or-org
REPO=open-agent            # or your chosen name

# module path in go.mod
go mod edit -module github.com/$OWNER/$REPO

# rewrite every internal import
grep -rl '"open-agent/internal' --include='*.go' . \
  | xargs sed -i '' "s#\"open-agent/internal#\"github.com/$OWNER/$REPO/internal#g"   # macOS sed
# (GNU sed: drop the '' after -i)

go build ./... && go test ./...
```

If you also rename the **binary/command** from `open-agent`, additionally replace the
literal string in: `Makefile` (`BIN`), `usage()` text in `main.go`, the sandbox image
name and inner `open-agent` invocation in `internal/sandbox/`, and the docs. This is
optional — the repo name and the command name may differ.

## 2. OpenRouter attribution (optional)

`internal/llm/client.go` sends `HTTP-Referer: https://github.com/open-agent/open-agent`.
Point it at your repo, or set `OPENROUTER_REFERER` at runtime.

## 3. Attach the remote and push

```sh
git remote add origin git@github.com:$OWNER/$REPO.git
git push -u origin public:main      # publish this clean branch as main
```

## 4. Nice-to-have before/after publishing

- GitHub Actions: `go vet ./... && go test ./...` on push/PR.
- A short `CONTRIBUTING.md` and issue templates.
- Tag a `v0.1.0` release once the module path is set (enables `go install …@v0.1.0`).

## Development workflow (dev → public)

`main` is the dev branch (full history, all features). `public` is the clean,
squashed branch you actually publish. They share an identical TREE — only history
differs — so feeding changes into public is friction-free:

- **Selective** (recommended — "feed only what's needed"): cherry-pick specific
  commits.
  ```sh
  git checkout public
  git cherry-pick <sha> [<sha> …]     # applies cleanly: trees are aligned
  git checkout main
  ```
- **Wholesale** (publish the current state as one commit):
  ```sh
  ./scripts/sync-to-public.sh "what changed"
  ```

Then push the clean branch as the remote's main:
```sh
git push origin public:main
```

## Pre-publication regression ritual

Run `make preflight` before every public sync: it executes the full test suite
and the execution-grounded bench matrix across the three main worker families
(`qwen,glm,minimax` — live OpenRouter calls, ~$0.15, a few minutes). Publish
only on a green preflight. A single family regressing means the change is
family-specific or broke routing — diagnose with the printed `replay <run-id>`
before shipping.

Keep doing everyday work on `main`; touch `public` only to publish.

## What was cleaned for publication

- Removed a 9 MB committed binary (`kimi`) and squashed history (no internal issue refs).
- Added `LICENSE` (MIT), an honest `README.md`, and `AGENTS.md` (the machine contract).
- `.gitignore` covers the build output, `.env`, and `reports/`.
- The security posture (host `bash` by default; `--sandbox` / `sandbox` for isolation) is
  documented in the README.
