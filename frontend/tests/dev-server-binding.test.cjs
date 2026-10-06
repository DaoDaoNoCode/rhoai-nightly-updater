const { test } = require('node:test');
const assert = require('node:assert/strict');

// The dev server proxies /api to a backend that acts with the developer's own
// cluster token (DEV_MODE). Binding it to anything but loopback would hand
// that token to anyone on the network.
test('webpack dev server listens on loopback only', () => {
  const config = require('../webpack.config.js');
  const resolved = typeof config === 'function' ? config({}, { mode: 'development' }) : config;
  assert.equal(resolved.devServer.host, '127.0.0.1');
  for (const entry of resolved.devServer.proxy) {
    if (!process.env.API_TARGET) {
      assert.match(entry.target, /^http:\/\/127\.0\.0\.1:/);
    }
  }
});
