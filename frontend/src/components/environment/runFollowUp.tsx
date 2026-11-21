import React from "react";
import { Content } from "@patternfly/react-core";
import type { PlaygroundStatus, ResourcesStatus } from "../../types";
import { type Busy, type FeatureState, type MaaSOp, type ModelsOp, type NextStep, type PlaygroundOp, modelsState, playgroundState } from "./featureState";
import type { SampleOp } from "./samples";
import { mlflowOpenHref } from "./UseIt";
import type { GoalRun } from "./useGoalRun";
import type { RunFeature } from "./environmentTabs";

export interface RunFollowUpInput {
  run: GoalRun | null;
  runFeature: RunFeature;
  sampleRun: SampleOp | null;
  modelsRun: ModelsOp | null;
  playgroundRun: PlaygroundOp | null;
  maasRun: MaaSOp | null;
  status: ResourcesStatus | null;
  pg: PlaygroundStatus | null;
  busy: Busy;
  projectNames: string[];
  /** The features' states now. */
  mlflowNow: FeatureState;
  maasNow: FeatureState;
  pipelines: FeatureState;
}

/**
 * The run dialog's main action (open) and "what next" words, from the
 * feature the run set up. A sample run: after adding MLflow samples, open
 * the project's workspace.
 */
export function runFollowUp({ run, runFeature, sampleRun, modelsRun, playgroundRun, maasRun, status, pg, busy, projectNames, mlflowNow, maasNow, pipelines }: RunFollowUpInput): { open: NextStep | null; whatNext: React.ReactNode } {
  const mlflow = status?.mlflow;
  const sampleNext: NextStep | null = sampleRun?.feature === "mlflow" && sampleRun.action !== "remove" && mlflow?.uiRoute
    ? { kind: "open", label: `Open ${sampleRun.project} in MLflow`, href: mlflowOpenHref(status?.dashboardUrl, mlflow.uiRoute, sampleRun.project) }
    : null;
  const modelsNext = modelsRun?.project ? modelsState(status, modelsRun.project, projectNames, busy, modelsRun.model ?? undefined).next : null;
  const playgroundNext = playgroundRun ? playgroundState(status, pg, playgroundRun.project, busy).next : null;
  const maasNext = runFeature === "maas" && maasNow.next?.kind === "open" ? maasNow.next : null;
  const runNext: NextStep | null = run?.kind === "setup"
    ? (runFeature === "playground" ? playgroundNext : runFeature === "models" ? modelsNext : runFeature === "mlflow" ? mlflowNow.next : runFeature === "maas" ? maasNext : runFeature === "pipelines" ? pipelines.next : null)
    : runFeature === "samples" ? sampleNext : null;
  const whatNext = run?.kind === "setup" && runFeature === "maas"
    ? <Content component="p">{maasRun?.action === "publish"
      ? `On the MaaS tab, Use it mints an API key (shown once) and calls ${maasRun.project ? `${maasRun.model ?? "the model"} in ${maasRun.project}` : "the model"} with it. The dashboard's Models as a Service pages list it with its subscription.`
      : "On the MaaS tab, publish a model to offer it through MaaS: callers then use an API key instead of their OpenShift token, within the subscription's token limits."}</Content>
    : run?.kind === "setup" && runFeature === "playground"
    ? <Content component="p">{`Open the playground in ${playgroundRun?.project}, choose a model and ask it something. On the Playground tab, Use it also has a chat request to run from a terminal.`}</Content>
    : run?.kind === "setup" && runFeature === "models" && modelsRun?.action === "allow"
    ? <Content component="p">{`The models in ${modelsRun.allowProject} attach to the inference gateway within a few seconds, and the dashboard shows them as it shows any other model.`}</Content>
    : run?.kind === "setup" && runFeature === "models"
    ? <Content component="p">{modelsRun?.project
      ? `On the Models tab, Use it has the model's address and commands that call it with your OpenShift token. In the dashboard, ${modelsRun.project} lists the model, and Gen AI studio offers it in the playground.`
      : "Choose a project on the Models tab and set up a model there: it routes through this gateway."}</Content>
    : run?.kind === "setup" && (runFeature === "mlflow" || runFeature === "pipelines")
    ? <Content component="p">{runFeature === "mlflow"
      ? "Open MLflow workspaces to choose a project and see its experiments. On the MLflow tab, Sample data adds example experiments, traces and a model to a project in one click."
      : `${runNext?.kind === "open" ? runNext.label : "Open pipelines"} to import a pipeline and create a run. The Pipelines tab has a sample pipeline to check that runs work, with the commands that run it with your own token.`}</Content>
    : runFeature === "samples" && sampleRun && sampleRun.action !== "remove"
      ? <Content component="p">{`Open ${sampleRun.project} in MLflow to see the sample data; the dashboard shows it on the pages each sample names.`}</Content>
      : undefined;
  return { open: runNext, whatNext };
}
