import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { format } from "node:util";
import { afterEach, beforeEach, vi } from "vitest";
import { setDocumentHidden } from "./utils";

// React's development warnings (a list item without a key, a <div> inside a
// <p>, an update outside act, ...) are bugs in what the UI renders, not
// noise: a test that logs an error fails, with the message. React 19's
// warnings have no "Warning: " prefix to tell them apart, and nothing else
// in the suite logs errors, so every console.error counts. A test that
// expects one replaces this with its own vi.spyOn(console, "error").
let reactWarnings: string[] = [];
beforeEach(() => {
  reactWarnings = [];
  const original = console.error;
  vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => {
    reactWarnings.push(format(...args));
    original(...args);
  });
});

// A block inside a paragraph is invalid HTML: a browser closes the <p> before
// it, so the page's structure is not what React rendered. React warns only
// once per component pair and file; this checks every test's page.
function blocksInParagraphs(): string[] {
  return Array.from(document.body.querySelectorAll("p div, p p, p ul, p ol, p pre, p table")).map((el) => {
    const p = el.closest("p");
    return `<${el.tagName.toLowerCase()} class="${el.getAttribute("class") ?? ""}"> inside <p>: "${(p?.textContent ?? "").slice(0, 80)}"`;
  });
}

afterEach(() => {
  const nested = blocksInParagraphs();
  cleanup();
  setDocumentHidden(false, { silent: true });
  const found = reactWarnings;
  reactWarnings = [];
  if (found.length > 0) throw new Error(`Logged an error (a React warning, or another console.error):\n${found.join("\n\n")}`);
  if (nested.length > 0) throw new Error(`Invalid HTML, a block inside a paragraph:\n${nested.join("\n")}`);
});
