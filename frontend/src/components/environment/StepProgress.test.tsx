import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { StepProgress } from "./StepProgress";

const steps = [{ id: "check", title: "Check" }, { id: "model", title: "Delete the model in dev" }];

describe("StepProgress", () => {
  it("leaves out a step's message when the outcome alert says the same (live: a delete said it twice)", () => {
    const done = "Model qwen25-05b-instruct deleted from 'dev'. The project stays.";
    render(<StepProgress idPrefix="t" aria-label="progress" running={false} shownElsewhere={done}
      steps={steps} states={{ check: { status: "success", message: "Ready to delete." }, model: { status: "success", message: done } }} />);
    expect(screen.getByText("Ready to delete.")).toBeInTheDocument();
    expect(screen.queryByText(done)).toBeNull();
  });

  it("keeps a running step's message even when it matches", () => {
    render(<StepProgress idPrefix="t" aria-label="progress" running shownElsewhere="Deleting"
      steps={steps} states={{ model: { status: "running", message: "Deleting" } }} />);
    expect(screen.getByText("Deleting")).toBeInTheDocument();
  });
});
