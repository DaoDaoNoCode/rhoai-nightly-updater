// Captures the docs images from the docs mock backend. Run by
// docs/tools/screenshots.sh through `playwright-cli run-code`: the page is
// open on the mock, and GET /__docs/job names the job and the output
// directory. Full pages are written at 2x and scaled to 1440 px later; crops
// stay 2x. The docs images are the ones docs/tools/images.manifest lists
// (light theme, no callouts); the *-review and themes jobs write review
// images to THEMES_DIR, never to docs/images.
async (page) => {
  const origin = page.url().match(/^https?:\/\/[^/]+/)[0];
  const { name: job, out, contrast: jobContrast } = await (await page.request.get(`${origin}/__docs/job`)).json();
  // The docs images use the Glass contrast mode; CONTRAST=default (or
  // high-contrast) in screenshots.sh captures them in another mode.
  const CONTRAST = jobContrast || "glass";
  const W = 1440;
  const H = 900;
  // Crops use a narrower window, close to the width the docs show them at
  // (760 px), so their text is not scaled down. Full pages use W.
  const CW = 860;
  let vw = W;
  await page.clock.setFixedTime(new Date("2026-10-06T14:30:00Z"));
  // Every target is found by role, label or text, never by coordinates. When
  // a UI change removes or renames one, the job stops here with the image
  // name and the locator instead of capturing the wrong thing.
  page.setDefaultTimeout(10000);
  const notFound = (name, what, loc) => (e) => {
    throw new Error(`${name}: ${what} not found (did the UI change?): ${loc}\n${String(e.message).split("\n")[0]}`);
  };

  const post = (p) => page.request.post(origin + p);
  const scenario = (name) => post(`/__docs/scenario?name=${name}`);
  const wait = (ms) => page.waitForTimeout(ms);

  async function settle() {
    await page.waitForLoadState("networkidle").catch(() => {});
    await page.evaluate(() => document.fonts.ready);
    await page.waitForFunction(() => !document.querySelector(".pf-v6-c-skeleton"), null, { timeout: 10000 }).catch(() => {});
    await wait(500);
  }

  /**
   * Opens path in scenario sc, in the light or dark color scheme and a
   * contrast mode (docs images use CONTRAST, Glass unless the run names
   * another), with clean storage. The keys
   * are src/theme/themeCore.ts COLOR_SCHEME_KEY and CONTRAST_MODE_KEY.
   */
  async function open(path, sc, { dark = false, contrast = CONTRAST, height = H, width = vw } = {}) {
    if (sc) await scenario(sc);
    await page.setViewportSize({ width, height });
    await page.goto(origin + "/__docs/state");
    await page.evaluate(([scheme, mode]) => {
      localStorage.clear();
      sessionStorage.clear();
      localStorage.setItem("rhoai-updater.color-scheme", scheme);
      localStorage.setItem("rhoai-updater.contrast-mode", mode);
    }, [dark ? "dark" : "light", contrast]);
    await page.goto(origin + path);
    // Same pixels on every run: no animation, no blinking caret, no hover.
    await page.addStyleTag({ content: "*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}" });
    await page.mouse.move(0, 0);
    await settle();
  }

  /** Makes the viewport as tall as the page, so nothing scrolls. */
  async function fit() {
    const h = await page.evaluate(() => {
      const m = document.querySelector(".pf-v6-c-page__main");
      return innerHeight - m.clientHeight + m.scrollHeight;
    });
    await page.setViewportSize({ width: page.viewportSize().width, height: Math.max(H, Math.ceil(h)) });
    await wait(300);
  }

  /**
   * Fails the job when the app asked the mock for an /api read it does not
   * answer: the image would show an error, or every action disabled, instead
   * of the state the docs describe. GET /__docs/unhandled empties the list.
   */
  async function mockAnsweredAll(name) {
    const { requests } = await (await page.request.get(`${origin}/__docs/unhandled`)).json();
    if (requests.length) throw new Error(`${name}: the docs mock does not answer ${requests.join(", ")}; add it to docs/tools/mock/server.mjs`);
  }

  /**
   * Writes a screenshot once two in a row are the same, as Playwright's
   * toHaveScreenshot waits for a steady page: after fit() resizes the window,
   * Chromium can still be redrawing the background image (background-size:
   * cover), and a capture during that redraw differs from run to run.
   */
  async function steadyShot(name, path, clip) {
    let last = await page.screenshot({ clip });
    for (let i = 0; i < 20; i++) {
      await wait(100);
      const next = await page.screenshot({ clip });
      if (next.equals(last)) {
        const written = await page.screenshot({ path, clip });
        if (written.equals(next)) return;
        last = written;
      } else {
        last = next;
      }
    }
    throw new Error(`${name}: no two screenshots in a row were the same; something on the page keeps moving`);
  }

  async function fullPage(name) {
    await fit();
    await mockAnsweredAll(name);
    await steadyShot(name, `${out}/full/${name}.png`);
  }

  const card = (title) => page.locator(".pf-v6-c-card").filter({ has: page.getByRole("heading", { name: title, exact: true }) }).first();
  const problem = (title) => page.locator(".pf-v6-c-card").filter({ has: page.getByRole("heading", { name: title }) }).first();
  const alert = (text) => page.locator(".pf-v6-c-alert").filter({ hasText: text }).first();

  function union(rects, pad) {
    const x0 = Math.min(...rects.map((r) => r.x)) - pad;
    const y0 = Math.min(...rects.map((r) => r.y)) - pad;
    const x1 = Math.max(...rects.map((r) => r.x + r.width)) + pad;
    const y1 = Math.max(...rects.map((r) => r.y + r.height)) + pad;
    const vp = page.viewportSize();
    const x = Math.max(0, Math.floor(x0)), y = Math.max(0, Math.floor(y0));
    return { x, y, width: Math.min(vp.width, Math.ceil(x1)) - x, height: Math.min(vp.height, Math.ceil(y1)) - y };
  }

  /** Screenshot of the targets' bounding box, plus padding. */
  async function crop(name, targets, { pad = 16, noFit = false } = {}) {
    if (!noFit) await fit();
    const rects = [];
    for (const t of [].concat(targets)) {
      const b = await t.boundingBox().catch(notFound(name, "crop target", t));
      if (!b) throw new Error(`${name}: crop target not visible: ${t}`);
      rects.push(b);
    }
    const clip = union(rects, pad);
    await mockAnsweredAll(name);
    await steadyShot(name, `${out}/crop/${name}.png`, clip).catch((e) => { throw new Error(`${name}: ${JSON.stringify(clip)}: ${e.message}`); });
  }

  // --- Jobs ------------------------------------------------------------------------

  // [scenario, path, image, a card to wait for]. Also the themes job's pages.
  const PAGES = [
    ["update-available", "/", "page-status"],
    ["healthy", "/components", "page-components"],
    ["healthy", "/builds", "page-build-explorer"],
    ["dashboard-session", "/dashboard-dev", "page-dashboard-dev"],
    ["env-live", "/test-resources", "page-test-resources"],
    ["diag-prerequisite-missing", "/diagnostics", "page-diagnostics"],
    // A signed-in user who is not a cluster-admin (per-user views).
    ["user", "/", "page-user-status", "DataScienceCluster"],
    ["user", "/test-resources?project=dev-project", "page-user-environment", "Pipelines"],
  ];

  const jobs = {
    async pages() {
      for (const [sc, path, name, ready] of PAGES) {
        await open(path, sc);
        if (ready) await card(ready).waitFor().catch(notFound(name, "card", ready));
        await fullPage(name);
      }
    },

    // Not a docs image: the Models tab in its states (each catalog model,
    // the picker with a model that has no room), light and dark, to check
    // it by eye (screenshots.sh models-review, THEMES_DIR).
    // Not a docs image: the "Left over from an earlier release" problems,
    // their Fix dialogs and the success and refusal states, at 1440 px,
    // light and dark (screenshots.sh leftovers-review, THEMES_DIR).
    async "leftovers-review"() {
      const fieldsTitle = "TrustyAI default-trustyai has field values from an earlier release";
      const recreateTitle = "trustyai-service-operator-controller-manager must be recreated: its selector changed";
      const openAll = async () => {
        const toggles = page.getByRole("button", { name: /Details/ });
        for (let i = 0; i < (await toggles.count()); i++) await toggles.nth(i).click();
        const more = page.getByRole("button", { name: /^(Show content|Show more)$/ });
        for (let i = 0; i < 12 && (await more.count()) > 0; i++) await more.first().click();
        await wait(400);
      };
      // fit() only grows the window; a rescan after a fix makes the page shorter again.
      const shot = async (name) => {
        await page.setViewportSize({ width: page.viewportSize().width, height: H });
        await fit();
        await page.screenshot({ path: `${out}/themes/leftovers-${name}.png` });
      };
      const dialogShot = async (name) => { await wait(400); await page.getByRole("dialog").screenshot({ path: `${out}/themes/leftovers-${name}.png` }); };
      const fixOf = (title) => problem(title).getByRole("button", { name: "Fix", exact: true });
      for (const dark of [false, true]) {
        const theme = dark ? "dark" : "light";
        await open("/diagnostics", "diag-leftovers", { dark });
        await openAll();
        await shot(`page-${theme}`);
        await fixOf(fieldsTitle).click();
        await dialogShot(`fields-dialog-${theme}`);
        await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click();
        await fixOf(recreateTitle).click();
        await dialogShot(`recreate-dialog-${theme}`);
        await page.getByRole("dialog").getByRole("checkbox").check();
        await dialogShot(`recreate-dialog-ticked-${theme}`);
        await page.getByRole("dialog").getByRole("button", { name: "Fix", exact: true }).click();
        await settle();
        await alert("Recreated Deployment").waitFor(); await settle(); await shot(`recreated-${theme}`);
        await fixOf(fieldsTitle).click();
        // Both fixes ask the user to acknowledge what they change.
        await page.getByRole("dialog").getByRole("checkbox").check();
        await page.getByRole("dialog").getByRole("button", { name: "Fix", exact: true }).click();
        await settle();
        await alert("Changed TrustyAI default-trustyai").waitFor(); await settle(); await shot(`fields-fixed-${theme}`);
        await open("/diagnostics", "diag-leftovers-refused", { dark });
        await problem(recreateTitle).getByRole("button", { name: /Details/ }).click();
        await fixOf(recreateTitle).click();
        await page.getByRole("dialog").getByRole("checkbox").check();
        await page.getByRole("dialog").getByRole("button", { name: "Fix", exact: true }).click();
        await settle();
        await alert("Blocked: prerequisites not met").waitFor(); await settle(); await shot(`refused-${theme}`);
      }
    },

    async "models-review"() {
      const shots = [
        ["env-models-several", "qwen25-05b-instruct", "qwen25"], ["env-models-several", "qwen3-1-7b", "qwen3"],
        ["env-models-several", "granite-embedding-125m-english", "granite"], ["env-models-several", "smollm2-135m-instruct", "smollm2-not-set-up"],
        ["env-models-capacity", "qwen3-1-7b", "qwen3-no-room"], ["env-models-starting", "qwen25-05b-instruct", "starting"],
        ["env-models-image-missing", "qwen25-05b-instruct", "image-missing"], ["env-maas-published", null, "maas"],
        ["env-models-not-allowed", "qwen25-05b-instruct", "not-allowed"], ["env-models-foreign", "qwen25-05b-instruct", "foreign"],
      ];
      for (const [sc, model, name] of shots) {
        for (const dark of [false, true]) {
          const path = model ? `/test-resources?tab=models&project=dev-project&model=${model}` : "/test-resources?tab=maas";
          await open(path, sc, { dark, width: 1440 });
          await fit();
          await page.screenshot({ path: `${out}/themes/models-${name}-${dark ? "dark" : "light"}.png` });
        }
      }
      for (const dark of [false, true]) {
        await open("/test-resources?tab=models&project=dev-project", "env-models-capacity", { dark, width: 1440 });
        await page.getByRole("button", { name: "Model", exact: true }).click();
        await settle();
        await page.screenshot({ path: `${out}/themes/models-picker-${dark ? "dark" : "light"}.png` });
      }
    },

    // Not a docs image: the cluster setup of a self-managed cluster (and one
    // whose topology is unknown) in every state, light and dark, to check it
    // by eye (screenshots.sh self-managed-review, THEMES_DIR).
    async "self-managed-review"() {
      const shoot = async (name, dark, el) => {
        await fit();
        await el.screenshot({ path: `${out}/themes/self-managed-${name}-${dark ? "dark" : "light"}.png` });
      };
      for (const dark of [false, true]) {
        await open("/", "self-managed", { dark, width: 1440 });
        await shoot("1-before-step-1", dark, card("One-time cluster setup"));
        await open("/", "self-managed-saved", { dark, width: 1440 });
        await shoot("2-after-step-1", dark, card("One-time cluster setup"));
        await card("One-time cluster setup").getByRole("button", { name: "Check that the nodes can pull" }).click();
        await alert("The nodes can pull from quay.io/rhoai").waitFor();
        await wait(300);
        await shoot("3-check-passed", dark, card("One-time cluster setup"));
        await open("/", "self-managed-saved-fails", { dark, width: 1440 });
        await card("One-time cluster setup").getByRole("button", { name: "Check that the nodes can pull" }).click();
        await alert("The nodes cannot pull from quay.io/rhoai").waitFor();
        await wait(300);
        await shoot("4-check-failed", dark, card("One-time cluster setup"));
        await open("/", "self-managed-unknown", { dark, width: 1440 });
        await shoot("5-topology-unknown", dark, card("One-time cluster setup"));
        // The oc commands are under each item's "Do it yourself instead"
        // (the one-click setup, node-setup-review, comes first).
        const item = (scope, title) => scope.getByRole("listitem").filter({ has: page.getByText(title, { exact: true }) }).first();
        await open("/", "self-managed-installed", { dark, width: 1440 });
        await page.locator("#cluster-setup-toggle").click();
        await item(card("Cluster setup"), "Pull secret").getByRole("button", { name: "Do it yourself instead" }).click();
        await wait(300);
        await shoot("6-installed", dark, card("Cluster setup"));
        await open("/", "self-managed", { dark, width: 1440 });
        await item(card("One-time cluster setup"), "Image mirror (IDMS)").getByRole("button", { name: "Do it yourself instead" }).click();
        await wait(300);
        await shoot("7-mirror-instructions", dark, card("One-time cluster setup"));
      }
    },

    // Not a docs image: the one-click setup of the image mirror and the
    // nodes' pull secret on every platform, its review dialogs and a run,
    // light and dark (screenshots.sh node-setup-review, THEMES_DIR).
    async "node-setup-review"() {
      const setup = () => card("One-time cluster setup");
      const modal = () => page.locator(".pf-v6-c-modal-box");
      const shoot = async (name, dark, el) => {
        await fit();
        await el.screenshot({ path: `${out}/themes/node-setup-${name}-${dark ? "dark" : "light"}.png` });
      };
      const review = async (name, dark, button) => {
        await setup().getByRole("button", { name: button }).click();
        await page.getByRole("dialog").getByRole("button", { name: /Cancel|Close/ }).last().waitFor();
        await wait(300);
        await shoot(name, dark, modal());
      };
      for (const dark of [false, true]) {
        await open("/", "self-managed-mirror", { dark, width: 1440 });
        await shoot("01-self-managed", dark, setup());
        await setup().getByRole("button", { name: "Do it yourself instead" }).first().click();
        await setup().getByRole("button", { name: "Do it yourself instead" }).last().click();
        await wait(300);
        await shoot("02-self-managed-diy", dark, setup());
        await open("/", "self-managed-mirror", { dark, width: 1440 });
        await review("03-mirror-create-review", dark, "Create the image mirror");
        await page.getByRole("dialog").getByRole("button", { name: "Create the image mirror" }).click();
        await page.getByRole("dialog").locator(".pf-v6-c-alert__title", { hasText: "Done" }).waitFor({ timeout: 20000 });
        await wait(300);
        await shoot("04-mirror-created", dark, modal());
        await page.getByRole("dialog").getByRole("button", { name: "Close" }).last().click();
        await wait(1500);
        await shoot("05-mirror-ready", dark, setup());
        await review("06-mirror-remove-review", dark, "Remove the image mirror");
        await open("/", "self-managed-mirror", { dark, width: 1440 });
        await review("07-pull-add-review", dark, "Add to the nodes' pull secret");
        await open("/", "self-managed-installed", { dark, width: 1440 });
        await page.locator("#cluster-setup-toggle").click();
        await wait(300);
        await shoot("08-installed", dark, card("Cluster setup"));
        await card("Cluster setup").getByRole("button", { name: "Remove from the nodes' pull secret" }).click();
        await page.getByRole("dialog").getByRole("heading", { name: "What the nodes do" }).waitFor();
        await wait(300);
        await shoot("09-pull-remove-review", dark, modal());
        await open("/", "self-managed-paused-pool", { dark, width: 1440 });
        await review("10-paused-pool-review", dark, "Create the image mirror");
        await open("/", "managed-classic", { dark, width: 1440 });
        await shoot("11-managed-classic", dark, setup());
        await open("/", "unknown-denied", { dark, width: 1440 });
        await shoot("12-unknown", dark, setup());
        await review("13-unknown-refused", dark, "Try to create it here");
        await open("/", "hosted-mirror-missing", { dark, width: 1440 });
        await shoot("14-hosted", dark, setup());
        await setup().getByRole("button", { name: "How to create the image mirror" }).click();
        await page.getByRole("dialog").waitFor();
        await wait(300);
        await shoot("15-hosted-instructions", dark, modal());
      }
    },

    // Not a docs image: who set the nodes' quay.io/rhoai entry, on a
    // cluster someone else provisioned (its own entry and mirror) and on
    // one where this tool added it, with the review dialogs, light and dark
    // (screenshots.sh node-ownership-review, THEMES_DIR).
    async "node-ownership-review"() {
      const setup = () => card("One-time cluster setup");
      const modal = () => page.locator(".pf-v6-c-modal-box");
      const shoot = async (name, dark, el) => {
        await fit();
        await el.screenshot({ path: `${out}/themes/node-ownership-${name}-${dark ? "dark" : "light"}.png` });
      };
      for (const dark of [false, true]) {
        await open("/", "provisioned-by-someone-else", { dark, width: 1440 });
        await page.locator("#cluster-setup-toggle").click();
        await wait(300);
        await shoot("1-provisioned-nodes-pull", dark, card("Cluster setup"));
        await open("/", "provisioned-by-someone-else-unchecked", { dark, width: 1440 });
        await shoot("2-provisioned-unchecked", dark, setup());
        await setup().getByRole("button", { name: "Add to the nodes' pull secret" }).click();
        await page.getByRole("dialog").getByRole("checkbox").waitFor();
        await wait(300);
        await shoot("3-add-refused", dark, modal());
        await page.getByRole("dialog").getByRole("checkbox").click();
        await page.getByRole("dialog").getByRole("button", { name: "Replace the entry" }).waitFor();
        await wait(300);
        await shoot("4-replace-acknowledged", dark, modal());
        await open("/", "self-managed-installed", { dark, width: 1440 });
        await page.locator("#cluster-setup-toggle").click();
        await wait(300);
        await shoot("5-added-by-this-tool", dark, card("Cluster setup"));
      }
    },

    // Not a docs image: the MaaS tab in every state, light and dark, to
    // check it by eye (screenshots.sh maas-review, THEMES_DIR).
    async "maas-review"() {
      for (const st of ["fresh", "rhcl-143", "tier-policies", "none", "blocked", "no-cnpg", "starting", "tls", "new-tls", "ready", "publishing", "published", "earlier-build", "someone-added", "orphan", "foreign", "no-model"]) {
        for (const dark of [false, true]) {
          await open("/test-resources?tab=maas", `env-maas-${st}`, { dark, width: 1440 });
          await fit();
          await page.screenshot({ path: `${out}/themes/maas-${st}-${dark ? "dark" : "light"}.png` });
        }
      }
      for (const dark of [false, true]) {
        const theme = dark ? "dark" : "light";
        // The one-click setup: every change listed before it runs; each
        // part's own fix confirms its changes.
        for (const [scenario, button, name] of [["env-maas-fresh", "Set up MaaS", "setup-confirm"], ["env-maas-rhcl-143", "Upgrade Connectivity Link", "upgrade-confirm"],
          ["env-maas-tier-policies", "Remove the old tier policies", "tier-policies-confirm"], ["env-maas-tls", "Configure Authorino TLS", "tls-confirm"]]) {
          await open("/test-resources?tab=maas", scenario, { dark, width: 1440 });
          await page.getByRole("button", { name: button, exact: true }).click();
          await page.getByRole("dialog").waitFor();
          await page.getByRole("dialog").getByRole("button", { name: "Details" }).click();
          await page.screenshot({ path: `${out}/themes/maas-${name}-${theme}.png` });
        }
        // Do it from the CLI instead, opened.
        await open("/test-resources?tab=maas", "env-maas-fresh", { dark, width: 1440 });
        await page.getByRole("button", { name: "Do it from the CLI instead" }).first().click();
        await fit();
        await page.screenshot({ path: `${out}/themes/maas-cli-${theme}.png` });
        // Publish says, before you confirm, that the direct address answers 404.
        await open("/test-resources?tab=maas", "env-maas-ready", { dark, width: 1440 });
        await page.getByRole("button", { name: "Publish", exact: true }).click();
        await page.getByRole("dialog").waitFor();
        await page.screenshot({ path: `${out}/themes/maas-publish-confirm-${theme}.png` });
        // The Models tab of a published model: Use it points at the MaaS
        // tab; the project's unpublished model keeps its own commands.
        await open("/test-resources?tab=models&project=dev-project&model=qwen25-05b-instruct", "env-models-published", { dark, width: 1440 });
        await fit();
        await page.screenshot({ path: `${out}/themes/models-published-${theme}.png` });
        await open("/test-resources?tab=models&project=dev-project&model=qwen3-1-7b", "env-models-published", { dark, width: 1440 });
        await fit();
        await page.screenshot({ path: `${out}/themes/models-unpublished-beside-published-${theme}.png` });
      }
      await open("/test-resources", "env-maas-published");
      await fit();
      await page.screenshot({ path: `${out}/themes/maas-overview-light.png` });
    },

    // Not a docs image: Clean up MaaS refused because others use MaaS, the
    // confirmation and then the run, light and dark (screenshots.sh
    // maas-remove-review, THEMES_DIR).
    async "maas-remove-review"() {
      for (const dark of [false, true]) {
        await open("/test-resources?tab=maas", "env-maas-in-use", { dark, width: 1440 });
        await page.getByRole("button", { name: "Clean up MaaS" }).click();
        await page.screenshot({ path: `${out}/themes/maas-remove-confirm-${dark ? "dark" : "light"}.png` });
        await page.getByRole("dialog").getByRole("button", { name: "Clean up", exact: true }).click();
        await page.getByText("Others use this MaaS platform").first().waitFor();
        await page.screenshot({ path: `${out}/themes/maas-in-use-${dark ? "dark" : "light"}.png` });
      }
    },

    // Not a docs image: the Environment Overview with one project, several,
    // a problem in one of several, someone else's parts and a 40-character
    // project name, light and dark, wide and narrow (the table stacks), with
    // the list of projects open where there is one (screenshots.sh
    // overview-review, THEMES_DIR).
    async "overview-review"() {
      for (const sc of ["env-live", "env-foreign", "env-overview-three", "env-overview-mixed", "env-overview-long-name", "env-models-not-allowed"]) {
        for (const dark of [false, true]) {
          for (const width of [1440, 800]) {
            const name = `overview-${sc.replace(/^env-(overview-)?/, "")}-${dark ? "dark" : "light"}-${width}`;
            await open("/test-resources", sc, { dark, width });
            await fit();
            await page.screenshot({ path: `${out}/themes/${name}.png` });
            const show = page.getByRole("row").filter({ has: page.getByRole("link", { name: "Pipelines", exact: true }) }).getByRole("button", { name: "Show projects" });
            if (await show.count()) {
              await show.click();
              await page.getByRole("dialog", { name: /^Pipelines in \d+ projects$/ }).waitFor();
              await wait(300);
              await page.screenshot({ path: `${out}/themes/${name}-projects.png` });
            }
          }
        }
      }
    },

    // Not a docs image: the Playground tab in every state, light and dark,
    // to check it by eye (screenshots.sh playground-review, THEMES_DIR).
    async "playground-review"() {
      for (const st of ["none", "no-model", "model-not-ready", "starting", "ready", "foreign", "failed", "api-differs", "blocked"]) {
        for (const dark of [false, true]) {
          await open("/test-resources?tab=playground&project=dev-project", `env-playground-${st}`, { dark, width: 1440 });
          await fit();
          await page.screenshot({ path: `${out}/themes/playground-${st}-${dark ? "dark" : "light"}.png` });
        }
      }
      await open("/test-resources", "env-playground-ready");
      await fit();
      await page.screenshot({ path: `${out}/themes/playground-overview-light.png` });
      for (const st of ["none", "ready"]) {
        for (const dark of [false, true]) {
          await open("/test-resources?tab=playground&project=dev-project", `env-playground-${st}`, { dark, width: 400 });
          await fit();
          await page.screenshot({ path: `${out}/themes/playground-${st}-${dark ? "dark" : "light"}-400.png` });
        }
      }
    },

    // Not a docs image: the Environment page while its reads are held by the
    // mock (POST /__docs/hold), light and dark, to check the loading states
    // by eye (screenshots.sh loading-review, THEMES_DIR).
    async "loading-review"() {
      const hold = (paths) => post(`/__docs/hold?${paths.map((p) => `path=${encodeURIComponent(p)}`).join("&")}`);
      const letGo = () => post("/__docs/hold");
      /** open(), with the reads under paths held: it waits for the Skeletons instead of their end. */
      async function openHeld(path, sc, paths, dark) {
        await scenario(sc);
        await page.setViewportSize({ width: 1440, height: H });
        await page.goto(origin + "/__docs/state");
        await page.evaluate((scheme) => {
          localStorage.clear();
          sessionStorage.clear();
          localStorage.setItem("rhoai-updater.color-scheme", scheme);
          localStorage.setItem("rhoai-updater.contrast-mode", "default");
        }, dark ? "dark" : "light");
        await hold(paths);
        await page.goto(origin + path);
        await page.addStyleTag({ content: "*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}" });
        await page.mouse.move(0, 0);
        await page.locator(".pf-v6-c-skeleton").first().waitFor();
        await page.evaluate(() => document.fonts.ready);
        await wait(500);
      }
      const shot = async (name) => {
        await fit();
        await page.screenshot({ path: `${out}/themes/loading-${name}.png` });
        await letGo();
      };
      for (const dark of [false, true]) {
        const theme = dark ? "dark" : "light";
        // Nothing read yet: every row's state is a Skeleton.
        await openHeld("/test-resources", "env-overview-three", ["/api/resources/"], dark);
        await shot(`overview-${theme}`);
        // The status is in; the playground's and MaaS's own reads are not.
        await openHeld("/test-resources", "env-overview-three", ["/api/resources/playground", "/api/resources/maas"], dark);
        await page.getByRole("button", { name: "Show projects" }).first().waitFor();
        await shot(`overview-partly-${theme}`);
        await openHeld("/test-resources?tab=models&project=dev-project", "env-models-ready", ["/api/resources/"], dark);
        await shot(`models-${theme}`);
        await openHeld("/test-resources?tab=maas", "env-maas-ready", ["/api/resources/maas"], dark);
        await shot(`maas-${theme}`);
        // Another project chosen on the Playground tab: its models load.
        await open("/test-resources?tab=playground&project=dev-project", "env-overview-three", { dark, width: 1440 });
        await hold(["/api/resources/playground"]);
        await page.getByRole("button", { name: "Show projects" }).click();
        await page.getByRole("option", { name: /^llm-eval/ }).click();
        await page.locator(".pf-v6-c-skeleton").first().waitFor();
        await wait(300);
        await shot(`playground-project-${theme}`);
      }
    },

    // Not a docs image: every page in every color scheme and contrast mode,
    // to check a theme change by eye (screenshots.sh themes, THEMES_DIR).
    async themes() {
      for (const [sc, path, name, ready] of PAGES) {
        for (const dark of [false, true]) {
          for (const contrast of ["default", "high-contrast", "glass"]) {
            await open(path, sc, { dark, contrast });
            if (ready) await card(ready).waitFor();
            await fit();
            await page.screenshot({ path: `${out}/themes/${name}-${dark ? "dark" : "light"}-${contrast}.png` });
          }
        }
      }
    },

    // Status: the one-time cluster setup of a fresh cluster.
    async status() {
      await open("/", "fresh");
      await crop("status-cluster-setup", card("One-time cluster setup"));
    },

    // The Environment page: one crop per feature tab.
    async resources() {
      const part = (name) => page.getByRole("listitem", { name, exact: true });
      const panel = () => page.getByRole("tabpanel");
      // The card a part sits in: its frame reaches past the part, which
      // ends at the card's padding.
      const inCard = (loc) => loc.locator("xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' pf-v6-c-card ')][1]");

      // The S3 storage, the cluster's: its own tab, no project picker.
      await open("/test-resources?tab=storage", "env-live");
      await crop("environment-storage", [panel().getByRole("heading", { name: "S3 storage", level: 2 }), inCard(part("SeaweedFS"))]);

      await open("/test-resources?tab=pipelines", "env-live");
      await crop("environment-pipelines", [panel().getByRole("heading", { name: "Pipelines", level: 2 }), inCard(part("Pipeline server"))]);

      await open("/test-resources?tab=mlflow", "env-live");
      await crop("environment-mlflow", [panel().getByRole("heading", { name: "MLflow", level: 2 }), inCard(part("MLflow server"))]);

      // Models: the chosen model (Qwen3, one of three in the project).
      await open("/test-resources?tab=models&project=dev-project&model=qwen3-1-7b", "env-models-several");
      await crop("environment-models", [panel().getByRole("heading", { name: "Models", level: 2 }), inCard(part("Qwen3 1.7B (CPU, tool calling)"))]);

      // MaaS: the platform's parts, and this tool's model published.
      await open("/test-resources?tab=maas", "env-maas-published");
      await crop("environment-maas", [panel().getByRole("heading", { name: "MaaS", level: 2 }), inCard(part("Database")), panel().getByRole("grid", { name: "This tool's models in MaaS" })]);

      await open("/test-resources?tab=playground&project=dev-project", "env-playground-ready");
      await crop("environment-playground", [panel().getByRole("heading", { name: "Playground", level: 2 }), inCard(part("Playground"))]);
    },
  };

  if (job !== "pages" && job !== "themes" && job !== "leftovers-review") vw = CW;
  if (!jobs[job]) throw new Error(`unknown job ${job}; jobs: ${Object.keys(jobs).join(" ")}`);
  // Forget what earlier jobs' pages asked for; they were checked then.
  await page.request.get(`${origin}/__docs/unhandled`);
  await jobs[job]();
  // Review jobs write their own screenshots: check what their pages read too.
  await mockAnsweredAll(job);
  await page.setViewportSize({ width: W, height: H });
  return `${job}: done`;
}
