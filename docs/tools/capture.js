// Captures the docs images from the docs mock backend. Run by
// docs/tools/screenshots.sh through `playwright-cli run-code`: the page is
// open on the mock, and GET /__docs/job names the job and the output
// directory. Full pages are written at 2x and scaled to 1440 px later; crops
// stay 2x; GIF frames go to frames/<gif>/NNN-<ms>.png.
async (page) => {
  const origin = page.url().match(/^https?:\/\/[^/]+/)[0];
  const { name: job, out } = await (await page.request.get(`${origin}/__docs/job`)).json();
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

  /** Opens path in scenario sc, in the light or dark theme, with clean storage. */
  async function open(path, sc, { dark = false, height = H, width = vw } = {}) {
    if (sc) await scenario(sc);
    await page.setViewportSize({ width, height });
    await page.goto(origin + "/__docs/state");
    await page.evaluate((t) => { localStorage.clear(); sessionStorage.clear(); localStorage.setItem("pf-theme", t); }, dark ? "dark" : "light");
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
   * [{ at: locator, n?: number, box?: false, pos?: "tl"|"tr"|"l"|"r" }].
   * Returns the rectangles drawn, so a crop can include them.
   */
  async function annotate(notes, name = "") {
    const items = [];
    for (const nt of notes) {
      const b = await nt.at.boundingBox().catch(notFound(name, `callout ${nt.n}`, nt.at));
      if (!b) throw new Error(`${name}: callout ${nt.n} target not visible: ${nt.at}`);
      items.push({ b, n: nt.n, box: nt.box !== false, pos: nt.pos || "tl" });
    }
    return page.evaluate(({ items, RED }) => {
      const layer = document.createElement("div");
      layer.id = "docs-annotations";
      layer.style.cssText = "position:fixed;inset:0;pointer-events:none;z-index:2147483647";
      const rects = [];
      const pad = 5;
      const R = 15;
      for (const { b, n, box, pos } of items) {
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
      await wait(350);
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
    ["s3-running", "/test-resources", "page-test-resources"],
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

      await open("/components", "healthy");
      await crop("masthead-version", head, { notes: [{ at: page.getByText("v2.0.0", { exact: true }), n: 1, pos: "l" }] });
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
    },

    async builds() {
      await open("/builds", "update-available");
      const search = page.getByPlaceholder("Image, commit SHA or PR #");
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
    },

    async resources() {
      await open("/test-resources", "s3-running");
      const storage = card("Storage");
      await crop("s3-running", storage, {
        notes: [
          { at: label(storage, "Running"), n: 1, pos: "r" },
          { at: storage.getByRole("link", { name: /Open admin UI/ }), n: 2, pos: "l" },
          { at: storage.getByText(/^Tear down is blocked/).locator("xpath=..").first(), n: 3, pos: "l" },
        ],
        pad: 26,
      });
      await crop("pipeline-servers", card("Pipeline servers"));

      await open("/test-resources", "s3-pending");
      const pending = card("Storage");
      await crop("s3-migration-pending", pending, {
        notes: [
          { at: label(pending, "MinIO (migration pending)"), n: 1, pos: "r" },
          { at: pending.getByRole("button", { name: "Migrate to SeaweedFS" }), n: 2, pos: "l" },
        ],
        pad: 24,
      });
      await pending.getByRole("button", { name: "Migrate to SeaweedFS" }).click();
      await wait(800);
      await crop("s3-migrate-dialog", page.locator(".pf-v6-c-modal-box"), { noFit: true, pad: 6 });
      await page.keyboard.press("Escape");

      await open("/test-resources", "s3-incomplete");
      const inc = card("Storage");
      await crop("s3-incomplete", inc, {
        notes: [
          { at: label(inc, "Incomplete"), n: 1, pos: "r" },
          { at: inc.getByText(/^Deployment seaweedfs is scaled to 0/).first(), n: 2, pos: "l" },
          { at: inc.getByRole("button", { name: "Repair" }), n: 3, pos: "l" },
        ],
        pad: 24,
      });

      await open("/test-resources", "s3-none");
      await crop("s3-setup", card("Storage"), { notes: [{ at: card("Storage").getByRole("button", { name: /^Set up/ }).first(), n: 1, pos: "l" }] });
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
      await open("/test-resources", "s3-pending");
      gif("s3-migrate", await mainClip());
      await frame(1200);
      await clickShown(card("Storage").getByRole("button", { name: "Migrate to SeaweedFS" }));
      await wait(700);
      await hidePointer();
      await frame(2400);
      await clickShown(page.locator(".pf-v6-c-modal-box").getByRole("button", { name: "Migrate and start fresh" }), 700);
      await hidePointer();
      await wait(500);
      await frame(1000);
      await release();
      await wait(1200);
      await settle();
      await frame(3000);
    },

    async "gif-repair"() {
      await open("/test-resources", "s3-incomplete");
      gif("s3-repair", await mainClip());
      await frame(1200);
      await clickShown(card("Storage").getByRole("button", { name: "Repair" }));
      await wait(700);
      await hidePointer();
      await frame(2200);
      await clickShown(page.locator(".pf-v6-c-modal-box").getByRole("button", { name: "Repair S3 storage" }), 700);
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
      const search = page.getByPlaceholder("Image, commit SHA or PR #");
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

  if (job !== "pages") vw = CW;
  if (!jobs[job]) throw new Error(`unknown job ${job}; jobs: ${Object.keys(jobs).join(" ")}`);
  await jobs[job]();
  await page.setViewportSize({ width: W, height: H });
  return `${job}: done`;
}
