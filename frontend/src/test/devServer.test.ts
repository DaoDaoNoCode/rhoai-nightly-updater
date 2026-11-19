import { createRequire } from "node:module";
import { describe, expect, it } from "vitest";

interface DevServerConfig {
  devServer: { host: string; proxy: Array<{ target: string }> };
}

const require = createRequire(import.meta.url);
const webpackConfig = require("../../webpack.config.js") as (env: object, argv: { mode: string }) => DevServerConfig;

// The dev server proxies /api to a backend that acts with the developer's own
// cluster token (DEV_MODE). Binding it to anything but loopback would hand
// that token to anyone on the network.
describe("webpack dev server", () => {
  it("listens on loopback only", () => {
    const { devServer } = webpackConfig({}, { mode: "development" });
    expect(devServer.host).toBe("127.0.0.1");
    for (const entry of devServer.proxy) {
      if (!process.env.API_TARGET) expect(entry.target).toMatch(/^http:\/\/127\.0\.0\.1:/);
    }
  });
});
