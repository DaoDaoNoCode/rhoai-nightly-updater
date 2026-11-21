import type { PlaygroundStatus, ResourcesStatus } from "../../types";
import { type Busy, pipelineServerIn, pipelineServerStatus, pipelineServerUnmanaged, playgroundIn } from "./featureState";
import { envStatusText } from "./resourceStatus";
import type { ProjectOption } from "./ProjectSelect";

/** The projects the pickers list: the data science projects, and any project the status names. */
export function environmentProjectNames(projectList: string[] | null, status: ResourcesStatus | null): string[] {
  return [...new Set([
    ...(projectList ?? []),
    ...(status?.pipelineServers ?? []).map((ps) => ps.namespace ?? ""),
    ...(status?.unmanagedPipelineServers ?? []).map((ps) => ps.namespace ?? ""),
  ].filter(Boolean))].sort();
}

/** The Pipelines tab's picker: each project's pipeline server. */
export function pipelinesProjectOptions(status: ResourcesStatus | null, names: string[], busy: Busy): ProjectOption[] {
  return names.map((name) => {
    const state = pipelineServerStatus(status, name, busy);
    const foreign = pipelineServerUnmanaged(status, name);
    const has = !!pipelineServerIn(status, name) || foreign;
    return { name, description: has ? `Pipeline server: ${envStatusText(state)}${foreign ? " · not managed by this tool" : ""}` : undefined };
  });
}

/** The Models tab's picker: each project's models. */
export function modelsProjectOptions(status: ResourcesStatus | null, names: string[]): ProjectOption[] {
  return names.map((name) => {
    const here = (status?.models?.items ?? []).filter((i) => i.namespace === name);
    const ready = here.filter((m) => m.ready).length;
    const words = here.length === 0 ? undefined : here.length === 1
      ? `Model: ${here[0].name} ${here[0].ready ? "ready" : here[0].terminating ? "deleting" : "not ready"}${here[0].managedByTool === false ? " · not managed by this tool" : ""}`
      : `Models: ${here.length}, ${ready} ready`;
    return { name, description: words };
  });
}

/** The Playground tab's picker: each project's playground. */
export function playgroundProjectOptions(pg: PlaygroundStatus | null, names: string[]): ProjectOption[] {
  return names.map((name) => {
    const i = playgroundIn(pg, name);
    return { name, description: i?.deployed ? `Playground: ${i.ready ? "Ready" : i.terminating ? "Deleting" : "Not ready"}${i.managedByTool ? "" : " · not managed by this tool"}` : undefined };
  });
}
