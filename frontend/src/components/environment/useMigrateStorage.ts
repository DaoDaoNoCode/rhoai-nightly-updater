import { useCallback, useEffect, useRef, useState } from "react";
import { streamMigrateStorage, streamSecureStorage, trackFeature, type MigrateStorageOptions } from "../../services/api";
import type { UpdateStep } from "../../types";

/** Migrate storage, the removal of the old storage, or Secure S3 storage (the same kind of streamed run). */
export type MigrateKind = "migrate" | "remove" | "secure";

/** The steps each run streams, in order (pkg/cluster MigrateS3Storage). */
export const MIGRATE_STEPS: Record<MigrateKind, { id: string; title: string }[]> = {
  migrate: [
    { id: "check", title: "Check the old storage" },
    { id: "storage", title: "Set up the new storage with new keys" },
    { id: "copy", title: "Copy every object and check it byte for byte" },
    { id: "repoint", title: "Move the pipeline servers this tool created" },
  ],
  remove: [
    { id: "check", title: "Check that nothing of this tool uses the old storage" },
    { id: "copy", title: "Copy and check objects written since" },
    { id: "remove", title: "Delete the old storage" },
    { id: "namespace", title: "Namespace minio" },
  ],
  // Then one step per recorded pipeline server (MigrateRun.extraSteps).
  secure: [
    { id: "check", title: "Check the S3 storage and its pipeline servers" },
    { id: "rollout", title: "Roll out a new admin key and turn on per-server keys" },
  ],
};

/** Secure S3 storage's step for one pipeline server (pkg/cluster SecureServerStep). */
export const secureServerStep = (name: string) => ({ id: `server:${name}`, title: `Give ${name} a key of its own` });

/** All steps of a run: its fixed ones, then those it adds (one per pipeline server for Secure S3 storage). */
export const runSteps = (run: MigrateRun) => [...MIGRATE_STEPS[run.kind], ...run.extraSteps];

export interface MigrateStepState {
  status: UpdateStep["status"];
  message: string;
}

/** Each run's dialog title. */
export const MIGRATE_TITLES: Record<MigrateKind, string> = { migrate: "Migrate storage", remove: "Delete the old storage", secure: "Secure S3 storage" };

/** What a Secure S3 storage run also does: the leftover keys its confirmation listed. */
export interface SecureOptions {
  removeLeftoverKeys?: string[];
}

export interface MigrateRun {
  kind: MigrateKind;
  /** When it started (epoch ms). */
  startedAt: number;
  steps: Record<string, MigrateStepState>;
  /** Steps besides the kind's fixed ones, in order: known at the start, or added as the server streams them. */
  extraSteps: { id: string; title: string }[];
  running: boolean;
  /** The outcome, once the run ended (or the connection to it was lost). */
  result: { success: boolean; message: string; errorCode?: string } | null;
}

/** The step a run is on now, for the alert shown while its dialog is closed. */
export function migrateStepNow(run: MigrateRun): { title: string; index: number; count: number } | null {
  const steps = runSteps(run);
  const i = steps.findIndex((s) => run.steps[s.id]?.status === "running");
  return i < 0 ? null : { title: steps[i].title, index: i + 1, count: steps.length };
}

/**
 * Runs Migrate storage (or the removal of the old storage) and keeps its
 * progress while the dialog is closed: the operation runs on the server
 * either way. onFinished runs when it ends, to reload the status.
 */
export function useMigrateStorage(onFinished: () => void) {
  const [run, setRun] = useState<MigrateRun | null>(null);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);

  const start = useCallback((kind: MigrateKind, options: MigrateStorageOptions & SecureOptions, extraSteps: { id: string; title: string }[] = []) => {
    trackFeature(kind === "migrate" ? "migrate-storage" : kind === "remove" ? "remove-old-storage" : "secure-minio");
    setRun({ kind, startedAt: Date.now(), steps: {}, extraSteps, running: true, result: null });
    let finalMessage = "";
    // A success may carry a code too ("partial": secured, some servers left).
    let finalCode: string | undefined;
    const finish = (success: boolean, message: string, errorCode?: string) => {
      controller.current = null;
      setRun((r) => (r ? { ...r, running: false, result: { success, message, errorCode } } : r));
      onFinished();
    };
    const onStep = (step: UpdateStep) => {
      if (step.step === "operation_complete") {
        finalMessage = step.message;
        finalCode = step.errorCode;
        return;
      }
      setRun((r) => {
        if (!r) return r;
        // A step the run did not list (a pipeline server set up meanwhile) is added in the order it comes.
        const known = runSteps(r).some((s) => s.id === step.step);
        const extra = known ? r.extraSteps : [...r.extraSteps, step.step.startsWith("server:") ? secureServerStep(step.step.slice(7)) : { id: step.step, title: step.step }];
        return { ...r, extraSteps: extra, steps: { ...r.steps, [step.step]: { status: step.status, message: step.message } } };
      });
    };
    const onDone = (success: boolean, error?: string, apiError?: { errorCode?: string }) => finish(success, success ? finalMessage : error || finalMessage || "The operation failed.", success ? finalCode : apiError?.errorCode);
    const onDrop = () => finish(false, "The connection to the server was lost. The operation continues on the server; the S3 storage tab shows the outcome when it ends.", "detached");
    controller.current = kind === "secure" ? streamSecureStorage(options.removeLeftoverKeys ?? [], onStep, onDone, onDrop) : streamMigrateStorage(options, onStep, onDone, onDrop);
  }, [onFinished]);

  const reset = useCallback(() => setRun((r) => (r && !r.running ? null : r)), []);
  return { run, start, reset };
}
