import React, { useEffect, useState } from "react";
import {
  ActionList,
  ActionListItem,
  Alert,
  AlertActionLink,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Checkbox,
  Content,
  Flex,
  FlexItem,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import { useNavigate } from "react-router-dom";
import { resumeRecipe, stopRecipe, toApiError, type ApiError } from "../../services/api";
import type { RecipeRun, RecipeRunStatus, RecipeRunStep } from "../../types";
import { ConfirmActionModal } from "../ConfirmActionModal";
import { OutcomeAlert } from "../OutcomeAlert";
import { RelativeTime } from "../RelativeTime";
import { StatusLabel, type StatusKind } from "../StatusLabel";
import { TooltipButton } from "../TooltipButton";
import { StepProgress, type StepState } from "../environment/StepProgress";
import { shortDigest } from "../../build";
import { formatClock, resumeStep, usuallySentence } from "./recipeForm";
import { RecipeProblems, refusalOf } from "./RecipeReviewModal";

/** Ticks once a second while `active`, so elapsed times stay current. */
function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [active]);
  return now;
}

/** The sign-in codes a run stops with before a step (pkg/api sessionProblem). */
const SIGN_IN_CODES = new Set(["sign_in_again", "session_expired"]);

/** What the run is now, for its label and its alert. */
export type RunPhase = "running" | "stopping" | "interrupted" | "succeeded" | "failed" | "stopped" | "sign-in";

export function runPhase(status: RecipeRunStatus, run: RecipeRun): RunPhase {
  if (status.running) return status.stopRequested ? "stopping" : "running";
  if (status.interrupted) return "interrupted";
  if (run.state === "running") return "interrupted";
  if (run.state === "succeeded") return "succeeded";
  if (run.state === "failed") return "failed";
  return SIGN_IN_CODES.has(run.errorCode ?? "") ? "sign-in" : "stopped";
}

const LABELS: Record<RunPhase, [StatusKind, string]> = {
  running: ["progress", "Running"],
  stopping: ["progress", "Stopping after this step"],
  interrupted: ["warning", "Interrupted"],
  succeeded: ["success", "Set up"],
  failed: ["danger", "Failed"],
  stopped: ["warning", "Stopped"],
  "sign-in": ["warning", "Stopped: sign in again"],
};

/** "4 of 8" for a step. */
function stepNumber(run: RecipeRun, step: RecipeRunStep): string {
  return `${run.steps.indexOf(step) + 1} of ${run.steps.length}`;
}

/**
 * The last recipe run: its steps as they ran, the running step's latest
 * progress and elapsed time, and what to do next. Running: Stop after this
 * step. Ended: the outcome, Resume (it continues where the run stopped;
 * what is set up stays) and Clean up.
 */
export const RecipeRunCard: React.FC<{
  status: RecipeRunStatus;
  run: RecipeRun;
  /** Why a change cannot start now (Resume, Clean up), or null. */
  blocker: string | null;
  /** Why this user cannot change the cluster at all (Stop), or null. */
  permissionReason: string | null;
  onChanged: () => void;
  onClusterBusy: (res: { errorCode?: string }) => void;
  onCleanup: (() => void) | null;
}> = ({ status, run, blocker, permissionReason, onChanged, onClusterBusy, onCleanup }) => {
  const navigate = useNavigate();
  const phase = runPhase(status, run);
  const live = phase === "running" || phase === "stopping";
  const now = useNow(live);
  const [stopOpen, setStopOpen] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [stopError, setStopError] = useState<ApiError | null>(null);
  const [resumeOpen, setResumeOpen] = useState(false);
  const [resuming, setResuming] = useState(false);
  const [resumeError, setResumeError] = useState<ApiError | null>(null);
  const [reresolve, setReresolve] = useState(false);

  const current = run.steps.find((s) => s.status === "running") ?? null;
  const failed = run.steps.find((s) => s.status === "failed") ?? null;
  const next = resumeStep(run);
  const name = run.recipe.metadata.name;

  const states: Record<string, StepState> = {};
  for (const s of run.steps) {
    if (s.status === "pending") continue;
    let message = s.message ?? "";
    if (s.status === "running" && live) {
      const started = s.startedAt ? Date.parse(s.startedAt) : NaN;
      const progress = status.step?.message || "Starting...";
      // "Waiting for SeaweedFS (1/1 pods starting) (1:12). Usually 1 minute, up to 5 minutes."
      const shown = Number.isFinite(started) ? `${progress} (${formatClock(now - started)})` : progress;
      message = `${/[.!?]$/.test(shown) ? shown : `${shown}.`} ${usuallySentence(s.typicalSeconds, s.maxSeconds)}`;
    } else if (s.status === "running") {
      // An interrupted run's record: the step that was running when its updater pod stopped.
      message = "Was running when the updater stopped.";
    }
    // A step found already set up is set up: drawn as done, its message says so.
    states[s.id] = { status: s.status === "running" && !live ? "failed" : s.status === "skipped" ? "success" : s.status, message };
  }

  const stop = async () => {
    setStopping(true);
    setStopError(null);
    try {
      await stopRecipe();
      setStopOpen(false);
      onChanged();
    } catch (e) {
      setStopError(toApiError(e));
    } finally {
      setStopping(false);
    }
  };

  const resume = async () => {
    setResuming(true);
    setResumeError(null);
    try {
      await resumeRecipe(run.id, reresolve);
      setResumeOpen(false);
      onChanged();
    } catch (e) {
      const err = toApiError(e);
      onClusterBusy(err);
      setResumeError(err);
    } finally {
      setResuming(false);
    }
  };

  const [labelKind, labelText] = LABELS[phase];
  const runStarted = Date.parse(run.resumedAt || run.startedAt);
  const stopReason = phase === "stopping" ? "Stop was asked for: the run stops after the current step." : status.remote
    ? `Updater pod ${status.pod || "(unknown)"} runs it; only that pod can stop it, and it does when it shuts down.`
    : permissionReason;
  const resumeButton = next && (
    <TooltipButton variant="primary" onClick={() => { setResumeError(null); setReresolve(false); setResumeOpen(true); }} disabledReason={blocker}>
      Resume
    </TooltipButton>
  );
  const continuesAt = next ? `Resume continues at step ${stepNumber(run, next)}, ${next.title}; what is set up stays.` : "";

  let outcome: React.ReactNode = null;
  switch (phase) {
    case "succeeded":
      outcome = (
        <Alert variant="success" isInline component="p" isLiveRegion title={run.stopReason || `Every step of ${name} is set up.`}
          actionLinks={<AlertActionLink onClick={() => navigate(run.project ? `/test-resources?project=${encodeURIComponent(run.project.name)}` : "/test-resources")}>Open Environment</AlertActionLink>}>
          {run.project ? `Project ${run.project.name} is ready to use.` : "What it set up is on the Environment page."}
        </Alert>
      );
      break;
    case "failed":
      // The failed step's own message: the title already names the step.
      outcome = (
        <OutcomeAlert result={{ success: false, message: failed?.message || run.stopReason || "The run failed.", errorCode: run.errorCode, logs: null }}
          failedTitle={failed ? `Step ${stepNumber(run, failed)} failed: ${failed.title}` : "The run failed"} />
      );
      break;
    case "sign-in":
      outcome = (
        <Alert variant="warning" isInline component="p" isLiveRegion title="Sign in again, then Resume"
          // The user menu's Log out: oauth-proxy's sign_out clears its session cookie (App).
          actionLinks={<AlertActionLink component="a" href="/oauth/sign_out">Log out</AlertActionLink>}>
          {/* The backend's reason names the step and says what to do ("Stopped before S3 storage: Your sign-in expires ..., then Resume."). */}
          {run.stopReason || <>{next ? `The run stopped before step ${stepNumber(run, next)}, ${next.title}: ` : "The run stopped: "}
            your sign-in ended or expires before that step could finish. Nothing of that step started. Log out and log in again, then Resume.</>}
        </Alert>
      );
      break;
    case "stopped":
      outcome = (
        <Alert variant="info" isInline component="p" isLiveRegion title={run.errorCode === "restart" ? "Stopped: the updater was restarting" : "Stopped"}>
          {run.stopReason}
        </Alert>
      );
      break;
    case "interrupted":
      outcome = (
        <Alert variant="warning" isInline component="p" isLiveRegion title="The run was interrupted">
          {run.pod ? `The updater pod ${run.pod}` : "The updater"} stopped{current ? ` during step ${stepNumber(run, current)}, ${current.title}` : ""}, before the run ended
          (a crash, a forced restart or a lost node). Nothing resumes by itself. Resume runs the steps again: each checks the cluster
          first, so what is set up stays.
        </Alert>
      );
      break;
    case "running":
    case "stopping":
      // The steps above show it; the outcome comes when it ends.
      break;
  }

  return (
    <Card>
      <CardHeader>
        <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
          <FlexItem><CardTitle><Title headingLevel="h2" size="xl">{live ? "Running" : "Last run"}: recipe {name}</Title></CardTitle></FlexItem>
          <FlexItem><StatusLabel status={labelKind}>{labelText}</StatusLabel></FlexItem>
        </Flex>
      </CardHeader>
      <CardBody>
        <Stack hasGutter>
          <StackItem>
            <Content component="p" className="pf-v6-u-text-color-subtle">
              Started by {run.user || "someone"} <RelativeTime date={run.startedAt} size="inherit" />
              {run.resumedAt && <>, resumed <RelativeTime date={run.resumedAt} size="inherit" /></>}
              {run.finishedAt && !live && <>, ended <RelativeTime date={run.finishedAt} size="inherit" /></>}
              {run.project && <>. Project {run.project.name}{run.project.owned ? ", created by the recipe" : ""}</>}
              {run.pinned && <>. Build {run.pinned.tag ? `${run.pinned.tag} ` : ""}<code>{shortDigest(run.pinned.digest)}</code></>}.
            </Content>
          </StackItem>
          {live && (
            <StackItem>
              <Content component="p">
                {status.remote ? `Running on updater pod ${status.pod || "(unknown)"}` : "Running"} for {Number.isFinite(runStarted) ? formatClock(now - runStarted) : "a while"}
                {current ? `, step ${stepNumber(run, current)}` : ""}. The steps run on the updater: you can leave this page or close the browser and
                come back. Logging out does not stop the run; Stop after this step does. Teammates cannot change this cluster until it ends.
              </Content>
            </StackItem>
          )}
          <StackItem>
            <StepProgress idPrefix="recipe-run" aria-label={`Recipe ${name}: steps`} states={states} running={live}
              steps={run.steps.map((s) => ({ id: s.id, title: s.title, pending: usuallySentence(s.typicalSeconds, s.maxSeconds) }))}
              shownElsewhere={phase === "failed" ? failed?.message || run.stopReason : undefined} />
          </StackItem>
          {outcome && <StackItem>{outcome}</StackItem>}
          {!live && next && phase !== "succeeded" && <StackItem><Content component="p">{continuesAt}</Content></StackItem>}
          {(live || resumeButton || onCleanup) && (
            <StackItem>
              <ActionList>
                {live && (
                  <ActionListItem>
                    <TooltipButton variant="secondary" onClick={() => { setStopError(null); setStopOpen(true); }} disabledReason={stopReason}>
                      Stop after this step
                    </TooltipButton>
                  </ActionListItem>
                )}
                {!live && resumeButton && phase !== "succeeded" && <ActionListItem>{resumeButton}</ActionListItem>}
                {!live && onCleanup && (
                  <ActionListItem>
                    <TooltipButton variant={phase === "succeeded" ? "secondary" : "link"} onClick={onCleanup} disabledReason={blocker}>
                      Clean up what the recipe set up...
                    </TooltipButton>
                  </ActionListItem>
                )}
              </ActionList>
            </StackItem>
          )}
        </Stack>
      </CardBody>
      <ConfirmActionModal
        isOpen={stopOpen}
        title="Stop after this step?"
        changes={[]}
        summary={
          <Content component="p">
            {current ? `Step ${stepNumber(run, current)}, ${current.title}, finishes` : "The running step finishes"}, then the run stops and the cluster
            is free for others. What is set up stays; Resume continues from there.
          </Content>
        }
        confirmLabel="Stop after this step"
        isLoading={stopping}
        onConfirm={() => void stop()}
        onCancel={() => setStopOpen(false)}
      >
        {stopError && (
          <Alert variant={stopError.errorCode === "not_running" ? "info" : "danger"} isInline component="p"
            title={stopError.errorCode === "not_running" ? "The run is not running in this updater pod" : "Could not ask the run to stop"}>
            {stopError.message}
          </Alert>
        )}
      </ConfirmActionModal>
      <ConfirmActionModal
        isOpen={resumeOpen}
        title={`Resume recipe ${name}?`}
        changes={[]}
        summary={
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                {next ? `It continues at step ${stepNumber(run, next)}, ${next.title}. ` : ""}Each step checks the cluster first, so what is set
                up stays and is not done again.
              </Content>
            </StackItem>
            <StackItem><Content component="p">Teammates cannot change this cluster until the run ends.</Content></StackItem>
          </Stack>
        }
        choice={run.pinned && (run.recipe.spec.rhoai.build === "latest" || !!run.recipe.spec.rhoai.build.tag) ? (
          <Checkbox id="recipe-reresolve" label="Look up the newest build again" isChecked={reresolve} onChange={(_e, v) => setReresolve(v)}
            description={`Off: the run keeps the build it looked up when it started, ${run.pinned?.tag ? `${run.pinned.tag} ` : ""}${shortDigest(run.pinned?.digest) ?? ""}.`} />
        ) : undefined}
        confirmLabel="Resume"
        isLoading={resuming}
        onConfirm={() => void resume()}
        onCancel={() => setResumeOpen(false)}
      >
        {resumeError && (refusalOf(resumeError).problems.length > 0 ? <RecipeProblems problems={refusalOf(resumeError).problems} /> : (
          <Alert variant={resumeError.errorCode === "cluster_busy" ? "warning" : "danger"} isInline component="p"
            title={resumeError.errorCode === "cluster_busy" ? "Another operation is changing this cluster" : "The run did not resume"}>
            {resumeError.message}
          </Alert>
        ))}
      </ConfirmActionModal>
    </Card>
  );
};
