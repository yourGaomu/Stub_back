import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import ts from "../../../front/node_modules/typescript/lib/typescript.js";
import { mergeOptions } from "../../../front/src/lib/options.ts";
import { mergeOptionsById } from "../../../app/src/hooks/use-paged-options.ts";
import { initialCreateForm, buildCreateLinkRequest } from "../../../app/src/lib/link-form.ts";

const getId = (value) => value.id;

test("Web option merging preserves selected values outside the result page", () => {
  const selected = { id: "selected-121", name: "Selected asset" };
  const page1 = [{ id: "a", name: "old" }, { id: "b", name: "second" }];
  const page2 = [{ id: "a", name: "updated" }, { id: "c", name: "third" }];
  const merged = mergeOptions(getId, [selected], page1, page2);
  assert.deepEqual(merged.map(getId), ["selected-121", "a", "b", "c"]);
  assert.equal(merged.find((value) => value.id === "a").name, "updated");
  assert.equal(merged[0], selected);
});

test("App option pagination deduplicates overlapping pages without changing selection", () => {
  const current = [{ id: "selected-121", name: "keep" }, { id: "first", name: "old" }];
  const merged = mergeOptionsById(current, [{ id: "first", name: "new" }, { id: "next", name: "page 2" }], getId);
  assert.deepEqual(merged.map(getId), ["selected-121", "first", "next"]);
  assert.equal(merged[1].name, "new");
  assert.equal(merged[0], current[0]);
});

test("App copy request carries chosen group and clearing explicitly sends null", () => {
  const source = { title: "Delivery", tags: [], groupId: "grp_00000000-0000-4000-8000-000000000001", target: { type: "url", url: "https://example.test" }, expiresAt: null, rules: { maxVisits: -1, allowedIpCidrs: [], requirePassword: false } };
  const form = initialCreateForm({ source });
  assert.equal(buildCreateLinkRequest(form).groupId, source.groupId);
  assert.equal(buildCreateLinkRequest({ ...form, groupId: null }).groupId, null);
});

// Compile only the actual client boundary; its environment constant is supplied
// explicitly and all network/storage/browser side effects are local test doubles.
const clientSource = (await readFile(new URL("../../../front/src/services/api/client.ts", import.meta.url), "utf8"))
  .replaceAll("import.meta.env.VITE_API_BASE_URL", '""');
const { outputText } = ts.transpileModule(clientSource, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const client = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const values = new Map();
const requests = [];
const dispatched = [];
let responseFactory;
const originalFetch = globalThis.fetch;
const originalStorage = globalThis.localStorage;
const originalWindow = globalThis.window;
const originalCustomEvent = globalThis.CustomEvent;
globalThis.localStorage = { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: (key) => values.delete(key) };
globalThis.window = { dispatchEvent: (event) => dispatched.push(event), setTimeout: (callback) => { callback(); return 1; } };
globalThis.CustomEvent = class { constructor(type, options) { this.type = type; this.detail = options?.detail; } };
globalThis.fetch = async (url, init) => { requests.push({ url, init }); return responseFactory(); };

test.after(() => {
  globalThis.fetch = originalFetch;
  globalThis.localStorage = originalStorage;
  globalThis.window = originalWindow;
  globalThis.CustomEvent = originalCustomEvent;
});

test("CSV download reuses authorization and sends applied filters without pagination", async () => {
  client.setStoredToken("local-test-token");
  responseFactory = () => new Response("\ufeffid,title\r\n1,example\r\n", { headers: { "Content-Type": "text/csv; charset=utf-8" } });
  const blob = await client.downloadBlob("/links/export.csv", { query: { groupId: "grp_example", keyword: "applied" } });
  const last = requests.at(-1);
  assert.equal(last.url, "/api/v1/links/export.csv?groupId=grp_example&keyword=applied");
  assert.equal(last.init.headers.get("Authorization"), "Bearer local-test-token");
  assert.equal(last.init.headers.get("X-Client-App"), "shortlink_web");
  assert.equal(await blob.text(), "id,title\r\n1,example\r\n");
});

test("CSV download parses JSON failures and does not produce a Blob", async () => {
  responseFactory = () => new Response(JSON.stringify({ error: { code: "export_limit_exceeded", message: "Narrow filters" } }), { status: 422, headers: { "Content-Type": "application/json" } });
  await assert.rejects(client.downloadBlob("/logs/access/export.csv"), (error) => error instanceof client.ApiError && error.status === 422 && error.code === "export_limit_exceeded");
});

test("JSON success cannot be downloaded as an apparent CSV", async () => {
  responseFactory = () => new Response(JSON.stringify({ error: { code: "not_csv", message: "Invalid download" } }), { headers: { "Content-Type": "application/json" } });
  await assert.rejects(client.downloadBlob("/links/export.csv"), (error) => error instanceof client.ApiError && error.code === "not_csv");
});

test("CSV unauthorized response clears the current session and emits an event", async () => {
  client.setStoredToken("expired-local-token");
  responseFactory = () => new Response(JSON.stringify({ error: { code: "unauthorized", message: "Expired" } }), { status: 401, headers: { "Content-Type": "application/json" } });
  await assert.rejects(client.downloadBlob("/links/export.csv"));
  assert.equal(client.getStoredToken(), null);
  assert.equal(dispatched.at(-1).type, "stub:unauthorized");
});

test("CSV HTML response is never saved as a file", async () => {
  responseFactory = () => new Response("<html>Sign in</html>", { headers: { "Content-Type": "text/html" } });
  await assert.rejects(client.downloadBlob("/links/export.csv"), (error) => error instanceof client.ApiError && error.code === "invalid_download");
});

test("CSV Content-Length is rejected before reading an oversize body", async () => {
  responseFactory = () => new Response("small", { headers: { "Content-Type": "text/csv", "Content-Length": String(20 * 1024 * 1024 + 1) } });
  await assert.rejects(client.downloadBlob("/links/export.csv"), (error) => error instanceof client.ApiError && /large|limit/.test(error.code));
});

test("CSV oversize response is rejected", async () => {
  responseFactory = () => new Response(new Uint8Array(20 * 1024 * 1024 + 1), { headers: { "Content-Type": "text/csv" } });
  await assert.rejects(client.downloadBlob("/links/export.csv"), (error) => error instanceof client.ApiError && /large|limit/.test(error.code));
});
