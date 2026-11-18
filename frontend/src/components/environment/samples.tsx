import React from "react";
import { Content } from "@patternfly/react-core";
import type { MLflowSampleAction, MLflowSampleId, SampleStatus } from "../../types";
import type { ConfirmSpec, Pending } from "./resourceRow";
import type { EnvStatus } from "./resourceStatus";
import type { GoalRunStepInfo } from "./useGoalRun";

/**
 * One-click sample data (pkg/cluster samples*.go): MLflow samples in a
 * project's workspace, and a sample pipeline run on the project's pipeline
 * server. The backend decides each sample's state and the actions it
 * allows; this module has the words.
 */

/** The MLflow samples in their add order (pkg/cluster mlflowSampleIDs); removal runs reversed. */
export const MLFLOW_SAMPLE_IDS: MLflowSampleId[] = ["genai-demo", "training", "mcp-server", "skill"];

interface SampleInfo {
  name: string;
  /** The name inside a sentence ("Add the MCP server"): written out, because lowercasing breaks acronyms. */
  inSentence: string;
  /** What you can see after adding it, and where, in one line. */
  value: string;
  /** Add's outcome, for the confirmation ("In p: <adds>, ..."). */
  adds: string;
  /** What Add creates, by name: the confirmation's Details. */
  creates: React.ReactNode;
}

/** The names are pkg/cluster mlflowSampleNames; the objects are the samples' own (samples_*.go). */
export const SAMPLE_INFO: Record<MLflowSampleId, SampleInfo> = {
  "genai-demo": {
    name: "Agent and prompt demo",
    inSentence: "agent and prompt demo",
    value: "Agent traces, prompts and evaluations of a sample agent. In the dashboard: Agent traces, Prompt management.",
    adds: "the agent and prompt demo (about 30 seconds)",
    creates: <>Experiment <code>MLflow Demo</code>, with the prompts, traces, evaluation datasets and runs that MLflow&apos;s own demo generator makes.</>,
  },
  training: {
    name: "Training experiment and registered model",
    inSentence: "training experiment and registered model",
    value: "Three training runs with metric charts, and a registered model. In the dashboard: Develop and train > Experiments.",
    adds: "a training experiment with a registered model",
    creates: <>Experiment <code>nightly-sample-training</code> with three runs (parameters and step metrics), and registered model <code>nightly-sample-classifier</code> with two versions and the alias <code>champion</code>. Metadata only: the model has no files to download or serve.</>,
  },
  "mcp-server": {
    name: "MCP server",
    inSentence: "MCP server",
    value: "A public MCP server (DeepWiki) to try in the Gen AI playground. In the dashboard: AI hub > MCP servers.",
    adds: "an MCP server entry",
    creates: <>MCP server <code>com.deepwiki/rhoai-nightly-sample</code>, active, with the endpoint <code>https://mcp.deepwiki.com/mcp</code>. The playground needs internet access to call it.</>,
  },
  skill: {
    name: "Agent skill",
    inSentence: "agent skill",
    value: "An agent skill (mcp-builder, from anthropics/skills). In the dashboard: AI hub > Skills.",
    adds: "an agent skill entry",
    creates: <>Skill <code>nightly-sample-mcp-builder</code>: a registration of <code>skills/mcp-builder</code> in <code>https://github.com/anthropics/skills</code> at a fixed commit. MLflow stores the reference only; it downloads nothing.</>,
  },
};

const nameInSentence = (id: MLflowSampleId) => SAMPLE_INFO[id].inSentence;

/** A sample operation this page runs now. */
export interface SampleOp {
  feature: "mlflow";
  project: string;
  action: MLflowSampleAction;
  /** The MLflow samples it changes. */
  samples: MLflowSampleId[];
}

/** The samples Add all adds and Remove all removes: those whose state allows it. */
export const samplesFor = (samples: SampleStatus[], action: "add" | "remove") =>
  MLFLOW_SAMPLE_IDS.filter((id) => samples.find((s) => s.id === id)?.actions.includes(action));

/** Why Add all or Remove all has nothing to do, or null. */
export const bulkEmptyReason = (action: "add" | "remove") =>
  action === "add" ? "Every sample that can be added is added already." : "Nothing here that this tool can remove.";

export const READ_ONLY_SAMPLES = "Read-only access: adding or removing sample data needs a cluster-admin. Ask one to do it for you.";

const BUSY_WORD: Record<SampleOp["action"], { status: EnvStatus; text: string }> = {
  add: { status: "setting-up", text: "Adding" },
  regenerate: { status: "setting-up", text: "Regenerating" },
  remove: { status: "deleting", text: "Removing" },
};

/**
 * A sample's state as one label of the page's vocabulary, or null for a
 * sample someone else created (that is ownership, shown apart).
 */
export function sampleLabel(s: SampleStatus, op: SampleOp | null): { status: EnvStatus; text: string } | null {
  if (op && op.samples.includes(s.id)) return BUSY_WORD[op.action];
  switch (s.state) {
    case "absent": return { status: "not-set-up", text: "Not added" };
    case "present": return { status: "ready", text: "Added" };
    case "partial": return { status: "needs-attention", text: "Partly added" };
    case "removed": return { status: "not-set-up", text: "Removed" };
    case "foreign": return null;
    case "unsupported": return { status: "unknown", text: "Not available" };
    default: return { status: "unknown", text: "Unknown" };
  }
}

/** What a sample's state means and what to do, in words, or null when the label says it all. */
export function sampleNote(s: SampleStatus): string | null {
  const missing = s.missing?.length ? `Missing: ${s.missing.join(", ")}.` : "";
  switch (s.state) {
    case "partial": {
      const fix = s.actions.includes("regenerate") ? "Regenerate completes it." : "Add completes it; Remove deletes what is there.";
      return [s.message || "Only part of it is there.", missing, s.message ? "" : fix].filter(Boolean).join(" ");
    }
    case "foreign":
      return s.message || "Something of this sample's name exists that this tool did not create, so it is left alone.";
    case "unsupported":
      return `${s.message || "This MLflow does not support it."} This sample cannot be added here.`;
    case "unknown":
      return s.message || "Its state could not be read.";
    default:
      return s.message || null;
  }
}

/** The steps an MLflow samples operation streams: the check, then one per sample in the server's order. */
export function mlflowSampleSteps(action: MLflowSampleAction, samples: MLflowSampleId[]): GoalRunStepInfo[] {
  const order = action === "remove" ? [...MLFLOW_SAMPLE_IDS].reverse() : MLFLOW_SAMPLE_IDS;
  const verb = action === "add" ? "Add" : action === "remove" ? "Remove" : "Regenerate";
  return [
    { id: "check", title: "Check MLflow" },
    ...order.filter((id) => samples.includes(id)).map((id) => ({ id, title: `${verb} the ${nameInSentence(id)}` })),
  ];
}

/**
 * "a, b and c". A comma goes before the last "and" too when an item has its
 * own "and", so "the training experiment and registered model, and the MCP
 * server" reads as two items.
 */
export const listWords = (items: string[]) => {
  if (items.length < 2) return items[0] ?? "";
  const last = items.some((i) => i.includes(" and ")) ? ", and " : " and ";
  return `${items.slice(0, -1).join(", ")}${last}${items[items.length - 1]}`;
};

/** The confirmation of a sample change, or null for another kind of change. */
export function samplesConfirm(pending: Pending): ConfirmSpec | null {
  switch (pending.kind) {
    case "mlflow-samples": {
      const { project, action, samples } = pending;
      const ids = MLFLOW_SAMPLE_IDS.filter((id) => samples.includes(id));
      const names = ids.map(nameInSentence);
      if (action === "add") {
        const many = ids.length > 1;
        return {
          title: many ? `Add sample data to ${project}?` : `Add the ${names[0]} to ${project}?`, confirm: "Add",
          runTitle: many ? `Add sample data to ${project}` : `Add the ${names[0]} to ${project}`,
          summary: <Content component="p">In {project}&apos;s MLflow workspace: {listWords(ids.map((id) => SAMPLE_INFO[id].adds))}. Everything is tagged as sample data of this tool; anyone with access to {project} sees it.</Content>,
          changes: ids.map((id) => SAMPLE_INFO[id].creates),
        };
      }
      if (action === "regenerate") {
        return {
          title: `Regenerate the agent and prompt demo in ${project}?`, confirm: "Regenerate", runTitle: `Regenerate the agent and prompt demo in ${project}`,
          summary: <Content component="p">MLflow generates its demo in {project} again, replacing the demo&apos;s prompts, traces and evaluations. Use it when the demo is incomplete. About 30 seconds.</Content>,
          changes: [<>Experiment <code>MLflow Demo</code>: its demo version tags (<code>mlflow.demo.version.*</code>) are reset, then MLflow&apos;s demo generator runs; it deletes and regenerates each part of the demo.</>],
        };
      }
      return {
        title: ids.length > 1 ? `Remove the sample data from ${project}?` : `Remove the ${names[0]} from ${project}?`, confirm: "Remove", danger: true,
        runTitle: ids.length > 1 ? `Remove the sample data from ${project}` : `Remove the ${names[0]} from ${project}`,
        summary: <Content component="p">Deletes {listWords(names.map((n) => `the ${n}`))} from {project}: only what this tool added, found by its tag. The agent and prompt demo stays; remove it in the MLflow UI.</Content>,
        changes: ids.map((id) => {
          switch (id) {
            case "training": return <>Registered model <code>nightly-sample-classifier</code> is deleted; experiment <code>nightly-sample-training</code> moves to MLflow&apos;s deleted experiments (Add restores it).</>;
            case "mcp-server": return <>MCP server <code>com.deepwiki/rhoai-nightly-sample</code>: its version is deprecated, then the server is deleted.</>;
            default: return <>Skill <code>nightly-sample-mcp-builder</code> is deleted.</>;
          }
        }),
      };
    }
    default:
      return null;
  }
}
