const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const ts = require('typescript');

const source = fs.readFileSync(path.join(__dirname, '../src/services/api.ts'), 'utf8');
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS } }).outputText;

function serviceWithResponse(status, body) {
  const exports = {};
  vm.runInNewContext(compiled, {
    exports,
    AbortSignal,
    fetch: async () => new Response(JSON.stringify(body), {
      status, headers: { 'Content-Type': 'application/json' },
    }),
  });
  return exports;
}

test('DSC repair rejects validation errors with the backend explanation', async () => {
  for (const mode of ['reset-defaults', 'remove-extra-components', 'remove-invalid']) {
    const message = 'operator version changed since the defaults preview; refresh and review defaults';
    const { repairDSC } = serviceWithResponse(422, { error: message, errorCode: 'validation' });
    await assert.rejects(repairDSC('default-dsc', mode, '3.6.0'), (err) => {
      // ApiError carries the status and code separately from the message.
      assert.equal(err.message, message);
      assert.equal(err.status, 422);
      assert.equal(err.errorCode, 'validation');
      return true;
    });
  }
});

test('DSC repair preserves successful operation results', async () => {
  const result = { success: true, message: 'DSC repaired', logs: [] };
  const { repairDSC } = serviceWithResponse(200, result);
  assert.deepEqual(await repairDSC('default-dsc', 'remove-invalid'), result);
});
