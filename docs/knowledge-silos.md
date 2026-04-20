# Knowledge Silo Detector

`scripts/knowledge-silos.mjs` reads the repository's git history and flags files (or directories) that are dominated by a single author. It is meant as a lightweight, forward-looking check: run it periodically and watch for paths where the team has concentrated knowledge in one person.

## What it measures

For every tracked path it computes, from `git log --numstat --no-merges`:

- **Total line-changes** - sum of added + deleted lines across all commits that touched the path.
- **Primary-author share** - fraction of those lines authored by the top contributor.
- **Bus factor** - how many distinct authors each contributed at least 10% of the lines. A path with bus factor 1 has no meaningful backup author.

A path is reported as a **silo** when its bus factor is 1 *and* the primary author's share meets `--threshold` (default 80%). Results are sorted by `share * lines` so high-traffic silos rise to the top.

Line counts are a proxy for knowledge, not a ground truth. They over-weight churn (reformatting, generated code) and under-weight review activity, but they are cheap and consistent across languages.

## Running it

```
node scripts/knowledge-silos.mjs
node scripts/knowledge-silos.mjs --group-by=dir --top=10
node scripts/knowledge-silos.mjs --format=markdown > docs/silos-report.md
node scripts/knowledge-silos.mjs --path='internal/**' --threshold=70
node scripts/knowledge-silos.mjs --since='1 year ago'
```

Or via Make:

```
make knowledge-silos
```

### Flags

| flag | default | purpose |
|---|---|---|
| `--threshold=<percent>` | `80` | Primary-author dominance required to flag a silo |
| `--min-lines=<n>` | `30` | Ignore paths with fewer total changed lines |
| `--path=<glob>` | (all) | Limit to paths matching glob (supports `*` and `**`) |
| `--format=text\|json\|markdown` | `text` | Output format |
| `--top=<n>` | `25` | Show top N silos |
| `--group-by=file\|dir` | `file` | Aggregate per file or per directory |
| `--since=<date>` | (all) | Pass through to `git log --since`, e.g. `"1 year ago"` |

### What it skips

Vendored and generated paths are filtered out so they don't drown the signal: `vendor/`, `node_modules/`, `dist/`, `build/`, lock files (`go.sum`, `package-lock.json`, `yarn.lock`, etc.), minified assets (`*.min.js`, `*.min.css`, `*.map`), and binary files (images, archives, PDFs). Binary entries in `git log --numstat` (marked `-`) are ignored automatically.

## Reading the output

The header summarizes the number of contributors seen in history and the path count analyzed. A bus-factor distribution shows how concentrated ownership is across the whole repo. Then the top N silos list the worst offenders.

### Single-contributor repos

If `git log` shows only one author, *every* path is technically a silo. The tool prints an explicit note in that case; treat the output as a baseline rather than an action list. Meaningful results require at least two committers.

## Caveats

- **Line-count proxy.** A 500-line generated config will dominate a small module even if nobody "owns" it. Use `--min-lines` and `--path` to focus on areas where knowledge concentration actually matters.
- **Renames.** Git only records renames when both old and new paths appear in the same commit; history before a rename that the detector can't stitch together is attributed to the new path from the rename commit forward. The parser follows git's `old => new` and `{a => b}/tail` rename notation where it appears.
- **Squash merges.** Squashed PRs attribute all lines to the merger, not the original authors. If your team uses squash merges, expect inflated ownership for whoever merges most often.
- **Email identity.** Authors are keyed by commit email. Contributors using multiple emails will appear as multiple authors and artificially inflate the bus factor.

## Suggested cadence

Run it quarterly, or whenever a new person joins and you want to see which files they should shadow first. Commit `docs/silos-report.md` if you want a dated snapshot for retrospectives; otherwise just run ad-hoc.
