/**
 * Knowledge Silo Detector
 *
 * Walks `git log --numstat` to estimate which files and directories are
 * dominated by a single author. Reports "silos" - paths where the top
 * contributor owns more than --threshold percent of retained line changes
 * and where the bus factor (distinct authors contributing at least 10% of
 * lines) is 1. Useful as a recurring check once a project has more than one
 * contributor.
 *
 * Usage:
 *   node scripts/knowledge-silos.mjs
 *   node scripts/knowledge-silos.mjs --threshold=70 --min-lines=50
 *   node scripts/knowledge-silos.mjs --path='internal/**' --format=markdown
 *   node scripts/knowledge-silos.mjs --group-by=dir --top=20
 *
 * Flags:
 *   --threshold=<percent>  Primary-author dominance to flag a silo (default 80)
 *   --min-lines=<n>        Ignore paths with fewer total changed lines (default 30)
 *   --path=<glob>          Only include paths matching this glob (simple * and **)
 *   --format=text|json|markdown  Output format (default text)
 *   --top=<n>              Show top N silos (default 25)
 *   --group-by=file|dir    Aggregate per file or per directory (default file)
 *   --since=<date>         Pass through to git log --since (e.g. "1 year ago")
 */

import { execSync, spawnSync } from 'child_process';
import path from 'path';

const SIGNIFICANT_SHARE = 0.10; // bus-factor threshold: author counts if they own >=10% of lines

const SKIP_PATTERNS = [
  /(^|\/)vendor\//,
  /(^|\/)node_modules\//,
  /(^|\/)dist\//,
  /(^|\/)build\//,
  /(^|\/)\.git\//,
  /(^|\/)go\.sum$/,
  /(^|\/)package-lock\.json$/,
  /(^|\/)yarn\.lock$/,
  /(^|\/)pnpm-lock\.yaml$/,
  /(^|\/)Cargo\.lock$/,
  /(^|\/)composer\.lock$/,
  /(^|\/)Gemfile\.lock$/,
  /\.min\.(js|css)$/,
  /\.map$/,
  /\.svg$/,
  /\.(png|jpg|jpeg|gif|ico|webp|pdf|zip|tar|gz|bin)$/i,
];

function parseArgs(argv) {
  const out = {
    threshold: 80,
    minLines: 30,
    pathGlob: null,
    format: 'text',
    top: 25,
    groupBy: 'file',
    since: null,
  };
  for (const arg of argv.slice(2)) {
    if (!arg.startsWith('--')) continue;
    const [rawKey, rawVal] = arg.slice(2).split('=');
    const val = rawVal ?? 'true';
    switch (rawKey) {
      case 'threshold':  out.threshold = Number(val); break;
      case 'min-lines':  out.minLines = Number(val); break;
      case 'path':       out.pathGlob = val; break;
      case 'format':     out.format = val; break;
      case 'top':        out.top = Number(val); break;
      case 'group-by':   out.groupBy = val; break;
      case 'since':      out.since = val; break;
      case 'help':
      case 'h':
        out.help = true;
        break;
      default:
        console.error(`Unknown flag: --${rawKey}`);
        process.exit(2);
    }
  }
  if (!['text', 'json', 'markdown'].includes(out.format)) {
    console.error(`--format must be one of: text, json, markdown`);
    process.exit(2);
  }
  if (!['file', 'dir'].includes(out.groupBy)) {
    console.error(`--group-by must be one of: file, dir`);
    process.exit(2);
  }
  return out;
}

function shouldSkip(file) {
  for (const re of SKIP_PATTERNS) if (re.test(file)) return true;
  return false;
}

// Minimal glob: supports * (non-slash) and ** (any).
function globToRegex(glob) {
  let re = '';
  for (let i = 0; i < glob.length; i++) {
    const c = glob[i];
    if (c === '*' && glob[i + 1] === '*') { re += '.*'; i++; }
    else if (c === '*') { re += '[^/]*'; }
    else if (c === '?') { re += '[^/]'; }
    else if ('.+^$()|{}[]\\'.includes(c)) { re += '\\' + c; }
    else { re += c; }
  }
  return new RegExp('^' + re + '$');
}

function readGitLog(since) {
  const args = ['log', '--no-merges', '--numstat', '--format=__COMMIT__%n%ae'];
  if (since) args.push(`--since=${since}`);
  const res = spawnSync('git', args, { encoding: 'utf8', maxBuffer: 1024 * 1024 * 200 });
  if (res.status !== 0) {
    console.error('git log failed:', res.stderr);
    process.exit(1);
  }
  return res.stdout;
}

function parseLog(raw) {
  const entries = []; // { file, author, added, deleted }
  const lines = raw.split('\n');
  let author = null;
  let expectAuthor = false;
  for (const line of lines) {
    if (line === '__COMMIT__') { expectAuthor = true; continue; }
    if (expectAuthor) { author = line.trim(); expectAuthor = false; continue; }
    if (!line.trim()) continue;
    const parts = line.split('\t');
    if (parts.length < 3) continue;
    const [addedStr, deletedStr, fileField] = parts;
    // Binary files report '-' - skip them
    if (addedStr === '-' || deletedStr === '-') continue;
    const added = Number(addedStr);
    const deleted = Number(deletedStr);
    if (!Number.isFinite(added) || !Number.isFinite(deleted)) continue;
    // Handle renames "old => new" or "{a => b}/tail"
    let file = fileField;
    const braceRename = file.match(/^(.*)\{(.*?) => (.*?)\}(.*)$/);
    if (braceRename) {
      file = braceRename[1] + braceRename[3] + braceRename[4];
      file = file.replace(/\/\//g, '/');
    } else if (file.includes(' => ')) {
      file = file.split(' => ')[1];
    }
    if (shouldSkip(file)) continue;
    entries.push({ file, author, added, deleted });
  }
  return entries;
}

function aggregate(entries, groupBy) {
  const byKey = new Map(); // key -> { authors: Map<author, lines>, total }
  for (const e of entries) {
    const key = groupBy === 'dir' ? (path.dirname(e.file) || '.') : e.file;
    let rec = byKey.get(key);
    if (!rec) { rec = { authors: new Map(), total: 0 }; byKey.set(key, rec); }
    const lines = e.added + e.deleted;
    rec.authors.set(e.author, (rec.authors.get(e.author) || 0) + lines);
    rec.total += lines;
  }
  return byKey;
}

function scorePath(rec) {
  const entries = [...rec.authors.entries()].sort((a, b) => b[1] - a[1]);
  const topAuthor = entries[0][0];
  const topLines = entries[0][1];
  const share = rec.total ? topLines / rec.total : 0;
  let busFactor = 0;
  for (const [, lines] of entries) {
    if (lines / rec.total >= SIGNIFICANT_SHARE) busFactor++;
  }
  if (busFactor === 0) busFactor = 1; // top author always counts
  return { topAuthor, topShare: share, busFactor, totalLines: rec.total, authors: entries };
}

function computeSilos(entries, opts) {
  const byKey = aggregate(entries, opts.groupBy);
  const pathMatcher = opts.pathGlob ? globToRegex(opts.pathGlob) : null;

  const scored = [];
  for (const [key, rec] of byKey) {
    if (pathMatcher && !pathMatcher.test(key)) continue;
    if (rec.total < opts.minLines) continue;
    const s = scorePath(rec);
    scored.push({ key, ...s });
  }

  const thresholdFrac = opts.threshold / 100;
  const silos = scored
    .filter(s => s.busFactor === 1 && s.topShare >= thresholdFrac)
    .sort((a, b) => (b.topShare * b.totalLines) - (a.topShare * a.totalLines));

  return { scored, silos };
}

function busFactorDistribution(scored) {
  const dist = new Map();
  for (const s of scored) dist.set(s.busFactor, (dist.get(s.busFactor) || 0) + 1);
  return [...dist.entries()].sort((a, b) => a[0] - b[0]);
}

function uniqueAuthors(entries) {
  const s = new Set();
  for (const e of entries) if (e.author) s.add(e.author);
  return s;
}

function renderText(report, opts) {
  const { meta, silos, distribution, scored } = report;
  const lines = [];
  lines.push(`Knowledge Silo Report  (threshold=${opts.threshold}%, min-lines=${opts.minLines}, group-by=${opts.groupBy})`);
  lines.push(`Repo contributors: ${meta.authorCount}  |  paths analyzed: ${scored.length}  |  total line-changes: ${meta.totalLines}`);
  if (meta.authorCount <= 1) {
    lines.push('');
    lines.push('Note: this repository currently has one contributor, so every path is technically a silo.');
    lines.push('      Re-run this tool once more contributors land to get a useful signal.');
  }
  lines.push('');
  lines.push('Bus-factor distribution (paths by number of authors owning >=10% each):');
  for (const [bf, n] of distribution) lines.push(`  bus_factor=${bf}: ${n} paths`);
  lines.push('');
  if (!silos.length) {
    lines.push('No silos above threshold. ');
    return lines.join('\n');
  }
  lines.push(`Top ${Math.min(opts.top, silos.length)} silos (sorted by owned lines):`);
  const width = Math.min(opts.top, silos.length);
  for (let i = 0; i < width; i++) {
    const s = silos[i];
    const pct = (s.topShare * 100).toFixed(1);
    lines.push(`  ${pct.padStart(5)}%  ${String(s.totalLines).padStart(6)} lines  ${s.topAuthor}  ${s.key}`);
  }
  return lines.join('\n');
}

function renderMarkdown(report, opts) {
  const { meta, silos, distribution, scored } = report;
  const out = [];
  out.push(`# Knowledge Silo Report`);
  out.push('');
  out.push(`- threshold: **${opts.threshold}%**`);
  out.push(`- min-lines: **${opts.minLines}**`);
  out.push(`- group-by: **${opts.groupBy}**`);
  out.push(`- contributors: **${meta.authorCount}**`);
  out.push(`- paths analyzed: **${scored.length}**`);
  out.push(`- total line-changes: **${meta.totalLines}**`);
  if (opts.since) out.push(`- since: **${opts.since}**`);
  out.push('');
  if (meta.authorCount <= 1) {
    out.push('> **Note:** only one contributor is visible in history, so every path is technically a silo. Re-run once more contributors land.');
    out.push('');
  }
  out.push('## Bus-factor distribution');
  out.push('');
  out.push('| bus_factor | paths |');
  out.push('|---:|---:|');
  for (const [bf, n] of distribution) out.push(`| ${bf} | ${n} |`);
  out.push('');
  if (!silos.length) {
    out.push('No silos above threshold.');
    return out.join('\n');
  }
  out.push(`## Top silos`);
  out.push('');
  out.push('| share | lines | primary author | path |');
  out.push('|---:|---:|---|---|');
  for (const s of silos.slice(0, opts.top)) {
    const pct = (s.topShare * 100).toFixed(1);
    out.push(`| ${pct}% | ${s.totalLines} | ${s.topAuthor} | \`${s.key}\` |`);
  }
  return out.join('\n');
}

function renderJson(report, opts) {
  return JSON.stringify({
    opts,
    meta: report.meta,
    distribution: Object.fromEntries(report.distribution),
    silos: report.silos.slice(0, opts.top),
  }, null, 2);
}

function main() {
  const opts = parseArgs(process.argv);
  if (opts.help) {
    console.log(`Usage: node scripts/knowledge-silos.mjs [flags]

Flags:
  --threshold=<percent>  Primary-author dominance to flag a silo (default 80)
  --min-lines=<n>        Ignore paths with fewer total changed lines (default 30)
  --path=<glob>          Only include paths matching glob (supports * and **)
  --format=text|json|markdown  (default text)
  --top=<n>              Show top N silos (default 25)
  --group-by=file|dir    (default file)
  --since=<date>         Pass through to git log --since (e.g. "1 year ago")
`);
    return;
  }

  // Must run inside a git worktree
  try { execSync('git rev-parse --is-inside-work-tree', { stdio: 'pipe' }); }
  catch { console.error('Not inside a git repository.'); process.exit(1); }

  const raw = readGitLog(opts.since);
  const entries = parseLog(raw);
  const { scored, silos } = computeSilos(entries, opts);
  const distribution = busFactorDistribution(scored);
  const meta = {
    authorCount: uniqueAuthors(entries).size,
    totalLines: entries.reduce((n, e) => n + e.added + e.deleted, 0),
  };
  const report = { meta, silos, distribution, scored };

  let out;
  switch (opts.format) {
    case 'json':     out = renderJson(report, opts); break;
    case 'markdown': out = renderMarkdown(report, opts); break;
    default:         out = renderText(report, opts);
  }
  console.log(out);
}

main();
