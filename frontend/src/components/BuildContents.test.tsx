import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ComponentImagesTable } from "./BuildContents";
import type { RelatedImage } from "../types";

const SHA = "a3b6b581b465f97f8701f220efadcf8787879dde";

function image(gitURL: string): RelatedImage {
  return { name: "odh-dashboard", image: `quay.io/rhoai/odh-dashboard-rhel9@sha256:${"a".repeat(64)}`, category: "core", gitCommit: SHA, gitURL };
}

describe("ComponentImagesTable commit links", () => {
  it("links a commit of a github.com repository", () => {
    render(<ComponentImagesTable label="Components" images={[image("https://github.com/red-hat-data-services/odh-dashboard.git")]} labelsPending={false} />);
    expect(screen.getByRole("link", { name: `Commit ${SHA} on GitHub` })).toHaveAttribute(
      "href", `https://github.com/red-hat-data-services/odh-dashboard/commit/${SHA}`);
  });

  // The labels are the image's: whoever builds it chooses them.
  it.each(["javascript:alert(document.cookie)//", "http://github.com/a/b", "https://evil.example/a/b", "https://github.com/a/b/../../x"])(
    "shows the commit without a link for %s", (gitURL) => {
      render(<ComponentImagesTable label="Components" images={[image(gitURL)]} labelsPending={false} />);
      expect(screen.getByText(SHA.slice(0, 7))).toBeInTheDocument();
      expect(screen.queryByRole("link")).not.toBeInTheDocument();
    });
});
