import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";
import { setDocumentHidden } from "./utils";

afterEach(() => {
  cleanup();
  setDocumentHidden(false, { silent: true });
});
