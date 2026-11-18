import React from "react";
import {
  Alert,
  Button,
  Checkbox,
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  InputGroup,
  InputGroupItem,
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
import {
  type AudienceForm, type DatabaseForm, type LimitForm, type WindowUnit, DATABASE_CHOICES, LIMIT_PRESETS, MAX_RATES,
  audienceProblem, connectionUrlProblem, limitProblem,
} from "./maasForms";

const Problem: React.FC<{ id: string; text: string | null }> = ({ id, text }) => text ? (
  <FormHelperText>
    <HelperText id={id}><HelperTextItem variant="error">{text}</HelperTextItem></HelperText>
  </FormHelperText>
) : null;

/**
 * Where MaaS keeps its API keys: the three options with their trade-offs,
 * the URL for Your PostgreSQL (masked by default, never stored), and what
 * the chosen one creates. except leaves out the current database (Change
 * database).
 */
export const DatabaseChoice: React.FC<{ db: MaaSDatabase; value: DatabaseForm; onChange: (f: DatabaseForm) => void; except?: string; idPrefix: string }> = ({ db, value, onChange, except, idPrefix }) => {
  const urlProblem = value.kind === "external" && value.connectionUrl ? connectionUrlProblem(value.connectionUrl.trim()) : null;
  return (
    <Form onSubmit={(e) => e.preventDefault()}>
      <FormGroup role="radiogroup" isStack fieldId={`${idPrefix}-database`} label="Where MaaS keeps its API keys">
        {DATABASE_CHOICES.filter((c) => c.id !== except || c.id === "external").map((c) => {
          const choice = db.choices.find((x) => x.id === c.id);
          const blocked = choice?.blockedReason;
          return (
            <Radio key={c.id} id={`${idPrefix}-database-${c.id}`} name={`${idPrefix}-database`} isChecked={value.kind === c.id} isDisabled={!!blocked}
              onChange={() => onChange({ ...value, kind: c.id })}
              label={<>{c.label}{c.id === db.default && <> <Label isCompact color="blue">Default</Label></>}</>}
              description={blocked ? <>{c.description} Not available: {blocked}</> : c.description}
              body={value.kind === c.id && c.id === "external" ? (
                <FormGroup label="Connection URL" isRequired fieldId={`${idPrefix}-database-url`}>
                  <InputGroup>
                    <InputGroupItem isFill>
                      <TextInput id={`${idPrefix}-database-url`} type={value.showUrl ? "text" : "password"} autoComplete="off" spellCheck={false}
                        placeholder="postgresql://USER:PASSWORD@HOST:5432/DATABASE?sslmode=require" value={value.connectionUrl}
                        validated={urlProblem ? "error" : "default"} aria-describedby={`${idPrefix}-database-url-help`}
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
                        <HelperTextItem>It holds the password: it is written only into Secret maas-db-config in {db.namespace} and never shown, logged or kept anywhere else. maas-api must reach the database from the cluster.</HelperTextItem>
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

/** A model's limit: the docs' presets, or custom rates of tokens per window. */
export const LimitFields: React.FC<{ value: LimitForm; onChange: (f: LimitForm) => void; idPrefix: string }> = ({ value, onChange, idPrefix }) => {
  const problem = limitProblem(value);
  const setRow = (i: number, patch: Partial<LimitForm["rows"][number]>) => onChange({ ...value, rows: value.rows.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  return (
    <Form onSubmit={(e) => e.preventDefault()}>
      <FormGroup role="radiogroup" isStack fieldId={`${idPrefix}-limit`} label="Token limit for each user">
        {LIMIT_PRESETS.map((p) => (
          <Radio key={p.id} id={`${idPrefix}-limit-${p.id}`} name={`${idPrefix}-limit`} isChecked={value.preset === p.id} label={p.label} description={p.description}
            onChange={() => onChange({ ...value, preset: p.id })} />
        ))}
        <Radio id={`${idPrefix}-limit-custom`} name={`${idPrefix}-limit`} isChecked={value.preset === "custom"} label="Custom"
          description="One or more limits, each a number of tokens per window; a user is held to all of them."
          onChange={() => onChange({ ...value, preset: "custom" })}
          body={value.preset === "custom" ? (
            <>
              {value.rows.map((r, i) => (
                <InputGroup key={i} className="pf-v6-u-mb-sm">
                  <InputGroupItem isFill>
                    <TextInput id={`${idPrefix}-rate-${i}-limit`} aria-label={`Rate ${i + 1}: tokens`} type="number" min={1} value={r.limit} onChange={(_e, v) => setRow(i, { limit: v })} />
                  </InputGroupItem>
                  <InputGroupItem isPlain>tokens every</InputGroupItem>
                  <InputGroupItem>
                    <TextInput id={`${idPrefix}-rate-${i}-amount`} aria-label={`Rate ${i + 1}: window length`} type="number" min={1} max={9999} value={r.amount} onChange={(_e, v) => setRow(i, { amount: v })} />
                  </InputGroupItem>
                  <InputGroupItem>
                    <FormSelect id={`${idPrefix}-rate-${i}-unit`} aria-label={`Rate ${i + 1}: window unit`} value={r.unit} onChange={(_e, v) => setRow(i, { unit: v as WindowUnit })}>
                      <FormSelectOption value="s" label="seconds" />
                      <FormSelectOption value="m" label="minutes" />
                      <FormSelectOption value="h" label="hours" />
                    </FormSelect>
                  </InputGroupItem>
                  <InputGroupItem>
                    <Button variant="plain" aria-label={`Remove rate ${i + 1}`} icon={<MinusCircleIcon />} isDisabled={value.rows.length === 1}
                      onClick={() => onChange({ ...value, rows: value.rows.filter((_r, j) => j !== i) })} />
                  </InputGroupItem>
                </InputGroup>
              ))}
              <Button variant="link" icon={<PlusCircleIcon />} isDisabled={value.rows.length >= MAX_RATES}
                onClick={() => onChange({ ...value, rows: [...value.rows, { limit: "10000", amount: "1", unit: "h" }] })}>Add a rate</Button>
              <Problem id={`${idPrefix}-limit-problem`} text={problem} />
            </>
          ) : undefined} />
      </FormGroup>
    </Form>
  );
};

/** Who may use the published models: everyone signed in, or named groups and users. */
export const AudienceFields: React.FC<{ value: AudienceForm; onChange: (f: AudienceForm) => void; idPrefix: string }> = ({ value, onChange, idPrefix }) => {
  const problem = audienceProblem(value);
  return (
    <Form onSubmit={(e) => e.preventDefault()}>
      <FormGroup role="radiogroup" isStack fieldId={`${idPrefix}-audience`} label="Who can use the published models">
        <Radio id={`${idPrefix}-audience-everyone`} name={`${idPrefix}-audience`} isChecked={value.everyone} label="Everyone signed in"
          description="Every user signed in to the cluster (group system:authenticated), as the MaaS docs' sample." onChange={() => onChange({ ...value, everyone: true })} />
        <Radio id={`${idPrefix}-audience-specific`} name={`${idPrefix}-audience`} isChecked={!value.everyone} label="Specific groups and users"
          description="Only these can create API keys for the models and call them." onChange={() => onChange({ ...value, everyone: false })}
          body={!value.everyone ? (
            <>
              <FormGroup label="OpenShift groups" fieldId={`${idPrefix}-groups`}>
                <TextArea id={`${idPrefix}-groups`} rows={2} resizeOrientation="vertical" value={value.groups} placeholder="One per line" onChange={(_e, v) => onChange({ ...value, groups: v })} />
                <FormHelperText><HelperText><HelperTextItem>The MaaS docs advise groups for people (oc adm groups new, oc adm groups add-users).</HelperTextItem></HelperText></FormHelperText>
              </FormGroup>
              <FormGroup label="Users" fieldId={`${idPrefix}-users`}>
                <TextArea id={`${idPrefix}-users`} rows={2} resizeOrientation="vertical" value={value.users} placeholder="system:serviceaccount:<namespace>:<name>, one per line" onChange={(_e, v) => onChange({ ...value, users: v })} />
                <FormHelperText><HelperText><HelperTextItem>For service accounts and other programmatic users; the docs warn that many individual users strain rate limiting.</HelperTextItem></HelperText></FormHelperText>
              </FormGroup>
              <Problem id={`${idPrefix}-audience-problem`} text={problem} />
            </>
          ) : undefined} />
      </FormGroup>
    </Form>
  );
};

/** Change database's acknowledgement: every API key is lost. */
export const KeysLostCheck: React.FC<{ checked: boolean; onChange: (v: boolean) => void }> = ({ checked, onChange }) => (
  <>
    <Alert variant="danger" isInline component="p" title="Every API key is lost">The new database starts empty: everyone creates their API keys again. The models, the subscription and the access policy stay.</Alert>
    <Checkbox id="maas-keys-lost" label="I understand that every API key stops working" isChecked={checked} onChange={(_e, v) => onChange(v)} />
  </>
);
