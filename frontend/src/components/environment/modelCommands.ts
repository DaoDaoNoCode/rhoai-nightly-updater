import type { ModelState } from "../../types";
import { shellQuote, shellWord } from "./UseIt";

/** The model's token ServiceAccount, as the dashboard names it (<model>-sa). */
export const tokenSA = (model: string) => `${model}-sa`;
const auth = `-H "Authorization: Bearer $TOKEN"`;

/**
 * The command that puts the model's own token in $TOKEN: the token Secret
 * this tool made as the dashboard does (pkg/cluster models_token.go), read
 * with the user's own oc. Only for this tool's model with that Secret;
 * otherwise null, and no command calls the model: the user's own token
 * never goes to a model, whose pod is in a project its editors control
 * (SECURITY.md, "Which token is used where"). The names are shell words
 * (shellWord): bare when plain, single-quoted otherwise.
 */
export function modelTokenLine(project: string, m: ModelState): string | null {
  return m.managedByTool === true && m.tokenSecret ? `TOKEN=$(oc get secret ${shellWord(m.tokenSecret)} -n ${shellWord(project)} -o jsonpath='{.data.token}' | base64 -d)` : null;
}

/**
 * A request's JSON body as one single-quoted shell word. The model name is
 * the model's spec.model.name, which anyone who can edit the project
 * chooses: it goes in through JSON.stringify and the quoting, so no
 * character of it can end the JSON string or the shell word.
 */
const jsonBody = (body: unknown) => shellQuote(JSON.stringify(body));

const HELLO = [{ role: "user", content: "Say hello in one sentence." }];
const EMBED_INPUT = ["Red Hat OpenShift AI serves models."];

/** Commands that call a chat model with its token (modelTokenLine). The address (from the cluster) and the body are single-quoted. */
export function modelCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    token,
    `curl -s ${auth} ${shellQuote(`${base}/v1/models`)}`,
    `curl -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`,
    `  -d ${jsonBody({ model, messages: HELLO, max_tokens: 64 })}`,
  ].join("\n");
}

/**
 * A chat request that turns the model's reasoning (thinking) on for this
 * request only. It streams (curl -N prints each event as it arrives):
 * thinking on CPU can take over a minute, and the cloud load balancer in
 * front of the gateway closes a connection that sends nothing for 60 s.
 */
export function thinkingCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    token,
    `curl -N -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`,
    `  -d ${jsonBody({ model, messages: [{ role: "user", content: "Is 1001 a prime number?" }], max_tokens: 1024, stream: true, chat_template_kwargs: { enable_thinking: true } })}`,
  ].join("\n");
}

/** A tool-calling request: the model answers with tool_calls when a tool fits. */
export function toolCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  const tool = { type: "function", function: { name: "get_weather", description: "Current weather in a city", parameters: { type: "object", properties: { city: { type: "string" } }, required: ["city"] } } };
  return [
    token,
    `curl -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`,
    `  -d ${jsonBody({ model, messages: [{ role: "user", content: "What is the weather in Brno?" }], tools: [tool], max_tokens: 256 })}`,
  ].join("\n");
}

/** Commands that ask an embedding model for a vector. */
export function embeddingCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    token,
    `curl -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/embeddings`)} \\`,
    `  -d ${jsonBody({ model, input: EMBED_INPUT })}`,
  ].join("\n");
}

/** A request to a model reached only inside the cluster: no token, from a pod on the cluster. */
export function internalCurl(url: string, model: string, embedding: boolean): string {
  const base = url.replace(/\/+$/, "");
  return embedding
    ? [`curl -sk -H "Content-Type: application/json" ${shellQuote(`${base}/v1/embeddings`)} \\`, `  -d ${jsonBody({ model, input: EMBED_INPUT })}`].join("\n")
    : [`curl -sk -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`, `  -d ${jsonBody({ model, messages: HELLO, max_tokens: 64 })}`].join("\n");
}
