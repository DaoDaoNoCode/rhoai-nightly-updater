import { useCallback, useEffect, useRef, useState } from "react";
import { streamMigrateStorage, trackFeature, type MigrateStorageOptions } from "../../services/api";
import type { UpdateStep } from "../../types";

export type MigrateKind = "migrate" | "remove";

/** The steps each run streams, in order (pkg/cluster MigrateS3Storage). */
export const MIGRATE_STEPS: Record<MigrateKind, { id: string; title: string }[]> = {
  migrate: [
    { id: "check", title: "Check the old storage" },
    { id: "storage", title: "Set up the new storage with the same keys" },
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

export interface MigrateRun {
  kind: MigrateKind;
  steps: Record<string, MigrateStepState>;
  running: boolean;
  /** The outcome, once the run ended (or the connection to it was lost). */
  result: { success: boolean; message: string; errorCode?: string } | null;
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
    setRun({ kind, steps: {}, running: true, result: null });
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
      () => finish(false, "The connection to the server was lost. The operation continues on the server; the Storage card shows the outcome when it ends.", "detached"),
    );
  }, [onFinished]);

  const reset = useCallback(() => setRun((r) => (r && !r.running ? null : r)), []);
  return { run, start, reset };
}
