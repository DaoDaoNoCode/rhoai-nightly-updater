import React from "react";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { NotRecorded, RelativeTime } from "./RelativeTime";

describe("RelativeTime and NotRecorded inside a sentence", () => {
  it("adds no <div> to the paragraph (valid DOM nesting) and keeps the date in a tooltip", async () => {
    const date = new Date(Date.now() - 3 * 60 * 60 * 1000);
    const { container } = render(
      <p>
        Changed since the snapshot (<RelativeTime date={date} size="inherit" />), commit <NotRecorded />.
      </p>,
    );
    const p = container.querySelector("p")!;
    expect(p.querySelector("div")).toBeNull();

    const time = p.querySelector("time")!;
    expect(time.getAttribute("dateTime")).toBe(date.toISOString());
    fireEvent.mouseEnter(time);
    await waitFor(() => expect(screen.getByRole("tooltip")).toHaveTextContent(date.toLocaleString()));
    fireEvent.mouseLeave(time);
  });

  it("shows the NotRecorded explanation on hover and to screen readers", async () => {
    const { container } = render(<p><NotRecorded what="No commit label" /></p>);
    expect(container.querySelector("p div")).toBeNull();
    expect(screen.getByText("No commit label")).toHaveClass("pf-v6-screen-reader");
    fireEvent.mouseEnter(container.querySelector('[aria-hidden="true"]')!);
    await waitFor(() => expect(screen.getByRole("tooltip")).toHaveTextContent("No commit label"));
  });
});
