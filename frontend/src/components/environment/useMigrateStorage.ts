import { useCallback, useEffect, useRef, useState } from "react";
import { streamMigrateStorage, trackFeature, type MigrateStorageOptions } from "../../services/api";
import type { UpdateStep } from "../../types";

export type MigrateKind = "migrate" | "remove";

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
};

export interface MigrateStepState {
  status: UpdateStep["status"];
  message: string;
}

/** Each run's dialog title. */
export const MIGRATE_TITLES: Record<MigrateKind, string> = { migrate: "Migrate storage", remove: "Delete the old storage" };

export interface MigrateRun {
  kind: MigrateKind;
  /** When it started (epoch ms). */
  startedAt: number;
  steps: Record<string, MigrateStepState>;
  running: boolean;
  /** The outcome, once the run ended (or the connection to it was lost). */
  result: { success: boolean; message: string; errorCode?: string } | null;
}

/** The step a run is on now, for the alert shown while its dialog is closed. */
export function migrateStepNow(run: MigrateRun): { title: string; index: number; count: number } | null {
  const steps = MIGRATE_STEPS[run.kind];
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

  const start = useCallback((kind: MigrateKind, options: MigrateStorageOptions) => {
    trackFeature(kind === "migrate" ? "migrate-storage" : "remove-old-storage");
    setRun({ kind, startedAt: Date.now(), steps: {}, running: true, result: null });
    let finalMessage = "";
    const finish = (success: boolean, message: string, errorCode?: string) => {
      controller.current = null;
      setRun((r) => (r ? { ...r, running: false, result: { success, message, errorCode } } : r));
      onFinished();
    };
    controller.current = streamMigrateStorage(
      options,
      (step) => {
        if (step.step === "operation_complete") {
          finalMessage = step.message;
          return;
        }
        setRun((r) => (r ? { ...r, steps: { ...r.steps, [step.step]: { status: step.status, message: step.message } } } : r));
      },
      (success, error, apiError) => finish(success, success ? finalMessage : error || finalMessage || "The operation failed.", apiError?.errorCode),
      () => finish(false, "The connection to the server was lost. The operation continues on the server; the S3 storage on the Pipelines tab shows the outcome when it ends.", "detached"),
    );
  }, [onFinished]);

  const reset = useCallback(() => setRun((r) => (r && !r.running ? null : r)), []);
  return { run, start, reset };
}
