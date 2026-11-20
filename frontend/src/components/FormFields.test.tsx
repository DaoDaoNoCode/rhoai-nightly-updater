import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { FieldHelperText, NestedFields } from "./FormFields";
import { AudienceFields, DatabaseChoice, LimitFields } from "./environment/MaaSDialogFields";
import { ReinstallPanel } from "./ReinstallPanel";
import { nightlyStatus, renderWithApp } from "../test/providers";
import type { MaaSDatabase } from "../types";

const MARGIN_VAR = "--pf-v6-c-form__helper-text--MarginBlockStart";

/** The helper text's FormHelperText, which gives it PatternFly's spacer-xs below the control. */
const formHelperOf = (text: string | RegExp) => screen.getByText(text).closest(".pf-v6-c-form__helper-text");

/** The value the helper text's margin comes from, as the nearest element that sets it says. */
function inheritedMargin(el: Element): string | null {
  for (let e: Element | null = el; e; e = e.parentElement) {
    const v = (e as HTMLElement).style?.getPropertyValue(MARGIN_VAR);
    if (v) return v;
  }
  return null;
}

afterEach(() => vi.unstubAllGlobals());

const BASE = "var(--pf-v6-c-form__helper-text--MarginBlockStart--base)";

describe("field helper text", () => {
  it("FieldHelperText is PatternFly's FormHelperText > HelperText > HelperTextItem, with the id on the HelperText", () => {
    render(<FieldHelperText id="x-help" variant="error">Enter a URL.</FieldHelperText>);
    const item = screen.getByText("Enter a URL.").closest(".pf-v6-c-helper-text__item");
    expect(item).toHaveClass("pf-m-error");
    expect(item?.closest(".pf-v6-c-helper-text")).toHaveAttribute("id", "x-help");
    expect(item?.closest(".pf-v6-c-form__helper-text")).not.toBeNull();
  });

  it("NestedFields sets the helper-text margin back to PatternFly's base value for what it holds", () => {
    render(<NestedFields><FieldHelperText>Help</FieldHelperText></NestedFields>);
    expect(inheritedMargin(formHelperOf("Help")!)).toBe(BASE);
  });
});

describe("fields inside a radio's body get PatternFly's helper-text spacing", () => {
  const db: MaaSDatabase = { current: "development", namespace: "redhat-ai-gateway-infra", needed: false, default: "development", choices: [{ id: "external", plan: [] }, { id: "cnpg", plan: [] }] };

  it("Change database: the connection URL's error is FormHelperText under the input group, with the stack's negative margin undone", () => {
    render(<DatabaseChoice db={db} except="development" idPrefix="change" onChange={() => {}} value={{ kind: "external", connectionUrl: "", urlTouched: true }} />);
    const helper = formHelperOf(/connection URL/)!;
    expect(helper).not.toBeNull();
    // Right under the input group, inside the Connection URL form group, inside the radio's body.
    expect(helper.previousElementSibling).toHaveClass("pf-v6-c-input-group");
    expect(helper.closest(".pf-v6-c-radio__body")).not.toBeNull();
    expect(screen.getByLabelText(/Connection URL/)).toHaveAccessibleDescription(/connection URL/);
    // The radio group is a stacked form group, which sets a negative helper margin that inherits.
    expect(helper.closest(".pf-v6-c-form__group-control.pf-m-stack")).not.toBeNull();
    expect(inheritedMargin(helper)).toBe(BASE);
  });

  it("Change database: the URL's help text, before any error, is spaced the same way", () => {
    render(<DatabaseChoice db={db} except="development" idPrefix="change" onChange={() => {}} value={{ kind: "external", connectionUrl: "" }} />);
    const helper = formHelperOf(/Stored only in Secret maas-db-config/)!;
    expect(helper.previousElementSibling).toHaveClass("pf-v6-c-input-group");
    expect(inheritedMargin(helper)).toBe(BASE);
  });

  it("custom token rate rows: a row's problem is FormHelperText under the row", () => {
    render(<LimitFields idPrefix="change" onChange={() => {}} value={{ preset: "custom", rows: [{ limit: "", amount: "1", unit: "h" }] }} />);
    const helper = document.getElementById("change-rate-0-problem")!.closest(".pf-v6-c-form__helper-text")!;
    expect(helper).not.toBeNull();
    expect(helper.closest(".pf-v6-c-radio__body")).not.toBeNull();
    expect(inheritedMargin(helper)).toBe(BASE);
  });

  it("custom token rate rows: a period problem marks the unit picker as PatternFly's danger status", () => {
    render(<LimitFields idPrefix="change" onChange={() => {}} value={{ preset: "custom", rows: [{ limit: "5", amount: "1", unit: "h" }, { limit: "9", amount: "1", unit: "h" }] }} />);
    const first = screen.getByRole("button", { name: "Limit 1: period unit" });
    const second = screen.getByRole("button", { name: "Limit 2: period unit" });
    expect(first).not.toHaveAttribute("aria-invalid");
    expect(first).not.toHaveClass("pf-m-danger");
    expect(second).toHaveAttribute("aria-invalid", "true");
    expect(second).toHaveClass("pf-m-danger");
    expect(second).toHaveAccessibleDescription("hours");
  });

  it("specific groups and users: each field's help and the audience problem are spaced as standalone fields", () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("[]", { status: 200 })));
    render(<AudienceFields idPrefix="change" onChange={() => {}} value={{ everyone: false, groups: "", users: "" }} />);
    for (const text of [/Use groups for people/, /For service accounts/]) {
      const helper = formHelperOf(text)!;
      expect(helper.previousElementSibling?.tagName).toBe("SPAN"); // the TextArea's wrapper
      expect(inheritedMargin(helper)).toBe(BASE);
    }
    expect(inheritedMargin(document.getElementById("change-audience-problem")!)).toBe(BASE);
    // The group's own note, a row of the stacked group, keeps PatternFly's stack offset.
    expect(inheritedMargin(formHelperOf("Applies to every model you publish here.")!)).toBeNull();
  });
});

describe("Install a different build: helper text is FormHelperText, not a bare HelperText touching the field", () => {
  it("the FBC image's error and the channel override's help", async () => {
    await renderWithApp(<ReinstallPanel status={nightlyStatus()} nightlyTags={[]} tagsLoading={false} prerequisitesMet runOperator={() => true} />).settled();
    fireEvent.click(screen.getByLabelText(/A specific FBC image/));
    fireEvent.change(screen.getByRole("textbox", { name: /^FBC image/ }), { target: { value: "quay.io/other/image:1" } });
    const image = formHelperOf(/Only quay.io\/rhoai\/rhoai-fbc-fragment images/)!;
    expect(image).not.toBeNull();
    expect(image.querySelector(".pf-v6-c-helper-text__item")).toHaveClass("pf-m-error");
    expect(screen.getByRole("textbox", { name: /^FBC image/ })).toHaveAccessibleDescription(/Only quay.io/);
    expect(formHelperOf("Optional. Only set it if the detected channel is wrong.")).not.toBeNull();
  });
});
