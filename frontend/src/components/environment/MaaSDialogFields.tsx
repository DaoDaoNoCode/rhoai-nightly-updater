import React from "react";
import {
  Alert,
  Button,
  Checkbox,
  Content,
  Flex,
  FlexItem,
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  InputGroup,
  InputGroupItem,
  Stack,
  StackItem,
  Label,
  Radio,
  TextArea,
  TextInput,
} from "@patternfly/react-core";
import EyeIcon from "@patternfly/react-icons/dist/esm/icons/eye-icon";
import EyeSlashIcon from "@patternfly/react-icons/dist/esm/icons/eye-slash-icon";
import PlusCircleIcon from "@patternfly/react-icons/dist/esm/icons/plus-circle-icon";
import MinusCircleIcon from "@patternfly/react-icons/dist/esm/icons/minus-circle-icon";
import type { MaaSDatabase } from "../../types";
import { checkMaaSGroups } from "../../services/api";
import {
  type AudienceForm, type DatabaseForm, type LimitForm, type WindowUnit, DATABASE_CHOICES, LIMIT_PRESETS, MAX_RATES,
  audienceProblem, audienceRequest, connectionUrlProblem, groupNameProblem, limitSummary, MAX_NAMES, openShiftGroupName, rateRowProblem,
} from "./maasForms";

const Problem: React.FC<{ id: string; text: string | null }> = ({ id, text }) => text ? (
  <FormHelperText>
    <HelperText id={id}><HelperTextItem variant="error">{text}</HelperTextItem></HelperText>
  </FormHelperText>
) : null;

/**
 * Where MaaS keeps its API keys: the three options with their trade-offs,
 * the URL for Your PostgreSQL (masked by default, never stored; checked
 * when the field is left, not at every keystroke), and the warning that
 * the development database loses its keys. except is the current database
 * (Change database): nothing is preselected, and no option is "Default".
 */
export const DatabaseChoice: React.FC<{ db: MaaSDatabase; value: DatabaseForm; onChange: (f: DatabaseForm) => void; except?: string; idPrefix: string }> = ({ db, value, onChange, except, idPrefix }) => {
  const urlProblem = value.kind === "external" && value.urlTouched ? connectionUrlProblem(value.connectionUrl.trim()) : null;
  return (
    <Form onSubmit={(e) => e.preventDefault()}>
      <FormGroup role="radiogroup" isStack fieldId={`${idPrefix}-database`} label="Where MaaS keeps its API keys">
        {DATABASE_CHOICES.filter((c) => c.id !== except || c.id === "external").map((c) => {
          const choice = db.choices.find((x) => x.id === c.id);
          const blocked = choice?.blockedReason;
          return (
            <Radio key={c.id} id={`${idPrefix}-database-${c.id}`} name={`${idPrefix}-database`} isChecked={value.kind === c.id} isDisabled={!!blocked}
              onChange={() => onChange({ ...value, kind: c.id })}
              label={<>
                {c.label}
                {except === undefined && c.id === db.default && <> <Label isCompact color="blue">Default</Label></>}
                {c.notInDocs && <> <Label isCompact color="orange">Not in the MaaS docs</Label></>}
              </>}
              description={blocked ? <>{c.description} Not available: {blocked}</> : c.description}
              body={value.kind !== c.id ? undefined : c.id === "development" ? (
                <Alert variant="warning" isInline isPlain component="p" title="API keys are lost whenever this database restarts">
                  The pod has no storage. After a restart (node reboot, eviction), every API key is gone and maas-api must be restarted; this tab shows how. Choose a persistent database if keys must survive.
                </Alert>
              ) : c.id === "external" ? (
                <FormGroup label="Connection URL" isRequired fieldId={`${idPrefix}-database-url`}>
                  <InputGroup>
                    <InputGroupItem isFill>
                      <TextInput id={`${idPrefix}-database-url`} type={value.showUrl ? "text" : "password"} autoComplete="off" spellCheck={false}
                        placeholder="postgresql://USER:PASSWORD@HOST:5432/DATABASE?sslmode=require" value={value.connectionUrl}
                        validated={urlProblem ? "error" : "default"} aria-describedby={`${idPrefix}-database-url-help`}
                        onBlur={() => onChange({ ...value, urlTouched: true })}
                        onChange={(_e, v) => onChange({ ...value, connectionUrl: v })} />
                    </InputGroupItem>
                    <InputGroupItem>
                      <Button variant="control" aria-label={value.showUrl ? "Hide the URL" : "Show the URL"} icon={value.showUrl ? <EyeSlashIcon /> : <EyeIcon />}
                        onClick={() => onChange({ ...value, showUrl: !value.showUrl })} />
                    </InputGroupItem>
                  </InputGroup>
                  {urlProblem ? <Problem id={`${idPrefix}-database-url-help`} text={urlProblem} /> : (
                    <FormHelperText>
                      <HelperText id={`${idPrefix}-database-url-help`}>
                        <HelperTextItem>Stored only in Secret maas-db-config in {db.namespace}; never shown again or logged. maas-api must reach this address from the cluster.</HelperTextItem>
                      </HelperText>
                    </FormHelperText>
                  )}
                </FormGroup>
              ) : undefined} />
          );
        })}
      </FormGroup>
    </Form>
  );
};

/** A model's limit: the docs' examples, unlimited (when the cluster's MaaS has it), or custom token rate limits. */
export const LimitFields: React.FC<{ value: LimitForm; onChange: (f: LimitForm) => void; idPrefix: string; noUnlimited?: boolean }> = ({ value, onChange, idPrefix, noUnlimited }) => {
  const summary = limitSummary(value);
  const setRow = (i: number, patch: Partial<LimitForm["rows"][number]>) => onChange({ ...value, rows: value.rows.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  const digits = (v: string) => v.replace(/[^0-9]/g, "");
  return (
    <Form onSubmit={(e) => e.preventDefault()}>
      <FormGroup role="radiogroup" isStack fieldId={`${idPrefix}-limit`} label="Token limit for each user">
        <HelperText id={`${idPrefix}-limit-help`}>
          <HelperTextItem>Each call counts the tokens in the prompt and the answer. Over the limit, MaaS refuses calls with 429 Too Many Requests until the period resets.</HelperTextItem>
        </HelperText>
        {LIMIT_PRESETS.map((p) => {
          const off = p.id === "unlimited" && !!noUnlimited;
          return (
            <Radio key={p.id} id={`${idPrefix}-limit-${p.id}`} name={`${idPrefix}-limit`} isChecked={value.preset === p.id} label={p.label} isDisabled={off}
              description={off ? <>{p.description} This cluster&apos;s MaaS has no unlimited option.</> : p.description}
              onChange={() => onChange({ ...value, preset: p.id })} />
          );
        })}
        <Radio id={`${idPrefix}-limit-custom`} name={`${idPrefix}-limit`} isChecked={value.preset === "custom"} label="Custom"
          description="Set one or more limits, for example 10,000 tokens per hour and 100,000 per day."
          onChange={() => onChange({ ...value, preset: "custom" })}
          body={value.preset === "custom" ? (
            <Stack hasGutter>
              {value.rows.map((r, i) => {
                const problem = rateRowProblem(value, i);
                const tokensBad = !!problem && problem.includes("tokens");
                const periodBad = !!problem && !tokensBad;
                const helpId = `${idPrefix}-rate-${i}-problem`;
                return (
                  <StackItem key={i}>
                    <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "nowrap" }}>
                      <FlexItem>
                        <TextInput id={`${idPrefix}-rate-${i}-limit`} aria-label={`Limit ${i + 1}: tokens`} type="text" inputMode="numeric" value={r.limit} style={{ width: "14ch" }}
                          validated={tokensBad ? "error" : "default"} aria-describedby={problem ? helpId : undefined} onChange={(_e, v) => setRow(i, { limit: digits(v) })} />
                      </FlexItem>
                      <FlexItem>tokens per</FlexItem>
                      <FlexItem>
                        <TextInput id={`${idPrefix}-rate-${i}-amount`} aria-label={`Limit ${i + 1}: period length`} type="text" inputMode="numeric" value={r.amount} style={{ width: "9ch" }}
                          validated={periodBad ? "error" : "default"} aria-describedby={problem ? helpId : undefined} onChange={(_e, v) => setRow(i, { amount: digits(v) })} />
                      </FlexItem>
                      <FlexItem>
                        <FormSelect id={`${idPrefix}-rate-${i}-unit`} aria-label={`Limit ${i + 1}: period unit`} value={r.unit} validated={periodBad ? "error" : "default"}
                          onChange={(_e, v) => setRow(i, { unit: v as WindowUnit })}>
                          <FormSelectOption value="s" label="seconds" />
                          <FormSelectOption value="m" label="minutes" />
                          <FormSelectOption value="h" label="hours" />
                        </FormSelect>
                      </FlexItem>
                      <FlexItem>
                        <Button variant="plain" aria-label={`Remove limit ${i + 1}`} icon={<MinusCircleIcon />} isDisabled={value.rows.length === 1}
                          onClick={() => onChange({ ...value, rows: value.rows.filter((_r, j) => j !== i) })} />
                      </FlexItem>
                    </Flex>
                    <Problem id={helpId} text={problem} />
                  </StackItem>
                );
              })}
              <StackItem>
                <Button variant="link" isInline icon={<PlusCircleIcon />} isDisabled={value.rows.length >= MAX_RATES}
                  onClick={() => onChange({ ...value, rows: [...value.rows, { limit: "100000", amount: "24", unit: "h" }] })}>Add token rate limit</Button>
              </StackItem>
              {summary && <StackItem><Content component="small" aria-live="polite">{summary}</Content></StackItem>}
            </Stack>
          ) : undefined} />
      </FormGroup>
    </Form>
  );
};

/** How long typing must pause before the groups typed are checked. */
export const GROUP_CHECK_DELAY_MS = 400;

/**
 * The groups typed that have no OpenShift Group: at once for a name that
 * cannot be one, and after typing pauses for the others (GET
 * /api/resources/maas/groups, which only a cluster-admin may ask). A failed
 * check shows nothing: Publish checks again and says so in its steps.
 */
function useMissingGroups(groups: string[]): string[] {
  const key = groups.join("\n");
  const [missing, setMissing] = React.useState<Record<string, boolean>>({});
  React.useEffect(() => {
    const ask = key.split("\n").filter((g) => g && !groupNameProblem(g) && openShiftGroupName(g)).slice(0, MAX_NAMES);
    if (ask.length === 0) return;
    let live = true;
    const timer = setTimeout(() => {
      checkMaaSGroups(ask).then((problems) => {
        if (!live) return;
        const found = new Set(problems.filter((p) => p.missing).map((p) => p.name));
        setMissing((m) => ({ ...m, ...Object.fromEntries(ask.map((g) => [g, found.has(g)])) }));
      }).catch(() => undefined);
    }, GROUP_CHECK_DELAY_MS);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [key]);
  return groups.filter((g) => !groupNameProblem(g) && (!openShiftGroupName(g) || missing[g]));
}

export const missingGroupText = (g: string) => `${g} is not a group on this cluster. Nobody is in it unless your identity provider adds it at sign-in.`;

/** Who may use the published models: everyone signed in, or named groups and users. */
export const AudienceFields: React.FC<{ value: AudienceForm; onChange: (f: AudienceForm) => void; idPrefix: string }> = ({ value, onChange, idPrefix }) => {
  const problem = audienceProblem(value);
  const missing = useMissingGroups(value.everyone ? [] : audienceRequest(value).groups ?? []);
  return (
    <Form onSubmit={(e) => e.preventDefault()}>
      <FormGroup role="radiogroup" isStack fieldId={`${idPrefix}-audience`} label="Who can use the published models">
        <Radio id={`${idPrefix}-audience-everyone`} name={`${idPrefix}-audience`} isChecked={value.everyone} label="Everyone signed in"
          description="Every user signed in to the cluster (group system:authenticated), as the MaaS docs' sample." onChange={() => onChange({ ...value, everyone: true })} />
        <Radio id={`${idPrefix}-audience-specific`} name={`${idPrefix}-audience`} isChecked={!value.everyone} label="Specific groups and users"
          description="Only these can create API keys for the models and call them." onChange={() => onChange({ ...value, everyone: false })}
          body={!value.everyone ? (
            <Stack hasGutter>
              <StackItem>
                <FormGroup label="Groups" fieldId={`${idPrefix}-groups`}>
                  <TextArea id={`${idPrefix}-groups`} aria-describedby={`${idPrefix}-groups-help`} rows={2} resizeOrientation="vertical" value={value.groups} placeholder="One per line" onChange={(_e, v) => onChange({ ...value, groups: v })} />
                  <FormHelperText>
                    <HelperText id={`${idPrefix}-groups-help`}>
                      <HelperTextItem>Use groups for people. A cluster admin creates them with oc adm groups new.</HelperTextItem>
                      {missing.map((g) => <HelperTextItem key={g} variant="warning">{missingGroupText(g)}</HelperTextItem>)}
                    </HelperText>
                  </FormHelperText>
                </FormGroup>
              </StackItem>
              <StackItem>
                <FormGroup label="Users" fieldId={`${idPrefix}-users`}>
                  <TextArea id={`${idPrefix}-users`} aria-describedby={`${idPrefix}-users-help`} rows={2} resizeOrientation="vertical" value={value.users} placeholder="system:serviceaccount:<namespace>:<name>, one per line" onChange={(_e, v) => onChange({ ...value, users: v })} />
                  <FormHelperText>
                    <HelperText id={`${idPrefix}-users-help`}>
                      <HelperTextItem>For service accounts. For teams, use a group: the MaaS docs advise against long user lists.</HelperTextItem>
                    </HelperText>
                  </FormHelperText>
                </FormGroup>
              </StackItem>
              {problem && <StackItem><Problem id={`${idPrefix}-audience-problem`} text={problem} /></StackItem>}
            </Stack>
          ) : undefined} />
        <HelperText><HelperTextItem>Applies to every model you publish here.</HelperTextItem></HelperText>
      </FormGroup>
    </Form>
  );
};

/** Change database's acknowledgement, under the data-loss alert: every API key is lost. */
export const KeysLostCheck: React.FC<{ checked: boolean; onChange: (v: boolean) => void }> = ({ checked, onChange }) => (
  <Checkbox id="maas-keys-lost" label="I understand that every API key stops working" isChecked={checked} onChange={(_e, v) => onChange(v)} />
);
