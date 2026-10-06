import React from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { CardList, CardListItem } from "./CardList";

const row = (props: Partial<React.ComponentProps<typeof CardListItem>> = {}) =>
  render(
    <CardList aria-label="Rows">
      <CardListItem actions={<button type="button">Act</button>} {...props}>
        <span>Text</span>
      </CardListItem>
    </CardList>,
  );

describe("CardListItem actions layout", () => {
  it("puts the actions beside the content from md by default", () => {
    row();
    const flex = screen.getByText("Text").closest(".pf-v6-l-flex");
    expect(flex).toHaveClass("pf-m-column", "pf-m-row-on-md");
    expect(flex).not.toHaveClass("pf-m-row-on-xl");
  });

  it("keeps them under the content until a later breakpoint when asked", () => {
    row({ actionsBesideFrom: "xl" });
    const flex = screen.getByText("Text").closest(".pf-v6-l-flex");
    expect(flex).toHaveClass("pf-m-column", "pf-m-row-on-xl", "pf-m-justify-content-space-between-on-xl");
    expect(flex).not.toHaveClass("pf-m-row-on-md");
  });
});
