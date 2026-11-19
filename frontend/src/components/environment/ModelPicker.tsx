import React, { useState } from "react";
import { MenuToggle, type MenuToggleElement, Select, SelectList, SelectOption } from "@patternfly/react-core";
import type { CatalogModel, ResourcesStatus } from "../../types";
import { envStatusText } from "./resourceStatus";
import { type Busy, catalogModel, modelIn, modelOptionReason, modelStatus } from "./featureState";

/** "6Gi" as "6 GiB". */
const gib = (q: string) => q.replace(/^(\d+(?:\.\d+)?)Gi$/, "$1 GiB");

/** What a catalog model reserves on a worker. */
export const modelNeeds = (c: CatalogModel | undefined): string =>
  c ? `${c.cpu} CPUs and ${gib(c.memoryRequest)} of memory (up to ${gib(c.memory)})` : "several CPUs and GiB of memory";

/**
 * What a catalog model is for, once: its use case as the dashboard shows it
 * (opendatahub.io/genai-use-case), and an embedding model's vector size.
 */
export function modelUseWords(c: CatalogModel): string {
  return c.type === "embedding" && c.embeddingDimension ? `${c.useCase}; vectors of ${c.embeddingDimension} numbers` : c.useCase;
}

/** One line about a catalog model: what it is for and what it reserves. */
export function modelOptionWords(c: CatalogModel): string {
  return `${modelUseWords(c)}. Reserves ${modelNeeds(c)}. Downloads about ${c.downloadSize}.`;
}

/**
 * The model picker: PatternFly's single Select, each option with what the
 * model is, what it is for, what it reserves and its download, its state
 * in the project, and, when it cannot be set up now, why.
 */
export const ModelPicker: React.FC<{ status: ResourcesStatus; project: string | null; selected: string; onSelect: (name: string) => void; busy: Busy }> = ({ status, project, selected, onSelect, busy }) => {
  const [isOpen, setIsOpen] = useState(false);
  const catalog = status.models?.catalog ?? [];
  const current = catalogModel(status, selected);
  const toggle = (ref: React.Ref<MenuToggleElement>) => (
    <MenuToggle ref={ref} id="models-model" isExpanded={isOpen} isFullWidth onClick={() => setIsOpen(!isOpen)} aria-label="Model">
      {current?.displayName ?? selected}
    </MenuToggle>
  );
  return (
    <Select id="models-model-select" isOpen={isOpen} selected={selected} toggle={toggle} shouldFocusToggleOnSelect popperProps={{ width: "trigger" }}
      onOpenChange={setIsOpen} onSelect={(_e, v) => { if (typeof v === "string") onSelect(v); setIsOpen(false); }}>
      <SelectList aria-label="Models this tool can set up">
        {catalog.map((c) => {
          const reason = modelOptionReason(status, project, c.name);
          const here = project ? modelIn(status, project, c.name) : undefined;
          const state = project && here ? ` In ${project}: ${envStatusText(modelStatus(status, project, busy, c.name))}.` : "";
          return (
            <SelectOption key={c.name} value={c.name} isDisabled={!!reason}
              description={reason ? `${modelOptionWords(c)} ${reason}` : `${modelOptionWords(c)}${state}`}>
              {c.displayName}
            </SelectOption>
          );
        })}
      </SelectList>
    </Select>
  );
};
