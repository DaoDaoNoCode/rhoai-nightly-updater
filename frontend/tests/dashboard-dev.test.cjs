const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const ts = require('typescript');

const source = fs.readFileSync(path.join(__dirname, '../src/services/api.ts'), 'utf8');
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS } }).outputText;

test('Dashboard Dev sends main, PR and revert requests and retains partial-failure details', async () => {
  const calls = [];
  const exports = {};
  const partial = { success: false, message: 'notebooks-ui failed; revert to resume operator', errorCode: 'partial_failure', logs: ['operator paused'] };
  vm.runInNewContext(compiled, {
    exports, AbortSignal,
    fetch: async (url, options) => {
      calls.push({ url, method: options.method, body: options.body });
      return new Response(JSON.stringify(partial), { status: 422, headers: { 'Content-Type': 'application/json' } });
    },
  });
  for (const result of [await exports.deployDashboardMain(), await exports.deployPR(123), await exports.revertDashboard()]) {
    assert.deepEqual(result, partial);
  }
  assert.deepEqual(calls.map(c => c.url), ['/api/dashboard/deploy-main', '/api/dashboard/deploy-pr', '/api/dashboard/revert']);
  assert.ok(calls.every(c => c.method === 'POST'));
  assert.deepEqual(JSON.parse(calls[1].body), { pr: 123 });
});
