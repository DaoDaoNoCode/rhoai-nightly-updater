/** Simulate the tab being hidden or shown, and fire visibilitychange. */
export function setDocumentHidden(hidden: boolean, opts: { silent?: boolean } = {}): void {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => (hidden ? "hidden" : "visible") });
  if (!opts.silent) document.dispatchEvent(new Event("visibilitychange"));
}

/** A JSON fetch Response. */
export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

/** A promise whose resolution the test controls. */
export function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/**
 * An SSE Response whose body the test writes to. `push` sends raw text, and
 * `close` ends the body, `fail` errors it like a dropped connection.
 */
export function sseResponse(): { response: Response; push: (text: string) => void; close: () => void; fail: () => void } {
  let ctrl!: ReadableStreamDefaultController<Uint8Array>;
  const stream = new ReadableStream<Uint8Array>({ start(c) { ctrl = c; } });
  const encoder = new TextEncoder();
  return {
    response: new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } }),
    push: (text) => ctrl.enqueue(encoder.encode(text)),
    close: () => ctrl.close(),
    fail: () => ctrl.error(new TypeError("network error")),
  };
}

export function sseEvent(step: string, status: string, message = "", extra: Record<string, unknown> = {}): string {
  return "data: " + JSON.stringify({ step, status, message, elapsedMs: 0, ...extra }) + "\n\n";
}
