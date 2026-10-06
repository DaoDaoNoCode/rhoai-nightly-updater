const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const ts = require('typescript');

// Execute the actual service module, with TypeScript's type-only imports
// erased. No browser or extra test framework is needed for stream semantics.
const source = fs.readFileSync(path.join(__dirname, '../src/services/api.ts'), 'utf8');
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS } }).outputText;
const exportsObject = {};
const sandbox = { exports: exportsObject, AbortController, TextDecoder, setTimeout, clearTimeout, fetch: (...args) => global.fetch(...args) };
vm.runInNewContext(compiled, sandbox);
const { streamSSE } = exportsObject;

function event(step, status, message) {
  return 'data: ' + JSON.stringify({ step, status, message }) + '\n\n';
}

test('stream results distinguish rejection, disconnect, pending and terminal failure', async () => {
  const previous = global.fetch;
  try {
    for (const tc of [
      { name: 'forbidden (backend JSON)', status: 403, contentType: 'application/json', text: JSON.stringify({ error: 'Read-only access', errorCode: 'forbidden' }), success: false, error: /Read-only/ },
      { name: 'oauth-proxy sign-in page (403 text/html)', status: 403, contentType: 'text/html', text: '<html>Log In</html>', success: false, error: /session has expired/i },
      { name: 'busy', status: 409, text: 'Another operation is in progress', success: false, error: /409/ },
      { name: 'validation', status: 400, text: 'Invalid image', success: false, error: /400.*Invalid image/ },
      { name: 'truncated stream', text: event('delete_subscription', 'success', 'Deleted'), drop: true },
      { name: 'empty stream', text: '', drop: true },
      { name: 'pending InstallPlan', text: event('verify_installplan', 'skipped', 'Pending') + event('operation_complete', 'success', 'Installation initiated'), success: true },
      { name: 'terminal failure', text: event('operation_complete', 'failed', 'Catalog validation failed'), success: false, error: /Catalog validation failed/ },
      { name: 'session expired', contentType: 'text/html', text: 'Login', success: false, error: /session has expired/i },
    ]) {
      global.fetch = async () => new Response(tc.text, { status: tc.status || 200, headers: { 'Content-Type': tc.contentType || 'text/event-stream' } });
      const actual = await new Promise((resolve) => streamSSE('/api/test', {}, () => {}, (success, error) => resolve({ success, error }), () => resolve({ drop: true })));
      if (tc.drop) assert.equal(actual.drop, true, tc.name);
      else {
        assert.equal(actual.drop, undefined, tc.name);
        assert.equal(actual.success, tc.success, tc.name);
        if (tc.error) assert.match(actual.error, tc.error, tc.name);
      }
    }
  } finally { global.fetch = previous; }
});
