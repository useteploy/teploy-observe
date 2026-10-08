import { test } from "node:test";
import assert from "node:assert/strict";
import { readAIConfig } from "./aiConfig.ts";
const valid = { provider: "openai", endpoint: "https://example.test", model: "test", has_key: true };
test("OBS26-111 rejected JSON responses cannot be adopted as saved config", async () => {
  for (const status of [400, 401, 403, 500]) {
    await assert.rejects(readAIConfig(new Response(JSON.stringify({ error: "refused" }), { status })), /refused/);
  }
});
test("AI config success is validated including masked key contract", async () => {
  assert.deepEqual(await readAIConfig(new Response(JSON.stringify(valid))), valid);
  for (const data of [{}, { ...valid, provider: 42 }, { ...valid, has_key: "true" }]) await assert.rejects(readAIConfig(new Response(JSON.stringify(data))), /Invalid/);
  await assert.rejects(readAIConfig(new Response("invalid")));
});
