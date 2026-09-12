import { readFile, writeFile } from 'node:fs/promises';
import process from 'node:process';

import {
  browserReplHelpRegistry,
  listBrowserReplHelpEntries,
} from '../../runtime/browser-repl-help.ts';

const check = process.argv.includes('--check');
const root = new URL('../../', import.meta.url);

function replaceGeneratedRegion(source, start, end, generated, file) {
  const startIndex = source.indexOf(start);
  const endIndex = source.indexOf(end);
  if (startIndex < 0 || endIndex < 0 || endIndex < startIndex) {
    throw new Error(`${file}: missing or misordered generated-region markers`);
  }
  const contentStart = startIndex + start.length;
  return `${source.slice(0, contentStart)}\n${generated}\n${source.slice(endIndex)}`;
}

function markdownReference() {
  const titles = {
    repl: 'REPL methods',
    browser: 'Browser-control methods',
    webmcp: 'WebMCP methods',
  };
  return Object.keys(titles)
    .map((group) => {
      const entries = listBrowserReplHelpEntries().filter((entry) => entry.group === group);
      const methods = entries
        .map((entry) => `- **\`${entry.signature}\`** — ${entry.description}`)
        .join('\n');
      return `### ${titles[group]}\n\n${methods}`;
    })
    .join('\n\n');
}

function openApiMethodList() {
  const names = (group, prefix) =>
    Object.keys(browserReplHelpRegistry[group])
      .map((name) => `\`${prefix}${name}\``)
      .join(', ');
  return [
    `        - REPL: ${names('repl', 'repl.')}`,
    `        - Browser control: ${names('browser', '')}`,
    `        - WebMCP: ${names('webmcp', 'webmcp.')}`,
  ].join('\n');
}

const targets = [
  {
    file: 'docs/repl.md',
    start: '<!-- BEGIN GENERATED REPL METHOD REFERENCE -->',
    end: '<!-- END GENERATED REPL METHOD REFERENCE -->',
    generated: markdownReference(),
  },
  {
    file: 'openapi.yaml',
    start: '        <!-- BEGIN GENERATED REPL METHOD LIST -->',
    end: '        <!-- END GENERATED REPL METHOD LIST -->',
    generated: openApiMethodList(),
  },
];

let stale = false;
for (const target of targets) {
  const url = new URL(target.file, root);
  const source = await readFile(url, 'utf8');
  const updated = replaceGeneratedRegion(
    source,
    target.start,
    target.end,
    target.generated,
    target.file,
  );
  if (updated === source) continue;
  if (check) {
    console.error(`${target.file} has stale Browser REPL help; run: make repl-help-generate`);
    stale = true;
  } else {
    await writeFile(url, updated);
    console.log(`updated ${target.file}`);
  }
}

if (stale) process.exitCode = 1;
