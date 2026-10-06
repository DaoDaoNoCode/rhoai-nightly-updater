import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { withImageRefs } from "./ImageRef";
import { NotRecorded, RelativeTime } from "./RelativeTime";

const IMAGE = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:0aa1d3d94c6ea06a6627a9ad822f30574dbac6200e007c9393a596d3e3e564bc";

describe("withImageRefs (UX round 2, Diagnostics details)", () => {
  it("shows a digest-pinned reference as tag@short digest with a copy of the full reference", () => {
    render(<p>{withImageRefs(`Nightly catalog is healthy (${IMAGE})`)}</p>);
    expect(screen.getByText("rhoai-3.6@0aa1d3d94c6e")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: `Copy image reference ${IMAGE}` })).toBeInTheDocument();
    expect(screen.queryByText(IMAGE)).not.toBeInTheDocument();
    expect(screen.getByText(/Nightly catalog is healthy:/)).toBeInTheDocument();
  });

  it("names an untagged image by its repository and drops the quotes around it", () => {
    const minio = "quay.io/hummingbird-community/minio@sha256:25268b5a6539d9ffc7d23b89a2ba846d12a49aac4e81172336700222818d5f45";
    const { container } = render(<p>{withImageRefs(`Back-off pulling image "${minio}": ErrImagePull`)}</p>);
    expect(screen.getByText("minio@25268b5a6539")).toBeInTheDocument();
    expect(container.textContent).not.toContain('"');
  });

  it("leaves text without an image reference unchanged", () => {
    expect(withImageRefs("All 2 nodes are ready")).toBe("All 2 nodes are ready");
  });
});

describe("RelativeTime and NotRecorded", () => {
  it("inside a sentence the time is a plain <time> that inherits the text style", () => {
    const { container } = render(<p>built <RelativeTime date={new Date(Date.now() - 2 * 3600_000)} size="inherit" /></p>);
    const time = container.querySelector("time")!;
    expect(time).toHaveTextContent("2h ago");
    expect(time.closest(".pf-v6-c-timestamp")).toBeNull();
  });

  it("a missing value is a muted dash with a screen-reader name", () => {
    render(<NotRecorded />);
    expect(screen.getByText("—")).toHaveAttribute("aria-hidden", "true");
    expect(screen.getByText("Not recorded in the image labels")).toHaveClass("pf-v6-screen-reader");
  });
});
