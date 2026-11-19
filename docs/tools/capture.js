// Captures the docs images from the docs mock backend. Run by
// docs/tools/screenshots.sh through `playwright-cli run-code`: the page is
// open on the mock, and GET /__docs/job names the job and the output
// directory. Full pages are written at 2x and scaled to 1440 px later; crops
// stay 2x; GIF frames go to frames/<gif>/NNN-<ms>.png.
async (page) => {
  const origin = page.url().match(/^https?:\/\/[^/]+/)[0];
  const { name: job, out, contrast: jobContrast } = await (await page.request.get(`${origin}/__docs/job`)).json();
  // The docs images use the Glass contrast mode; CONTRAST=default (or
  // high-contrast) in screenshots.sh captures them in another mode.
  const CONTRAST = jobContrast || "glass";
  const W = 1440;
  const H = 900;
  // Crops and GIFs use a narrower window, close to the width the docs show
  // them at (760 px), so their text is not scaled down. Full pages use W.
  const CW = 860;
  let vw = W;
  const RED = "#EE0000";
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
  const advance = (n = 1) => post(`/__docs/advance?n=${n}`);
  const release = () => post("/__docs/release");
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

  async function fullPage(name) {
    await fit();
    await page.screenshot({ path: `${out}/full/${name}.png` });
  }

  const card = (title) => page.locator(".pf-v6-c-card").filter({ has: page.getByRole("heading", { name: title, exact: true }) }).first();
  const problem = (title) => page.locator(".pf-v6-c-card").filter({ has: page.getByRole("heading", { name: title }) }).first();
  const alert = (text) => page.locator(".pf-v6-c-alert").filter({ hasText: text }).first();
  /** A description list row (term and description) inside scope. */
  const row = (scope, term) => scope.locator(".pf-v6-c-description-list__group").filter({ has: page.getByText(term, { exact: true }) }).first();
  const label = (scope, text) => scope.locator(".pf-v6-c-label").filter({ hasText: text }).first();

  /**
   * Draws numbered callouts and highlight boxes over the page. notes:
   * [{ at: locator, n?: number, box?: false, pos?: "tl"|"tr"|"l"|"r"|"b", pad?: number }].
   * "b" sits just under the middle of the bottom edge, for a target with text on both sides.
   * pad is how far the box reaches past the target (5 px); a negative pad keeps the boxes of
   * adjacent targets (two options of one list) apart.
   * Returns the rectangles drawn, so a crop can include them.
   */
  async function annotate(notes, name = "") {
    const items = [];
    for (const nt of notes) {
      const b = await nt.at.boundingBox().catch(notFound(name, `callout ${nt.n}`, nt.at));
      if (!b) throw new Error(`${name}: callout ${nt.n} target not visible: ${nt.at}`);
      items.push({ b, n: nt.n, box: nt.box !== false, pos: nt.pos || "tl", pad: nt.pad ?? 5 });
    }
    return page.evaluate(({ items, RED }) => {
      const layer = document.createElement("div");
      layer.id = "docs-annotations";
      layer.style.cssText = "position:fixed;inset:0;pointer-events:none;z-index:2147483647";
      const rects = [];
      const R = 15;
      for (const { b, n, box, pos, pad } of items) {
        const x = b.x - pad, y = b.y - pad, w = b.width + 2 * pad, h = b.height + 2 * pad;
        if (box) {
          const r = document.createElement("div");
          r.style.cssText = `position:absolute;left:${x}px;top:${y}px;width:${w}px;height:${h}px;border:3px solid ${RED};border-radius:8px;box-sizing:border-box;box-shadow:0 0 0 2px rgba(255,255,255,.7)`;
          layer.append(r);
          rects.push({ x, y, width: w, height: h });
        }
        if (n) {
          const at = {
            tl: [x, y], tr: [x + w, y],
            l: [x - R - 6, y + h / 2], r: [x + w + R + 6, y + h / 2],
            b: [x + w / 2, y + h + R - 2],
          }[pos];
          const c = document.createElement("div");
          c.textContent = String(n);
          c.style.cssText = `position:absolute;left:${at[0] - R}px;top:${at[1] - R}px;width:${2 * R}px;height:${2 * R}px;border-radius:50%;background:${RED};color:#fff;font:700 16px/30px RedHatText,"Red Hat Text",Helvetica,Arial,sans-serif;text-align:center;border:2px solid #fff;box-sizing:border-box;box-shadow:0 1px 4px rgba(0,0,0,.45)`;
          layer.append(c);
          rects.push({ x: at[0] - R, y: at[1] - R, width: 2 * R, height: 2 * R });
        }
      }
      document.body.append(layer);
      return rects;
    }, { items, RED });
  }
  const clearNotes = () => page.evaluate(() => document.getElementById("docs-annotations")?.remove());

  function union(rects, pad) {
    const x0 = Math.min(...rects.map((r) => r.x)) - pad;
    const y0 = Math.min(...rects.map((r) => r.y)) - pad;
    const x1 = Math.max(...rects.map((r) => r.x + r.width)) + pad;
    const y1 = Math.max(...rects.map((r) => r.y + r.height)) + pad;
    const vp = page.viewportSize();
    const x = Math.max(0, Math.floor(x0)), y = Math.max(0, Math.floor(y0));
    return { x, y, width: Math.min(vp.width, Math.ceil(x1)) - x, height: Math.min(vp.height, Math.ceil(y1)) - y };
  }

  /** Screenshot of the targets' bounding box (plus padding), with callouts. */
  async function crop(name, targets, { notes = [], pad = 16, noFit = false } = {}) {
    if (!noFit) await fit();
    const rects = [];
    for (const t of [].concat(targets)) {
      const b = await t.boundingBox().catch(notFound(name, "crop target", t));
      if (!b) throw new Error(`${name}: crop target not visible: ${t}`);
      rects.push(b);
    }
    rects.push(...(await annotate(notes, name)));
    const clip = union(rects, pad);
    await page.screenshot({ path: `${out}/crop/${name}.png`, clip }).catch((e) => { throw new Error(`${name}: ${JSON.stringify(clip)}: ${e.message}`); });
    await clearNotes();
  }

  // --- GIF frames -----------------------------------------------------------------

  let gifName = "", gifClip = null, frameNo = 0;
  function gif(name, clip) { gifName = name; gifClip = clip; frameNo = 0; }
  async function frame(ms) {
    frameNo += 1;
    await page.screenshot({ path: `${out}/frames/${gifName}/${String(frameNo).padStart(3, "0")}-${ms}.png`, clip: gifClip });
  }
  /** The page below the masthead, at most maxHeight tall. */
  async function mainClip(maxHeight = 2000) {
    const top = await page.evaluate(() => Math.ceil(document.querySelector(".pf-v6-c-masthead").getBoundingClientRect().bottom));
    return { x: 0, y: top, width: page.viewportSize().width, height: Math.min(page.viewportSize().height - top, maxHeight) };
  }

  /** A pointer drawn into the page, so GIFs show where the click goes. */
  async function pointer(loc, { dx = 0.5, dy = 0.5 } = {}) {
    const b = await loc.boundingBox();
    await page.evaluate(({ x, y }) => {
      let p = document.getElementById("docs-pointer");
      if (!p) {
        p = document.createElement("div");
        p.id = "docs-pointer";
        p.style.cssText = "position:fixed;z-index:2147483647;pointer-events:none;width:26px;height:26px;filter:drop-shadow(0 1px 2px rgba(0,0,0,.5))";
        p.innerHTML = '<svg viewBox="0 0 24 24" width="26" height="26"><path d="M4 2 L4 20 L9 15.5 L12.5 22 L15.5 20.6 L12.2 14.2 L19 14 Z" fill="#151515" stroke="#fff" stroke-width="1.6" stroke-linejoin="round"/></svg>';
        document.body.append(p);
      }
      p.style.left = `${x - 5}px`;
      p.style.top = `${y - 2}px`;
    }, { x: b.x + b.width * dx, y: b.y + b.height * dy });
  }
  const hidePointer = () => page.evaluate(() => document.getElementById("docs-pointer")?.remove());
  /** Points at loc, shows it, clicks it. */
  async function clickShown(loc, ms = 700) {
    await loc.scrollIntoViewIfNeeded();
    await loc.hover();
    await wait(450);
    await pointer(loc);
    await frame(ms);
    await loc.click();
  }
  /** Scrolls the page's main area so loc's top sits near the top of the clip. */
  async function scrollTo(loc, offset = 20) {
    const b = await loc.boundingBox();
    await page.evaluate(({ dy }) => { document.querySelector(".pf-v6-c-page__main").scrollBy(0, dy); }, { dy: b.y - gifClip.y - offset });
    await wait(250);
  }

  /** An update or first install: click, confirm, the steps two at a time, the result. */
  async function operatorFlow(name, sc, button, confirmName) {
    await open("/", sc);
    gif(name, await mainClip());
    await frame(1000);
    await clickShown(page.getByRole("button", { name: button }), 600);
    await wait(700);
    await hidePointer();
    await frame(1400);
    await clickShown(page.locator(".pf-v6-c-modal-box").getByRole("button", { name: confirmName }).first(), 600);
    await hidePointer();
    await wait(600);
    await scrollTo(page.getByRole("region", { name: "Operation progress" }), 12);
    // The page clock moves 25 s per frame, so the elapsed time counts up.
    const t0 = Date.parse("2026-10-06T14:30:00Z");
    for (let i = 0; i < 21; i += 2) {
      await page.clock.setFixedTime(new Date(t0 + (i + 2) * 12500));
      await advance(i === 20 ? 1 : 2);
      // The elapsed time re-renders once a second: wait for that tick.
      await wait(1100);
      await frame(i === 20 ? 1500 : 280);
    }
    await wait(1200);
    await settle();
    await page.evaluate(() => document.querySelector(".pf-v6-c-page__main").scrollTo(0, 0));
    await wait(300);
    await frame(2000);
  }

  // --- Jobs ------------------------------------------------------------------------

  const PAGES = [
    ["update-available", "/", "page-status"],
    ["healthy", "/components", "page-components"],
    ["healthy", "/builds", "page-build-explorer"],
    ["dashboard-session", "/dashboard-dev", "page-dashboard-dev"],
    ["env-live", "/test-resources", "page-test-resources"],
    ["diag-prerequisite-missing", "/diagnostics", "page-diagnostics"],
  ];

  const jobs = {
    async pages() {
      for (const dark of [false, true]) {
        for (const [sc, path, name] of PAGES) {
          await open(path, sc, { dark });
          await fullPage(`${name}-${dark ? "dark" : "light"}`);
        }
      }
    },

    // Not a docs image: the Models tab in its states (each catalog model,
    // the picker with a model that has no room), light and dark, to check
    // it by eye (screenshots.sh models-review, THEMES_DIR).
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
        await open("/", "self-managed-installed", { dark, width: 1440 });
        await page.locator("#cluster-setup-toggle").click();
        await page.getByRole("button", { name: "The command that adds the quay.io/rhoai entry" }).click();
        await wait(300);
        await shoot("6-installed", dark, card("Cluster setup"));
        await open("/", "self-managed", { dark, width: 1440 });
        await page.getByRole("button", { name: "How to create the image mirror" }).click();
        await page.getByRole("dialog").waitFor();
        await wait(300);
        await shoot("7-mirror-instructions", dark, page.locator(".pf-v6-c-modal-box"));
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
        await page.getByRole("button", { name: "Clean up MaaS..." }).click();
        await page.screenshot({ path: `${out}/themes/maas-remove-confirm-${dark ? "dark" : "light"}.png` });
        await page.getByRole("dialog").getByRole("button", { name: "Remove", exact: true }).click();
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
      for (const [sc, path, name] of PAGES) {
        for (const dark of [false, true]) {
          for (const contrast of ["default", "high-contrast", "glass"]) {
            await open(path, sc, { dark, contrast });
            await fit();
            await page.screenshot({ path: `${out}/themes/${name}-${dark ? "dark" : "light"}-${contrast}.png` });
          }
        }
      }
    },

    async status() {
      await open("/", "update-available");
      const rhoai = card("RHOAI on this cluster");
      await crop("status-installed-vs-latest", rhoai, {
        notes: [
          { at: row(rhoai, "Installed"), n: 1, pos: "l" },
          { at: row(rhoai, "Latest"), n: 2, pos: "l" },
          { at: label(rhoai, "Update available"), n: 3, pos: "l" },
          { at: rhoai.getByRole("button", { name: "Update to latest" }), n: 4, pos: "tl" },
        ],
        pad: 24,
      });

      await open("/", "operator-failed");
      await crop("status-operator-failed", card("RHOAI on this cluster"), {
        notes: [
          { at: label(card("RHOAI on this cluster"), "Operator failed"), n: 1, pos: "l" },
          { at: alert("The operator is Failed"), n: 2, pos: "l" },
          { at: card("RHOAI on this cluster").getByRole("button", { name: "Update to latest" }), n: 3, pos: "tl" },
        ],
        pad: 24,
      });

      await open("/", "no-subscription");
      await crop("status-no-subscription", alert("The operator has no Subscription"));

      await open("/", "fresh");
      const setup = card("One-time cluster setup");
      await crop("status-cluster-setup", setup, {
        notes: [
          { at: setup.getByRole("button", { name: "Create secret" }), n: 1, pos: "r" },
          { at: setup.getByRole("button", { name: "How to create the image mirror" }), n: 2, pos: "r" },
        ],
        pad: 24,
      });

      // Self-managed OpenShift after step 1: the nodes' command and check.
      await open("/", "self-managed-saved");
      const selfManaged = card("One-time cluster setup");
      await selfManaged.getByRole("button", { name: "Check that the nodes can pull" }).click();
      await selfManaged.locator(".pf-v6-c-alert__title", { hasText: "The nodes can pull from quay.io/rhoai" }).waitFor();
      await crop("status-cluster-setup-self-managed", selfManaged, {
        notes: [
          { at: selfManaged.locator("#node-pull-secret-command-code"), n: 1, pos: "l" },
          { at: selfManaged.getByRole("button", { name: "Check that the nodes can pull" }), n: 2, pos: "r" },
          { at: selfManaged.locator(".pf-v6-c-alert__title", { hasText: "The nodes can pull from quay.io/rhoai" }), n: 3, pos: "r" },
        ],
        pad: 24,
      });

      await open("/", "no-dsc");
      await crop("status-create-dsc", card("Create the DataScienceCluster"), {
        notes: [{ at: page.getByRole("button", { name: /^Create DataScienceCluster/ }), n: 1, pos: "r" }],
      });

      await open("/", "healthy");
      await crop("status-recovery", card("Recovery"));
    },

    async banners() {
      await open("/components", "remote-operation");
      const remote = alert("on updater pod");
      await crop("banner-remote-operation", remote);

      await open("/components", "interrupted");
      await alert("was interrupted").locator("button").first().click();
      await wait(400);
      await crop("banner-interrupted", alert("was interrupted"));

      await open("/components", "major-update");
      await crop("banner-major-update", alert("is available"));
      const head = page.locator(".pf-v6-c-masthead .pf-v6-c-toolbar__group.pf-m-align-end");

      await open("/components", "patch-update");
      await crop("banner-patch-update", alert("is available"));

      // The desktop masthead: below PatternFly's lg breakpoint the
      // appearance button moves into a menu, and the crop window (CW) is
      // narrower. A crop's text size does not depend on the window width.
      await open("/components", "healthy", { width: W });
      await crop("masthead-version", head, { notes: [{ at: page.getByText("v4.0.0", { exact: true }), n: 1, pos: "l" }] });
      // Tall enough for the whole Help dialog, so its body does not scroll.
      await page.setViewportSize({ width: vw, height: 2600 });
      await page.getByRole("button", { name: /help/i }).first().click();
      await wait(800);
      const about = page.getByRole("dialog").locator('[aria-label="About this installation"]');
      await crop("help-about", [page.getByRole("dialog").getByRole("heading", { name: "About this installation" }), page.locator(".pf-v6-c-modal-box__footer")], {
        notes: [{ at: about.locator(".pf-v6-c-description-list__group").filter({ hasText: "Updater build" }), n: 1, pos: "tl" }],
        noFit: true, pad: 12,
      });
      await page.keyboard.press("Escape");

      await open("/components", "template-outdated");
      await alert("deployment is out of date").locator("button").first().click();
      await wait(400);
      await crop("banner-template-outdated", alert("deployment is out of date"));
    },

    async components() {
      await open("/components", "components-not-ready");
      const dsc = page.locator(".pf-v6-c-card").first();
      await crop("components-cause", dsc, {
        notes: [
          { at: label(dsc, "Not Ready"), n: 1, pos: "r" },
          { at: dsc.getByText(/^Cause: Prerequisite operator/).locator("xpath=..").first(), n: 2, pos: "l" },
        ],
        pad: 24,
      });

      // The comparison with the release default when nothing needs fixing:
      // the summary and the first rows, with their labels.
      await open("/components", "healthy");
      const okTree = card("Components and sub-components");
      await okTree.getByText("Nothing to fix compared with the release default").waitFor();
      await settle();
      // The card's header down to the end of a row (no pad: the next row stays out).
      await crop("components-summary-ok", [okTree.locator(".pf-v6-c-card__header").first(), okTree.getByRole("row").filter({ has: page.locator("code", { hasText: /^modelsAsAService$/ }) })], { pad: 0 });

      // The differences: the summary with Fix, Show only differences on,
      // and each row with its labels. Then a toggle's confirmation.
      await open("/components", "components-differences");
      const tree = card("Components and sub-components");
      await tree.getByRole("switch", { name: "Show only differences" }).check({ force: true });
      await settle();
      await crop("components-differences", tree, {
        notes: [
          { at: tree.locator(".pf-v6-c-alert").first(), n: 1, box: false, pos: "l" },
          { at: tree.getByRole("button", { name: "Fix", exact: true }), n: 2, pos: "l" },
          { at: tree.getByRole("switch", { name: "Show only differences" }).locator("xpath=ancestor::label[1]"), n: 3, pos: "b" },
          { at: tree.getByRole("button", { name: "Deploys nothing — parent Removed" }), n: 4, pos: "r" },
        ],
        pad: 24,
      });
      await tree.getByRole("switch", { name: "Show only differences" }).uncheck({ force: true });
      await settle();
      await tree.getByRole("combobox", { name: "Set ray" }).selectOption("Removed");
      await page.getByRole("dialog").getByText(/removes it/).waitFor();
      await settle();
      await crop("components-toggle", page.locator(".pf-v6-c-modal-box"), { noFit: true, pad: 6 });
      await page.keyboard.press("Escape");

      // MaaS set up and a model published: the resets keep the components
      // the user turned on (checked by default); unchecked, and turning
      // aigateway off, the MaaS guard refuses. Tall enough that no dialog
      // body scrolls.
      await open("/components", "components-turned-on", { height: 1800 });
      const modal = page.locator(".pf-v6-c-modal-box");
      const dialog = page.getByRole("dialog");
      const keep = dialog.getByRole("checkbox", { name: /^Keep the components you turned on/ });
      const turnedOn = card("Components and sub-components");
      // No focus ring on the dialog's close button or the checkbox.
      const unfocus = () => page.evaluate(() => document.activeElement?.blur());
      await turnedOn.getByRole("button", { name: "Reset to the release default…" }).click();
      await dialog.getByText(/^Kept:/).waitFor();
      await settle();
      await unfocus();
      await crop("components-soft-reset", modal, { noFit: true, pad: 6 });
      await keep.uncheck();
      await dialog.getByText("This change is not possible now").waitFor();
      await settle();
      await unfocus();
      await crop("components-soft-reset-maas-in-use", modal, { noFit: true, pad: 6 });
      await keep.check();
      await dialog.getByLabel("Hard reset").check();
      await dialog.getByText(/Every component is torn down/).waitFor();
      await dialog.getByText(/^Kept:/).waitFor();
      await dialog.getByRole("textbox").fill("default-dsc");
      await settle();
      await unfocus();
      await crop("components-hard-reset", modal, { noFit: true, pad: 6 });
      await page.keyboard.press("Escape");
      await wait(400);
      await turnedOn.getByRole("combobox", { name: "Set aigateway", exact: true }).selectOption("Removed");
      await dialog.getByText("This change is not possible now").waitFor();
      await settle();
      await unfocus();
      await crop("components-toggle-maas-in-use", modal, { noFit: true, pad: 6 });
      await page.keyboard.press("Escape");
    },

    // Signing in: the page once the session expired (any API answers 401
    // session_expired), and a sign-in from before 4.0.0, whose changes are
    // off with the reason (GET /api/user/permissions, reasonCode sign_in_again).
    async signin() {
      await open("/", "session-expired");
      await crop("signin-session-expired", page.locator(".pf-v6-c-empty-state"), { pad: 0 });
      // The page from the masthead down: the banner with Log out, the user
      // menu it points to, and a disabled change with the same reason.
      await open("/", "sign-in-before-4");
      const rhoai = card("RHOAI on this cluster");
      const banner = alert("Sign in again to make changes");
      await banner.waitFor();
      await rhoai.getByRole("button", { name: "Update to latest" }).hover({ force: true });
      const tip = page.getByRole("tooltip").filter({ hasText: "Your sign-in cannot make changes" });
      await tip.waitFor();
      await wait(300);
      await crop("signin-before-4", [page.locator(".pf-v6-c-masthead"), rhoai, tip], {
        notes: [
          { at: banner.getByRole("link", { name: "Log out" }), n: 1, pos: "r" },
          { at: rhoai.getByRole("button", { name: "Update to latest" }), n: 2, pos: "tl" },
        ],
        noFit: true, pad: 0,
      });
    },

    async builds() {
      await open("/builds", "update-available");
      const search = page.getByPlaceholder("Image, commit SHA, PR # or PR URL");
      await search.fill("#5123");
      await search.press("Enter");
      await wait(1500);
      await settle();
      await crop("builds-pr-search", card("Nightly builds"));
    },

    async dashdev() {
      await open("/dashboard-dev", "dashboard-session");
      const s = page.locator(".pf-v6-c-card").first();
      await crop("dashboard-dev-session", s, {
        notes: [
          { at: row(s, "Started"), n: 1, pos: "l" },
          { at: s.getByRole("button", { name: "Revert to default" }), n: 2, pos: "l" },
        ],
        pad: 24,
      });
      // The containers card: gen-ai-ui runs a commit two pushes behind the
      // PR, and its odh-pr tag has moved on to a newer build.
      const containers = card("Dashboard containers (4)");
      const genAI = containers.getByRole("row").filter({ hasText: "Gen AI" }).first();
      await crop("dashboard-dev-builds", [alert("has a newer build"), containers], {
        notes: [
          { at: label(genAI, "newer commits"), n: 3, pos: "l" },
          { at: genAI.getByText("Newer build available"), n: 4, pos: "l" },
        ],
        pad: 24,
      });
    },

    // The Environment page: the Overview, then one tab per feature.
    async resources() {
      const part = (name) => page.getByRole("listitem", { name, exact: true });
      const panel = () => page.getByRole("tabpanel");

      await open("/test-resources", "env-live");
      const table = page.getByRole("grid", { name: "Features" });
      await crop("environment-overview", [page.getByRole("heading", { name: "Overview", level: 2 }), table], {
        notes: [
          { at: page.getByText(/^\d+ of \d+ ready$/), n: 1, pos: "r" },
          { at: table.getByRole("link", { name: "Pipelines", exact: true }), n: 2, pos: "l" },
          { at: table.getByRole("row").filter({ has: page.getByRole("link", { name: "MLflow", exact: true }) }).locator('td[data-label="State"]'), n: 3, pos: "tl" },
          { at: table.getByRole("link", { name: /Open MLflow/ }), n: 4, pos: "r" },
        ],
        pad: 24,
      });

      await open("/test-resources?tab=pipelines", "env-live");
      const storage = part("S3 storage");
      await crop("environment-pipelines", [panel().getByRole("heading", { name: "Pipelines", level: 2 }), part("Pipeline server")], {
        notes: [
          { at: label(panel(), "Ready in dev-project"), n: 1, pos: "r" },
          // The whole picker (text field and toggle), the badge on its top-right corner: on its right it covered the toggle.
          { at: panel().locator(".pf-v6-c-menu-toggle").filter({ has: page.getByRole("combobox") }), n: 2, pos: "tr" },
          { at: row(storage, "Endpoint"), n: 3, pos: "l" },
          { at: storage.getByRole("button", { name: "Show the S3 secret key" }), n: 4, pos: "r" },
          { at: part("Pipeline server").getByRole("link", { name: /Open pipelines/ }), n: 6, pos: "l" },
          { at: row(part("Pipeline server"), "Connection in the dashboard"), n: 5, pos: "l" },
        ],
        pad: 24,
      });

      // A pipeline server an earlier version named nightly-dspa: the
      // workbench notice with Recreate, and its confirmation.
      await open("/test-resources?tab=pipelines&project=dev-project", "env-old-name");
      const oldServer = part("Pipeline server");
      await crop("environment-pipelines-old-name", oldServer, {
        notes: [
          { at: oldServer.getByText(/Workbench pipelines don.t work/), n: 1, pos: "r" },
          { at: oldServer.getByRole("button", { name: "Recreate with the standard name" }), n: 2, pos: "r" },
        ],
        pad: 24,
      });
      await oldServer.getByRole("button", { name: "Recreate with the standard name" }).click();
      await wait(800);
      await page.getByRole("dialog").getByRole("button", { name: "Details" }).click().catch(() => {});
      await wait(300);
      await crop("environment-pipelines-recreate-dialog", page.locator(".pf-v6-c-modal-box"), { noFit: true, pad: 6 });
      await page.keyboard.press("Escape");

      await open("/test-resources?tab=mlflow", "env-live");
      await crop("environment-mlflow", [panel().getByRole("heading", { name: "MLflow", level: 2 }), part("MLflow server")], {
        notes: [
          // Below: the title is on its left and the ownership label on its right.
          { at: label(part("MLflow server"), "Ready"), n: 1, pos: "b" },
          { at: label(part("MLflow server"), "Not managed by this tool"), n: 2, pos: "r" },
        ],
        pad: 24,
      });

      // Models: the gateway and the chosen model (Qwen3, one of three in the
      // project); the picker with a model that has no room; a nightly preset
      // image that is missing.
      await open("/test-resources?tab=models&project=dev-project&model=qwen3-1-7b", "env-models-several");
      const gateway = part("Inference gateway");
      const qwen = part("Qwen3 1.7B (CPU, tool calling)");
      await crop("environment-models", [panel().getByRole("heading", { name: "Models", level: 2 }), qwen], {
        notes: [
          { at: label(panel(), "Ready in dev-project"), n: 1, pos: "r" },
          { at: panel().getByRole("button", { name: "Model", exact: true }), n: 2, pos: "r" },
          { at: row(gateway, "External address"), n: 3, pos: "l" },
          { at: row(qwen, "Use case"), n: 4, pos: "l" },
          { at: row(qwen, "Address"), n: 5, pos: "l" },
          { at: row(qwen, "Hardware profile"), n: 6, pos: "l" },
          { at: row(qwen, "Token authentication"), n: 7, pos: "l" },
        ],
        pad: 24,
      });
      await open("/test-resources?tab=models&project=dev-project", "env-models-capacity");
      const picker = panel().getByRole("button", { name: "Model", exact: true });
      await picker.click();
      const options = page.getByRole("listbox", { name: "Models this tool can set up" });
      await crop("environment-models-picker", [picker, options], {
        notes: [
          // Adjacent options: the boxes stay inside them, so they do not touch.
          { at: options.getByRole("option", { name: /Granite Embedding/ }), n: 1, pos: "r", pad: -3 },
          { at: options.getByRole("option", { name: /Qwen3 1.7B/ }), n: 2, pos: "r", pad: -3 },
        ],
        pad: 24,
      });
      await page.keyboard.press("Escape");
      await open("/test-resources?tab=models&project=dev-project&model=qwen3-1-7b", "env-models-several");
      const use = page.getByRole("region", { name: "Use it" });
      await crop("environment-models-use-qwen3", use, {
        notes: [
          { at: row(use, "Tool calling"), n: 1, pos: "l" },
          { at: row(use, "Reasoning"), n: 2, pos: "l" },
        ],
        pad: 24,
      });
      // A model deployed from the dashboard in a project the gateway does
      // not list: named, with Allow.
      await open("/test-resources?tab=models&project=dev-project", "env-models-not-allowed");
      const listed = part("Inference gateway");
      await crop("environment-models-not-allowed", listed, {
        notes: [
          { at: listed.getByText(/can't be reached: its project isn't allowed/), n: 1, pos: "r" },
          { at: listed.getByRole("button", { name: "Allow fraud-detection" }), n: 2, pos: "r" },
          { at: row(listed, "Projects allowed on the gateway"), n: 3, pos: "l" },
        ],
        pad: 24,
      });
      await open("/test-resources?tab=models&project=dev-project", "env-models-image-missing");
      const broken = part("Qwen2.5 0.5B Instruct (CPU)");
      await crop("environment-models-image", broken, {
        notes: [
          { at: broken.getByText("The cluster preset's image is not published yet"), n: 1, pos: "r" },
          { at: broken.getByRole("button", { name: "Use the newest released Red Hat image" }), n: 2, pos: "r" },
        ],
        pad: 24,
      });

      // MaaS: the platform's parts, and this tool's model published.
      await open("/test-resources?tab=maas", "env-maas-published");
      await crop("environment-maas", [panel().getByRole("heading", { name: "MaaS", level: 2 }), panel().getByRole("grid", { name: "This tool's models in MaaS" })], {
        notes: [
          { at: part("Database"), n: 1, pos: "r" },
          { at: part("Authorino TLS"), n: 2, pos: "r" },
          { at: panel().getByRole("button", { name: "Unpublish" }), n: 3, pos: "r" },
        ],
        pad: 24,
      });

      // Playground: a ready playground; Set up with the models it uses.
      await open("/test-resources?tab=playground&project=dev-project", "env-playground-ready");
      const playground = part("Playground");
      await crop("environment-playground", [panel().getByRole("heading", { name: "Playground", level: 2 }), playground], {
        notes: [
          { at: label(panel(), "Ready in dev-project"), n: 1, pos: "r" },
          { at: part("Models").getByText("Qwen2.5 0.5B Instruct (CPU)"), n: 2, pos: "l" },
          { at: playground.getByRole("link", { name: /Open the playground/ }), n: 3, pos: "r" },
        ],
        pad: 24,
      });
      await open("/test-resources?tab=playground&project=dev-project", "env-playground-none");
      const setUp = panel().getByRole("button", { name: "Set up the playground in dev-project" });
      await crop("environment-playground-setup", [part("Models"), setUp], {
        notes: [
          { at: panel().getByRole("checkbox", { name: "Select row 0" }), n: 1, pos: "l" },
          { at: panel().getByText(/^Not used unless you choose it/).first(), n: 2, pos: "r" },
          { at: setUp, n: 3, pos: "r" },
        ],
        pad: 24,
      });

      // Samples: the MLflow tab's Sample data and the Pipelines tab's sample pipeline.
      const section = (name) => page.getByRole("region", { name });
      await open("/test-resources?tab=mlflow&project=dev-project", "env-samples");
      const samples = section("Sample data");
      await crop("environment-mlflow-samples", samples, {
        notes: [
          { at: samples.getByRole("button", { name: "Add all" }), n: 1, pos: "l" },
          { at: label(samples, "Partly added"), n: 2, pos: "r" },
          { at: samples.getByRole("button", { name: "Regenerate the agent and prompt demo" }), n: 3, pos: "r" },
          { at: label(samples, "Not managed by this tool"), n: 4, pos: "r" },
        ],
        pad: 24,
      });
      await open("/test-resources?tab=pipelines&project=dev-project", "env-samples");
      const sample = section("Run a sample pipeline");
      await crop("environment-pipeline-sample", sample, {
        notes: [
          { at: sample.getByRole("link", { name: "Download rhoai-nightly-sample.yaml" }), n: 1, pos: "r" },
          { at: sample.getByRole("link", { name: "Open pipelines in dev-project" }), n: 2, pos: "r" },
          { at: sample.getByRole("button", { name: "Copy the commands" }), n: 3, pos: "l" },
        ],
        pad: 24,
      });

      await open("/test-resources?tab=pipelines", "s3-migrate");
      const old = part("S3 storage");
      await old.getByRole("button", { name: "Storage of an earlier version" }).click();
      await wait(400);
      await crop("s3-migration-needed", old, {
        notes: [
          { at: label(old, "Needs attention"), n: 1, pos: "r" },
          { at: old.getByRole("button", { name: "Migrate storage" }), n: 2, pos: "l" },
          { at: old.getByRole("button", { name: "Storage of an earlier version" }), n: 3, pos: "l" },
        ],
        pad: 24,
      });
      await old.getByRole("button", { name: "Migrate storage" }).click();
      await wait(800);
      await crop("s3-migrate-dialog", page.locator(".pf-v6-c-modal-box"), { noFit: true, pad: 6 });
      await page.keyboard.press("Escape");

      await open("/test-resources?tab=pipelines", "s3-migrate-remove");
      await part("S3 storage").getByRole("button", { name: "S3 storage actions" }).click();
      await page.getByRole("menuitem", { name: /Delete old storage/ }).click();
      await wait(800);
      await crop("s3-remove-dialog", page.locator(".pf-v6-c-modal-box"), { noFit: true, pad: 6 });
      await page.keyboard.press("Escape");

      await open("/test-resources?tab=pipelines", "s3-incomplete");
      const inc = part("S3 storage");
      await crop("s3-incomplete", inc, {
        notes: [
          { at: label(inc, "Needs attention"), n: 1, pos: "r" },
          { at: inc.locator(".pf-v6-c-alert").first(), n: 2, pos: "l" },
          { at: inc.getByRole("button", { name: "Repair" }), n: 3, pos: "l" },
        ],
        pad: 24,
      });
    },

    async diagnostics() {
      const one = async (sc, name, title, { notesFor } = {}) => {
        await open("/diagnostics", sc);
        const item = problem(title);
        await item.getByRole("button", { name: "Details" }).click();
        await wait(500);
        // Show the whole command and all evidence.
        const more = item.getByRole("button", { name: /^(Show content|Show more)$/ });
        for (let i = 0; i < 6 && (await more.count()) > 0; i++) await more.first().click();
        await wait(500);
        await crop(name, item, { notes: notesFor ? await notesFor(item) : [], pad: notesFor ? 18 : 12 });
      };
      await one("diag-prerequisite-missing", "diag-prerequisite-missing", "Prerequisite operator Job Set Operator is not installed", {
        notesFor: async (item) => [
          { at: item.getByText("Observed", { exact: true }), n: 1, pos: "l", box: false },
          { at: item.getByText("Fix", { exact: true }), n: 2, pos: "l", box: false },
          { at: item.getByText("Command", { exact: true }), n: 3, pos: "l", box: false },
          { at: item.getByRole("button", { name: "Copy command" }), n: 4, pos: "r" },
        ],
      });
      await one("diag-operand-missing", "diag-operand-missing", "its JobSetOperator/cluster does not exist");
      await one("diag-module-backoff", "diag-module-backoff", "The trainer module operator has not retried yet", {
        notesFor: async (item) => [{ at: item.getByRole("button", { name: "Fix", exact: true }), n: 1, pos: "l" }],
      });
      await one("diag-certificate-stale", "diag-certificate-stale", "reports Ready, but its Secret");
      await one("diag-apply-failures", "diag-apply-failures", "kuberay-operator must be recreated");
      await one("diag-upgrade-gates", "diag-upgrade-gates", "waits on an upgrade gate");
      await open("/diagnostics", "diag-healthy");
      await crop("diag-all-passing", page.locator(".pf-v6-c-alert").first());
    },

    // --- GIFs ---

    async "gif-update"() {
      await operatorFlow("update", "update-flow", "Update to latest", /^Update/);
    },

    async "gif-install"() {
      await operatorFlow("install", "install-flow", /^Install latest nightly/, /^Install/);
    },

    async "gif-migrate"() {
      await open("/test-resources?tab=pipelines", "s3-migrate");
      gif("s3-migrate", await mainClip());
      await frame(1200);
      await clickShown(page.getByRole("listitem", { name: "S3 storage", exact: true }).getByRole("button", { name: "Migrate storage" }));
      await wait(700);
      await hidePointer();
      await frame(2600);
      await clickShown(page.locator(".pf-v6-c-modal-box").getByRole("button", { name: "Migrate storage" }), 700);
      await hidePointer();
      // The four steps, two events each, then the outcome.
      for (let i = 0; i < 4; i++) {
        await advance(2);
        await wait(600);
        await frame(900);
      }
      await advance(1);
      await wait(900);
      await frame(1200);
      // The outcome: the data is in the new storage; the old storage is deleted later, apart.
      await page.locator(".pf-v6-c-modal-box__body").evaluate((el) => el.scrollTo(0, el.scrollHeight));
      await wait(400);
      await frame(3200);
    },

    async "gif-repair"() {
      await open("/test-resources?tab=pipelines", "s3-incomplete");
      gif("s3-repair", await mainClip());
      await frame(1200);
      await clickShown(page.getByRole("listitem", { name: "S3 storage", exact: true }).getByRole("button", { name: "Repair" }));
      await wait(700);
      await hidePointer();
      await frame(2200);
      await clickShown(page.locator(".pf-v6-c-modal-box").getByRole("button", { name: "Repair", exact: true }), 700);
      await hidePointer();
      await wait(500);
      await frame(1000);
      await release();
      await wait(1200);
      await settle();
      await frame(3000);
    },

    async "gif-diagnostics"() {
      await open("/diagnostics", "diag-prerequisite-missing");
      gif("diagnostics-copy-command", await mainClip());
      await frame(1000);
      const item = problem("Prerequisite operator Job Set Operator is not installed");
      await clickShown(item.getByRole("button", { name: "Details" }));
      await wait(600);
      await hidePointer();
      await frame(1400);
      await scrollTo(item, 10);
      await frame(1000);
      await clickShown(item.getByRole("button", { name: "Show content" }), 700);
      await wait(500);
      await hidePointer();
      await frame(1800);
      await clickShown(item.getByRole("button", { name: "Copy command" }), 700);
      await wait(1000);
      await frame(2400);
    },

    async "gif-pr-search"() {
      await open("/builds", "update-available");
      gif("build-explorer-pr-search", await mainClip(640));
      await frame(1000);
      const search = page.getByPlaceholder("Image, commit SHA, PR # or PR URL");
      await clickShown(search, 600);
      for (const ch of "#5123") {
        await search.type(ch);
        await frame(220);
      }
      await hidePointer();
      await search.press("Enter");
      await wait(1500);
      await settle();
      await frame(3500);
    },
  };

  if (job !== "pages" && job !== "themes") vw = CW;
  if (!jobs[job]) throw new Error(`unknown job ${job}; jobs: ${Object.keys(jobs).join(" ")}`);
  await jobs[job]();
  await page.setViewportSize({ width: W, height: H });
  return `${job}: done`;
}
