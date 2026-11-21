import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { OutcomeAlert } from "./OutcomeAlert";

// PatternFly's Alert renders its description element whenever its children
// are truthy (Alert.js: `children && <div className="pf-v6-c-alert__description">`),
// and an array of empty parts is truthy: an empty description adds space
// under the title.
const description = (container: HTMLElement) => container.querySelector(".pf-v6-c-alert__description");

describe("OutcomeAlert", () => {
  it.each([
    ["without logs", false],
    ["with logs offered", true],
  ])("renders no description for a success with nothing under its title (%s)", (_name, logs) => {
    const { container } = render(<OutcomeAlert result={{ success: true, message: "Deploying main", logs: [] }} failedTitle="Deploy failed" logs={logs} />);
    expect(screen.getByText("Deploying main")).toBeInTheDocument();
    expect(description(container)).toBeNull();
  });

  it("renders no description for a failure without message, words or logs", () => {
    const { container } = render(<OutcomeAlert result={{ success: false, message: "", logs: [] }} failedTitle="Fix failed" logs>{null}{false}</OutcomeAlert>);
    expect(description(container)).toBeNull();
  });

  it("shows the logs of a success under Show details", () => {
    const { container } = render(<OutcomeAlert result={{ success: true, message: "Fixed", logs: ["deleted x"] }} failedTitle="Fix failed" logs />);
    expect(description(container)).not.toBeNull();
    expect(screen.getByRole("button", { name: /Show details/ })).toBeInTheDocument();
  });

  it("shows a failure's message and the words after it", () => {
    const { container } = render(<OutcomeAlert result={{ success: false, message: "Could not reach the cluster.", logs: [] }} failedTitle="Fix failed" logs><span>Try again.</span></OutcomeAlert>);
    expect(description(container)).toHaveTextContent("Could not reach the cluster.Try again.");
  });
});
