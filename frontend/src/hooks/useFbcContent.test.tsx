import { describe, expect, it, vi } from "vitest";
import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { useFbcContent } from "./useFbcContent";
import { FBCContentModal } from "../components/FBCContentModal";
import { LiveAnnouncerProvider } from "../state/LiveAnnouncer";
import type { FBCContentResponse } from "../types";
import { deferred, jsonResponse } from "../test/utils";

function content(tag: string, commit?: string): FBCContentResponse {
  return {
    tag,
    image: `quay.io/rhoai/rhoai-fbc-fragment:${tag}`,
    bundleName: `rhods-operator.${tag}`,
    relatedImages: [{ name: "odh-dashboard", image: `quay.io/rhoai/odh-dashboard@sha256:${tag}`, category: "core", gitCommit: commit }],
    categories: { core: 1 },
  };
}

const A = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6";
const B = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5";

function stubContent() {
  const slowLabelsForA = deferred<Response>();
  const signals: Record<string, AbortSignal | undefined> = {};
  vi.stubGlobal("fetch", vi.fn((url: string, init: RequestInit) => {
    const params = new URL(url, "http://x").searchParams;
    const image = params.get("image");
    const labels = params.get("labels") === "true";
    signals[`${image}|${labels}`] = init.signal ?? undefined;
    if (image === A && labels) return slowLabelsForA.promise;
    const tag = image === A ? "rhoai-3.6" : "rhoai-3.5";
    return Promise.resolve(jsonResponse(content(tag, labels ? `sha-${tag}` : undefined)));
  }));
  return { slowLabelsForA, signals };
}

describe("useFbcContent (A06-8)", () => {
  it("never shows labels from a previous image", async () => {
    const { slowLabelsForA, signals } = stubContent();
    const { result, rerender } = renderHook(({ image }) => useFbcContent(image), { initialProps: { image: A as string | null } });
    await waitFor(() => expect(result.current.data?.tag).toBe("rhoai-3.6"));
    rerender({ image: B });
    await waitFor(() => expect(result.current.data?.relatedImages?.[0].gitCommit).toBe("sha-rhoai-3.5"));
    expect(signals[`${A}|true`]?.aborted).toBe(true);
    // A's slow labels finally arrive: they must be ignored.
    await act(async () => {
      slowLabelsForA.resolve(jsonResponse(content("rhoai-3.6", "STALE-FROM-A")));
    });
    expect(result.current.data?.tag).toBe("rhoai-3.5");
    expect(result.current.data?.relatedImages?.[0].gitCommit).toBe("sha-rhoai-3.5");
  });

  it("the preview modal shows the current image after reopening with another image", async () => {
    const { slowLabelsForA } = stubContent();
    const ui = (image: string, isOpen: boolean) => (
      <LiveAnnouncerProvider>
        <MemoryRouter>
          <FBCContentModal image={image} isOpen={isOpen} onClose={() => {}} />
        </MemoryRouter>
      </LiveAnnouncerProvider>
    );
    const view = render(ui(A, true));
    await screen.findByText("Catalog: rhods-operator.rhoai-3.6");
    view.rerender(ui(A, false));
    view.rerender(ui(B, true));
    await screen.findByText("Catalog: rhods-operator.rhoai-3.5");
    await act(async () => {
      slowLabelsForA.resolve(jsonResponse(content("rhoai-3.6", "STALE-FROM-A")));
    });
    expect(screen.queryByText(/STALE/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Component category: All categories" })).toHaveTextContent("1");
  });
});
