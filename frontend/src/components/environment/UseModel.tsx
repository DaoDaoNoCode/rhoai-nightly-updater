import React from "react";
import {
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  HelperText,
  HelperTextItem,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { CatalogModel, ModelState } from "../../types";
import { TabSection, WhatNext } from "./FeatureLayout";
import { CopyableCode } from "./UseIt";
import { callCheckFailed, dashboardProjectHref } from "./featureState";
import { embeddingCurl, internalCurl, modelCurl, modelTokenLine, thinkingCurl, tokenSA, toolCurl } from "./modelCommands";

/** The two 404s a caller can get, and what each means. */
export const NotFoundNote: React.FC<{ name: string }> = ({ name }) => (
  <Content component="p">
    A 404 means one of two things. With <code>{`{"error":{..."type":"NotFoundError"}}`}</code> the request reached the model but its <code>&quot;model&quot;</code> is not <code>{name}</code> (the display name or the Hugging Face id are not it); with an empty body the request did not reach the model at all (on a gateway the path is <code>/&lt;project&gt;/&lt;model&gt;/v1/...</code>).
  </Content>
);

/**
 * A ready model without commands: this tool's model with no token of its
 * own yet (Set up again creates it), or a model of a catalog name this
 * tool did not set up (its token is the dashboard's). Never the user's
 * own token: a model's pod is in a project its editors control.
 */
const NoModelCommands: React.FC<{ model: ModelState; project: string; readOnly: boolean; baseReason: string | null; onSetUpAgain: () => void }> = ({ model, project, readOnly, baseReason, onSetUpAgain }) => {
  const name = model.name!;
  if (model.managedByTool !== true) {
    return (
      <Content component="p">
        This model was not set up by this tool (for example, it was deployed in the dashboard), so there are no commands for it here. Call it with the token the dashboard shows under Token authentication for that deployment: open {project} in the dashboard, then the model&apos;s details. Use that token rather than your own OpenShift token: whoever can edit {project} controls what answers at the model&apos;s address.
      </Content>
    );
  }
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">
          The model has no token of its own yet, so there are no commands to call it: it was set up before this tool made the dashboard&apos;s token for each model. Setting it up again creates ServiceAccount <code>{tokenSA(name)}</code>, which may only call this model, and its token Secret <code>default-name-{tokenSA(name)}</code>; nothing else changes.
        </Content>
      </StackItem>
      <StackItem>
        {readOnly
          ? <Content component="p">A cluster-admin sets it up again to create its token.</Content>
          : <>
            <Button variant="secondary" onClick={onSetUpAgain} isAriaDisabled={!!baseReason}>Set up again to create its token</Button>
            {baseReason && <Content component="p">Not available now: {baseReason}</Content>}
          </>}
      </StackItem>
    </Stack>
  );
};

/**
 * How to call a ready model: its address, curl with the model's own token
 * (by its type), the dashboard. A model published to MaaS answers only
 * through MaaS, so then it points at the MaaS tab's commands instead.
 */
export const UseModel: React.FC<{ model: ModelState; catalog?: CatalogModel; project: string; dashboardUrl?: string; onMaaS: () => void; readOnly: boolean; baseReason: string | null; onSetUpAgain: () => void }> = ({ model, catalog: c, project, dashboardUrl, onMaaS, readOnly, baseReason, onSetUpAgain }) => {
  const href = dashboardProjectHref(dashboardUrl, project);
  // The name the model serves: the "model" every request sends.
  const name = model.servedName ?? model.name!;
  const internal = model.exposure === "internal";
  const url = internal ? (model.internalUrl ?? model.url!) : model.url!;
  const embedding = c?.type === "embedding";
  const published = !!model.maasPublished;
  const token = modelTokenLine(project, model);
  const ours = model.managedByTool === true;
  if (model.servedNameWithheld && !published) {
    // The backend withholds a served name that is not safe in commands (pkg/cluster safeServedName).
    return (
      <TabSection title="Use it" id="models-use">
        <Content component="p">
          There are no commands for this model: the name it serves (<code>spec.model.name</code>) has characters that are not safe to paste into a shell, so this tool does not show it. Someone who can edit {project} set it; check the model in the dashboard.
        </Content>
      </TabSection>
    );
  }
  return (
    <TabSection title="Use it" id="models-use">
      {callCheckFailed(model) && (
        <HelperText><HelperTextItem variant="warning">{model.callCheck?.result === "unauthenticated"
          ? "The last check found that the model also answers without a token: anyone who can reach its address can use it."
          : "The last check got no answer; these commands will fail until it passes."}</HelperTextItem></HelperText>
      )}
      <DescriptionList aria-label="How to use the model">
        <DescriptionListGroup>
          <DescriptionListTerm>From a terminal</DescriptionListTerm>
          <DescriptionListDescription>
            {published ? (
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    {model.maasOnlySetup
                      ? "The model is reached through MaaS only: it is used through MaaS with an API key and in the dashboard's playground, and it has no direct address."
                      : "The model is published to Models as a Service. While it is published, it is used through MaaS with an API key and in the dashboard's playground; its direct address answers 404 until it is unpublished, because MaaS counts every call against a subscription, which a direct call does not have."}
                  </Content>
                </StackItem>
                <StackItem><Button variant="link" isInline onClick={onMaaS}>Use the MaaS tab&apos;s commands</Button></StackItem>
              </Stack>
            ) : internal ? (
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    The model is reached only inside the cluster, at its in-cluster address, without a token (anyone on the cluster whose network reaches {project} can call it). Run the command from a pod on the cluster; <code>-k</code> because the address uses KServe&apos;s own certificate.
                  </Content>
                </StackItem>
                <StackItem><CopyableCode id="models-curl" label="the command" code={internalCurl(url, name, embedding)} /></StackItem>
                <StackItem><NotFoundNote name={name} /></StackItem>
              </Stack>
            ) : !token ? (
              <NoModelCommands model={model} project={project} readOnly={readOnly} baseReason={baseReason} onSetUpAgain={onSetUpAgain} />
            ) : (
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    The model speaks the OpenAI API. The first command reads the model&apos;s own token from Secret <code>{model.tokenSecret}</code>: a token that can only call this model, the same one the dashboard shows for the model. Anyone who can read Secrets in {project} can use it.
                  </Content>
                </StackItem>
                <StackItem><CopyableCode id="models-curl" label="the commands" code={embedding ? embeddingCurl(url, name, token) : modelCurl(url, name, token)} /></StackItem>
                {!embedding && <StackItem><Content component="p">Requests that take longer than about a minute must stream (<code>{`"stream": true`}</code>), because the cloud load balancer closes a connection that sends nothing for 60 seconds (the AWS default).</Content></StackItem>}
                {embedding && <StackItem><Content component="p">The answer holds one vector of {c?.embeddingDimension ?? "fixed"} numbers per input text, under <code>data[].embedding</code>. Inputs longer than 512 tokens are refused.</Content></StackItem>}
                <StackItem><NotFoundNote name={name} /></StackItem>
              </Stack>
            )}
          </DescriptionListDescription>
        </DescriptionListGroup>
        {!published && !internal && token && c?.toolCalling && (
          <DescriptionListGroup>
            <DescriptionListTerm>Tool calling</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><Content component="p">Describe your tools in the request&apos;s <code>tools</code>; when one fits, the answer has <code>tool_calls</code> instead of text. Your program runs the tool and sends the result back in a <code>tool</code> message.</Content></StackItem>
                <StackItem><CopyableCode id="models-tools-curl" label="the tool-calling command" code={toolCurl(url, name, token)} /></StackItem>
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {!published && !internal && token && c?.toolCalling && (
          <DescriptionListGroup>
            <DescriptionListTerm>Reasoning</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><Content component="p">Thinking is off by default, so answers come quickly. To turn it on for one request, add <code>{`"chat_template_kwargs": {"enable_thinking": true}`}</code>; the reasoning then comes in <code>reasoning_content</code>, apart from the answer.</Content></StackItem>
                <StackItem><CopyableCode id="models-thinking-curl" label="the command with thinking on" code={thinkingCurl(url, name, token)} /></StackItem>
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {href && (
          <DescriptionListGroup>
            <DescriptionListTerm>In the dashboard</DescriptionListTerm>
            <DescriptionListDescription>
              <Content component="p">
                Open {project} in the dashboard to see the model with the project&apos;s deployments.
                {embedding
                  ? " In Gen AI studio it is listed under AI asset endpoints; the Playground tab here adds it to the playground as an embedding model."
                  : " In Gen AI studio, the model is listed under AI asset endpoints, where you can try it in the playground."}
              </Content>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      <WhatNext id="models-next" items={published ? [
        <>Run the MaaS tab&apos;s commands: they get an API key and ask the model to say hello.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI asset endpoints, then the playground.</>,
        model.maasOnlySetup
          ? <>Unpublishing it on the MaaS tab leaves it with no outside route, as before it was published: it then answers only at its in-cluster address. Deleting it here unpublishes it first.</>
          : <>To call its direct address again, unpublish it on the MaaS tab.</>,
      ] : internal ? [
        <>Run the command above from a pod on the cluster (for example a workbench in {project}): it asks the model {embedding ? "for the vector of one sentence" : "to say hello"}.</>,
        <>Point an OpenAI client that runs on the cluster at the in-cluster address, with <code>{name}</code> as the model. No token is needed.</>,
        <>To reach it from outside the cluster, delete it and set it up again through a gateway.</>,
      ] : !token ? [
        ours
          ? <>Set it up again to create its token; this tab then shows the commands that call it.</>
          : <>Open {project} in the dashboard and copy the deployment&apos;s token under Token authentication.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI asset endpoints, then the playground.</>,
      ] : embedding ? [
        <>Copy the commands above and run them: they ask the model for the vector of one sentence.</>,
        <>Point an OpenAI client&apos;s embeddings call at the address, with the token as the API key and <code>{name}</code> as the model.</>,
        <>Set up the playground on the Playground tab: it adds this model for searching your documents.</>,
      ] : [
        <>Copy the commands above and run them: they read the token, list the model, then ask it to say hello.</>,
        <>Point an OpenAI client at the address, with the token as the API key and <code>{name}</code> as the model.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI asset endpoints, then the playground.</>,
      ]} />
    </TabSection>
  );
};
