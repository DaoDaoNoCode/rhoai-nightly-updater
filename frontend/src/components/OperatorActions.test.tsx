import React from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useOperatorActions, type OperatorRequest } from "./OperatorActions";
import { ReinstallPanel, isValidCustomImage } from "./ReinstallPanel";
import { UpdateConfirmModal } from "./UpdatePanel";
import { nightlyStatus, renderWithApp } from "../test/providers";
import { jsonResponse, sseEvent } from "../test/utils";

const IMAGE = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "b".repeat(64);

/** fetch stub: answers each operator stream POST with the next scripted response and records the bodies. */
function stubStreams(responses: Array<() => Response>) {
  const bodies: Array<Record<string, unknown> | null> = [];
  const urls: string[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/api/pageview") return jsonResponse({});
    urls.push(url);
    bodies.push(init?.body ? JSON.parse(String(init.body)) : null);
    const next = responses.shift();
    if (!next) throw new Error("unexpected request " + url);
    return next();
  }));
  return { bodies, urls };
}

const sse = (...events: string[]) => () => new Response(events.join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

const Harness: React.FC<{ request: OperatorRequest }> = ({ request }) => {
  const { runOperator, followUps } = useOperatorActions();
  return (
    <>
      <button onClick={() => runOperator(request)}>go</button>
      {followUps}
    </>
  );
};

describe("dashboard_dev_active (D1 guard)", () => {
  it("offers to revert Dashboard Dev and continue, and resends with revertDashboardDev", async () => {
    const refusal = "A Dashboard Dev session is active (dashboard-operator is paused). Update would keep the dashboard paused under the new operator, so it was not started and nothing was changed.";
    const { bodies, urls } = stubStreams([
      sse(sseEvent("validate_prerequisites", "failed", refusal, { errorCode: "dashboard_dev_active" }), sseEvent("operation_complete", "failed", refusal, { errorCode: "dashboard_dev_active" })),
      sse(sseEvent("operation_complete", "success", "RHOAI nightly update complete")),
    ]);
    renderWithApp(<Harness request={{ kind: "update", image: IMAGE }} />);
    fireEvent.click(screen.getByText("go"));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Revert Dashboard Dev before the update?")).toBeInTheDocument();
    expect(within(dialog).getByText(/nothing was changed/)).toBeInTheDocument();
    expect(bodies[0]).toEqual({ image: IMAGE });

    fireEvent.click(within(dialog).getByRole("button", { name: "Revert Dashboard Dev and continue" }));
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(urls[1]).toBe("/api/update/stream");
    expect(bodies[1]).toEqual({ image: IMAGE, revertDashboardDev: true });
  });

  it("Cancel sends nothing more", async () => {
    const { bodies } = stubStreams([sse(sseEvent("operation_complete", "failed", "active", { errorCode: "dashboard_dev_active" }))]);
    renderWithApp(<Harness request={{ kind: "refresh" }} />);
    fireEvent.click(screen.getByText("go"));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Revert Dashboard Dev before the re-deploy?")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(bodies).toEqual([null]);
  });

  it("does not ask again when the revert itself failed", async () => {
    stubStreams([sse(sseEvent("operation_complete", "failed", "Could not end the Dashboard Dev session", { errorCode: "dashboard_dev_active" }))]);
    const AlreadyReverting: React.FC = () => {
      const { runOperator, followUps } = useOperatorActions();
      return <><button onClick={() => runOperator({ kind: "update", image: IMAGE }, { revertDashboardDev: true })}>go</button>{followUps}</>;
    };
    renderWithApp(<AlreadyReverting />);
    fireEvent.click(screen.getByText("go"));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("the Update dialog asks proactively when a session is known to be active", async () => {
    const onConfirm = vi.fn();
    renderWithApp(
      <UpdateConfirmModal target={{ image: IMAGE, tag: "rhoai-3.6", digest: "sha256:" + "b".repeat(64) }} status={nightlyStatus()} onConfirm={onConfirm} onClose={vi.fn()} />,
      { dashboard: async () => ({ override: { active: true, operatorPaused: true, sessionRecorded: true, mode: "pr", prNumber: 7, startedBy: "bob", stale: false, dashboardDeleting: false } }) as never },
    );
    const button = await screen.findByRole("button", { name: "Revert Dashboard Dev and update" });
    expect(screen.getByText(/PR #7 by bob/)).toBeInTheDocument();
    fireEvent.click(button);
    expect(onConfirm).toHaveBeenCalledWith({ revertDashboardDev: true });
  });
});

describe("Update confirmation (A07-1, A07-7)", () => {
  it("names the target build and lists the backend's real steps with one primary button", async () => {
    renderWithApp(<UpdateConfirmModal target={{ image: IMAGE, tag: "rhoai-3.6", digest: "sha256:" + "b".repeat(64), buildDate: "2026-10-05T21:47:53Z" }} status={nightlyStatus()} onConfirm={vi.fn()} onClose={vi.fn()} />);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("bbbbbbbbbbbb")).toBeInTheDocument();
    const steps = within(dialog).getAllByRole("listitem").map((li) => li.textContent?.split(":")[0]);
    expect(steps).toEqual([
      "Check prerequisites", "Save snapshot", "Replace the nightly catalog", "Wait for the catalog", "Detect the channel",
      "Remove the old operator version", "Create the Subscription", "Wait for the operator install",
    ]);
    expect(within(dialog).getAllByRole("button").filter((b) => b.className.includes("pf-m-primary"))).toHaveLength(1);
  });

  it("says Update is the usual fix when the CSV is Failed (A07-5)", async () => {
    renderWithApp(<UpdateConfirmModal target={{ image: IMAGE }} status={nightlyStatus({ csv: { name: "rhods-operator.3.6.0", version: "3.6.0", phase: "Failed" } })} onConfirm={vi.fn()} onClose={vi.fn()} />);
    expect(await screen.findByText("The installed operator is Failed.")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Update" })).not.toHaveAttribute("aria-disabled", "true"));
  });
});

describe("cluster_busy (A07-3)", () => {
  it("tells the user their request did not run and who is running what", async () => {
    stubStreams([() => jsonResponse({ error: "alice is running \"Update to nightly\". Wait for it to finish.", errorCode: "cluster_busy", operation: { id: "x", type: "update", label: "Update to nightly", user: "alice", startedAt: new Date().toISOString() } }, 409)]);
    renderWithApp(<Harness request={{ kind: "update", image: IMAGE }} />);
    fireEvent.click(screen.getByText("go"));
    expect(await screen.findByText("Cluster busy: your request did not run")).toBeInTheDocument();
    expect(screen.getAllByText(/alice is running "Update to nightly"/).length).toBeGreaterThan(0);
  });
});

describe("Reinstall downgrades (A07-6, §8 #8)", () => {
  const status = nightlyStatus();
  const renderPanel = () => {
    const runOperator = vi.fn(() => true);
    renderWithApp(<ReinstallPanel status={status} nightlyTags={[]} tagsLoading={false} prerequisitesMet runOperator={runOperator} />);
    return runOperator;
  };

  it("preselects no target and marks GA 3.5.1 as a downgrade from 3.6.0", () => {
    renderPanel();
    for (const radio of screen.getAllByRole("radio")) expect(radio).not.toBeChecked();
    expect(screen.getByText("Downgrade")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Reinstall operator/ })).toHaveAttribute("aria-disabled", "true");
  });

  it("requires the typed confirmation and an explicit downgrade acknowledgement before sending allowDowngrade", async () => {
    const runOperator = renderPanel();
    await waitFor(() => expect(screen.getByRole("button", { name: /Reinstall operator/ })).toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(screen.getByLabelText(/Latest stable \(GA\)/));
    await waitFor(() => expect(screen.getByRole("button", { name: /Reinstall operator/ })).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(screen.getByRole("button", { name: /Reinstall operator/ }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Reinstall RHOAI 3.5.1 (downgrade from 3.6.0)?")).toBeInTheDocument();
    const confirm = within(dialog).getByRole("button", { name: "Reinstall older version" });
    fireEvent.change(within(dialog).getByLabelText(/Type "reinstall" to confirm/), { target: { value: "reinstall" } });
    expect(confirm).toBeDisabled();
    fireEvent.click(within(dialog).getByLabelText(/I understand and want to install the older version/));
    expect(confirm).not.toBeDisabled();
    fireEvent.click(confirm);
    expect(runOperator).toHaveBeenCalledWith(
      expect.objectContaining({ kind: "reinstall", targetType: "stable" }),
      { allowDowngrade: true },
    );
  });

  it("asks for confirmation when the backend finds a downgrade the page could not see", async () => {
    const message = "The target rhods-operator.3.6.0-ea.1 is older than the installed rhods-operator.3.6.0. Nothing was changed. Confirm the downgrade to continue.";
    const { bodies } = stubStreams([
      sse(sseEvent("operation_complete", "failed", message, { errorCode: "downgrade_requires_confirmation" })),
      sse(sseEvent("operation_complete", "success", "Reinstall to nightly complete")),
    ]);
    renderWithApp(<Harness request={{ kind: "reinstall", targetType: "custom", image: IMAGE, detail: IMAGE }} />);
    fireEvent.click(screen.getByText("go"));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(message)).toBeInTheDocument();
    const confirm = within(dialog).getByRole("button", { name: "Reinstall the older version" });
    expect(confirm).toBeDisabled();
    fireEvent.click(within(dialog).getByRole("checkbox"));
    fireEvent.click(confirm);
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[0]).toEqual({ targetType: "custom", image: IMAGE });
    expect(bodies[1]).toEqual({ targetType: "custom", image: IMAGE, allowDowngrade: true });
  });

  it("accepts only RHOAI FBC builds for custom images (backend allowlist)", () => {
    expect(isValidCustomImage("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5")).toBe(true);
    expect(isValidCustomImage(IMAGE)).toBe(true);
    expect(isValidCustomImage("quay.io/rhoai/rhoai-fbc-fragment")).toBe(false);
    expect(isValidCustomImage("quay.io/attacker/rhoai-fbc-fragment:x")).toBe(false);
  });
});
