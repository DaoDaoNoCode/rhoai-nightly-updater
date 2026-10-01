const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const ts = require('typescript');

test('Build Explorer requests versions without waiting for build dates', async () => {
  const source = fs.readFileSync(path.join(__dirname, '../src/services/api.ts'), 'utf8');
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS } }).outputText;
  const exports = {};
  const paths = [];
  vm.runInNewContext(compiled, {
    exports,
    AbortSignal,
    fetch: async (url) => {
      paths.push(url);
      return new Response(JSON.stringify({ tags: [] }), { headers: { 'Content-Type': 'application/json' } });
    },
  });
  await exports.getBuildExplorerTags();
  await exports.getBuildExplorerTags(true);
  assert.deepEqual(paths, [
    '/api/build-explorer/tags?includeDates=false',
    '/api/build-explorer/tags?includeDates=true',
  ]);
});
