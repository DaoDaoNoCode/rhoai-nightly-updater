import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";

// setup.ts fails every test that logs an error, React's act warning
// included; React sends that warning only while IS_REACT_ACT_ENVIRONMENT is
// true (react-dom-client's isConcurrentActEnvironment).
describe("the act environment", () => {
  it("is on, so a state update outside act is reported", async () => {
    expect((globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT).toBe(true);
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    let setCount: (n: number) => void = () => {};
    const Counter = () => {
      const [count, set] = useState(0);
      setCount = set;
      return <p>{count}</p>;
    };
    render(<Counter />);
    setCount(1); // as a fetch that answers after the test's assertions would
    expect(errors).toHaveBeenCalledWith(expect.stringContaining("not wrapped in act(...)"), "Counter");
    await act(async () => {});
    expect(screen.getByText("1")).toBeInTheDocument();
  });
});
