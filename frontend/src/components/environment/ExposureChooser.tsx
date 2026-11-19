import React, { useEffect, useState } from "react";
import {
  Alert,
  AlertActionLink,
  Button,
  Content,
  Label,
  Radio,
  HelperText,
  HelperTextItem,
  Skeleton,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { ModelExposureOption, ModelExposures, ModelState } from "../../types";
import { getModelExposures, toApiError } from "../../services/api";
import { type Pending } from "./resourceRow";

// --- Models routing: how a new model is reached ---------------------------------

/** GET /api/resources/models/exposures for the project and model, read again when refreshKey changes. */
export function useModelExposures(project: string | null, model: string, refreshKey: string, enabled: boolean) {
  const [data, setData] = useState<ModelExposures | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (!enabled || !project) {
      setData(null);
      return;
    }
    let live = true;
    getModelExposures(project, model)
      .then((d) => { if (live) { setData(d); setError(null); } })
      .catch((e) => { if (live) setError(toApiError(e, "Cannot read how the model can be reached").message); });
    return () => { live = false; };
  }, [project, model, refreshKey, enabled]);
  return { data, error };
}

/** How a model is reached, in a few words. */
export function exposureWords(m: Pick<ModelState, "exposure" | "gateway" | "maasOnlySetup">): string {
  switch (m.exposure) {
    case "maas":
      return m.maasOnlySetup ? "Through MaaS only" : "Through MaaS while published (Unpublish on the MaaS tab routes it as before)";
    case "gateway":
      return `Through gateway ${m.gateway ?? ""}`;
    case "internal":
      return "Only inside the cluster (no route)";
    case "default-gateway":
      return `Through the cluster's default inference gateway (${m.gateway ?? "KServe's default"})`;
    default:
      return "Unknown";
  }
}

/**
 * The choice of how a new model is reached: every option with what it
 * means, the recommended one first chosen and why, and why the others
 * cannot be used now.
 */
export const ExposureChooser: React.FC<{
  exposures: ModelExposures | null; error: string | null; selected: string | null; onSelect: (key: string) => void;
  /** Set up the inference gateway (null: not offered, for example read-only). */
  onSetUpGateway?: (() => void) | null;
  /** Open the MaaS tab. */
  onMaaS?: () => void;
}> = ({ exposures, error, selected, onSelect, onSetUpGateway, onMaaS }) => {
  if (!exposures) {
    return error ? <Alert variant="warning" isInline component="p" title="Cannot read how the model can be reached">{error}</Alert> : (
      <Stack hasGutter>
        {[0, 1, 2].map((i) => <StackItem key={i}><Skeleton width={i === 0 ? "60%" : "45%"} screenreaderText={i === 0 ? "Reading the gateways on the cluster" : undefined} /></StackItem>)}
      </Stack>
    );
  }
  const chosen = exposures.options.find((o) => o.key === selected);
  const fixLink = (o: ModelExposureOption) => o.fix === "maas-tab" && onMaaS
    ? <Button variant="link" isInline onClick={onMaaS}>Go to the MaaS tab</Button>
    : o.fix === "inference-gateway" && onSetUpGateway
      ? <Button variant="link" isInline onClick={onSetUpGateway}>Set up the inference gateway</Button>
      : null;
  return (
    <Stack hasGutter>
      {!exposures.default && exposures.recommendGateway && (
        <StackItem>
          <Alert variant="info" isInline component="div" title="No gateway can carry this model yet"
            actionLinks={onSetUpGateway ? <AlertActionLink onClick={onSetUpGateway}>Set up the inference gateway</AlertActionLink> : undefined}>
            <Content component="p">{exposures.recommendGatewayReason} Or choose Only inside the cluster.</Content>
          </Alert>
        </StackItem>
      )}
      {exposures.options.map((o) => (
        <StackItem key={o.key}>
          <Radio id={`models-exposure-${o.key}`} name="models-exposure" isChecked={selected === o.key} isDisabled={!o.available} onChange={() => onSelect(o.key)}
            label={<>{o.title}{o.key === exposures.default ? <> <Label isCompact color="green">Recommended</Label></> : null}{o.needsAuth ? <> <Label isCompact color="orange">Needs Connectivity Link</Label></> : null}</>}
            description={o.description}
            body={!o.available ? (
              <HelperText>
                <HelperTextItem variant="warning">{o.reason ?? "Not available now."}{fixLink(o) ? <> {fixLink(o)}</> : null}</HelperTextItem>
              </HelperText>
            ) : o.key === exposures.default && exposures.defaultReason ? (
              // Why it is the recommended one, next to its Recommended label.
              <HelperText><HelperTextItem>{exposures.defaultReason.charAt(0).toUpperCase() + exposures.defaultReason.slice(1)}</HelperTextItem></HelperText>
            ) : undefined} />
        </StackItem>
      ))}
      {chosen?.warning && <StackItem><Alert variant="warning" isInline component="p" title="Before you choose this">{chosen.warning}</Alert></StackItem>}
    </Stack>
  );
};

/** The setup of a model, with the chosen way to reach it. */
export function withExposure(setup: Pending[], option: ModelExposureOption | undefined): Pending[] {
  if (!option) return setup;
  return setup.map((p) => (p.kind === "setup-models" && p.project ? { ...p, exposure: option.exposure, ...(option.gatewayRef && option.exposure === "gateway" ? { gatewayRef: option.gatewayRef } : {}), option } : p));
}
