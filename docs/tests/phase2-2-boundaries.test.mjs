import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { setImmediate } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { compileFunction } from "node:vm";
import test from "node:test";
import ts from "../../../front/node_modules/typescript/lib/typescript.js";
import * as webForm from "../../../front/src/lib/link-form.ts";
import * as appForm from "../../../app/src/lib/link-form.ts";

const newYork = "America/New_York";
const now = Date.parse("2024-01-01T00:00:00Z");
const blankWindow = { activeFrom: "", activeUntil: "" };
const historicWindow = {
  activeFrom: "2023-06-01T13:15:07.987654Z",
  activeUntil: "2023-06-01T13:45:00.500Z",
  expiresAt: "2023-06-02T00:00:00Z",
};
const copyWindow = {
  activeFrom: "2024-06-01T09:15:07.987654-04:00",
  activeUntil: "2024-06-01T13:45:00.500Z",
  expiresAt: "2024-06-01T14:00:00.999999Z",
};

// Only synchronous test bodies use TZ; restore it even when an assertion fails.
function inTimeZone(zone, run) {
  const previous = process.env.TZ;
  process.env.TZ = zone;
  try { return run(); } finally {
    if (previous === undefined) delete process.env.TZ;
    else process.env.TZ = previous;
  }
}

function editableWindow(lib, source) {
  return { activeFrom: lib.localWindowTime(source.activeFrom), activeUntil: lib.localWindowTime(source.activeUntil) };
}

for (const [name, lib] of [["Web", webForm], ["App", appForm]]) {
  const { buildCreateWindow, buildWindowPatch, localWindowTime } = lib;

  test(`${name} window create and edit serialize local time using the applicable UTC offset`, () => {
    inTimeZone(newYork, () => {
      // The offset switches from -05:00 to -04:00 across this known DST transition.
      const form = { activeFrom: "2024-03-10T01:59:59.125", activeUntil: "2024-03-10T03:00:00.5" };
      const expected = { activeFrom: "2024-03-10T06:59:59.125Z", activeUntil: "2024-03-10T07:00:00.500Z" };
      assert.deepEqual(buildCreateWindow(form, null, null, now), expected);
      assert.deepEqual(buildWindowPatch({}, form, null, now), expected);
      assert.equal(localWindowTime("2024-06-01T15:20:07+02:00"), "2024-06-01T09:20:07");
    });
    inTimeZone("Asia/Kolkata", () => {
      const form = { activeFrom: "2024-02-29T00:10", activeUntil: "2024-02-29T08:15:06.042" };
      const expected = { activeFrom: "2024-02-28T18:40:00.000Z", activeUntil: "2024-02-29T02:45:06.042Z" };
      assert.deepEqual(buildCreateWindow(form, null, null, now), expected);
      assert.deepEqual(buildWindowPatch({}, form, null, now), expected);
    });
  });

  test(`${name} window create and edit reject invalid local dates, including the DST gap`, () => {
    inTimeZone(newYork, () => {
      for (const value of [
        "2024-02-30T12:00", "2024-13-01T12:00", "2024-06-01T24:00", "2024-06-01T12:60",
        "2024-06-01T12:30:00.1234", "2024-06-01T12:30-04:00", "2024-03-10T02:30",
      ]) {
        for (const field of ["activeFrom", "activeUntil"]) {
          const form = { ...blankWindow, [field]: value };
          assert.throws(() => buildCreateWindow(form, null, null, now), /无效.*本地日期时间/, `${field}: ${value}`);
          assert.throws(() => buildWindowPatch({}, form, null, now), /无效.*本地日期时间/, `${field}: ${value}`);
        }
      }
    });
  });

  test(`${name} window start must precede both the end and expiration`, () => {
    inTimeZone(newYork, () => {
      const activeFrom = "2024-06-01T10:15";
      for (const activeUntil of [activeFrom, "2024-06-01T10:14"]) {
        const form = { activeFrom, activeUntil };
        assert.throws(() => buildCreateWindow(form, null, null, now), /早于开放结束时间/);
        assert.throws(() => buildWindowPatch({}, form, null, now), /早于开放结束时间/);
      }
      for (const expiresAt of ["2024-06-01T14:15:00Z", "2024-06-01T16:14:00+02:00"]) {
        const form = { activeFrom, activeUntil: "2024-06-01T10:16" };
        assert.throws(() => buildCreateWindow(form, expiresAt, null, now), /早于短链到期时间/);
        assert.throws(() => buildWindowPatch({}, form, expiresAt, now), /早于短链到期时间/);
      }
      assert.throws(() => buildWindowPatch(copyWindow, editableWindow(lib, copyWindow), "2024-06-01T13:15:07.987Z", now), /早于短链到期时间/);

      // Expiration may be earlier than the window end; it does not replace that field.
      const form = { activeFrom, activeUntil: "2024-06-01T10:17" };
      const expiresAt = "2024-06-01T14:16:00Z";
      const expected = { activeFrom: "2024-06-01T14:15:00.000Z", activeUntil: "2024-06-01T14:17:00.000Z" };
      assert.deepEqual(buildCreateWindow(form, expiresAt, null, now), expected);
      assert.deepEqual(buildWindowPatch({}, form, expiresAt, now), expected);
    });
  });

  test(`${name} preserves ordering for stored window boundaries within one millisecond`, () => {
    inTimeZone(newYork, () => {
      const activeFrom = "2024-06-01T09:15:07.987654-04:00";
      const activeUntil = "2024-06-01T13:15:07.987655Z";
      const expiresAt = "2024-06-01T13:15:07.987656Z";
      const source = { activeFrom, activeUntil, expiresAt };
      const displayed = editableWindow(lib, source);
      const lastClockTickBeforeEnd = Date.parse("2024-06-01T13:15:07.987Z");
      assert.deepEqual(buildCreateWindow(displayed, expiresAt, source, lastClockTickBeforeEnd), { activeFrom, activeUntil });
      assert.throws(() => buildCreateWindow(displayed, expiresAt, source, lastClockTickBeforeEnd + 1), /晚于当前时间/);
      assert.deepEqual(buildWindowPatch(source, displayed, activeUntil, now), {});

      const equalToStart = "2024-06-01T13:15:07.987654Z";
      const equalWindow = { ...source, activeUntil: equalToStart };
      assert.throws(() => buildCreateWindow(editableWindow(lib, equalWindow), expiresAt, equalWindow, now), /早于开放结束时间/);
      assert.throws(() => buildWindowPatch(source, displayed, equalToStart, now), /早于短链到期时间/);
    });
  });

  test(`${name} window patch omits unchanged historical times and sends null for explicit clears`, () => {
    inTimeZone(newYork, () => {
      const displayed = editableWindow(lib, historicWindow);
      assert.deepEqual(buildWindowPatch(historicWindow, displayed, undefined, now), {});
      assert.deepEqual(buildWindowPatch(historicWindow, { ...displayed, activeFrom: "" }, undefined, now), { activeFrom: null });
      assert.deepEqual(buildWindowPatch(historicWindow, { ...displayed, activeUntil: "" }, undefined, now), { activeUntil: null });
      assert.deepEqual(buildWindowPatch(historicWindow, blankWindow, undefined, now), { activeFrom: null, activeUntil: null });
      assert.deepEqual(buildWindowPatch({ activeFrom: null }, blankWindow, undefined, now), {});
      // 19:00 EST is exactly `now`; a changed cutoff must be strictly later.
      for (const activeUntil of ["2023-06-01T09:46", "2023-12-31T19:00"]) {
        assert.throws(() => buildWindowPatch(historicWindow, { ...displayed, activeUntil }, undefined, now), /晚于当前时间/);
      }
    });
  });

  test(`${name} copy preserves original timestamp precision but requires clearing or moving an ended cutoff`, () => {
    inTimeZone(newYork, () => {
      const form = editableWindow(lib, copyWindow);
      const cutoff = Date.parse(copyWindow.activeUntil);
      const original = { activeFrom: copyWindow.activeFrom, activeUntil: copyWindow.activeUntil };
      assert.deepEqual(buildCreateWindow(form, copyWindow.expiresAt, copyWindow, cutoff - 1), original);
      for (const time of [cutoff, cutoff + 1]) {
        assert.throws(() => buildCreateWindow(form, copyWindow.expiresAt, copyWindow, time), /晚于当前时间/);
      }
      assert.deepEqual(buildCreateWindow({ ...form, activeUntil: "" }, copyWindow.expiresAt, copyWindow, cutoff + 1), {
        activeFrom: original.activeFrom, activeUntil: null,
      });
      assert.deepEqual(buildCreateWindow({ ...form, activeUntil: "2024-06-01T09:50:00.75" }, copyWindow.expiresAt, copyWindow, cutoff + 1), {
        activeFrom: original.activeFrom, activeUntil: "2024-06-01T13:50:00.750Z",
      });
    });
  });
}

test("Web editing another rule leaves a historical window alone unless explicitly cleared", () => {
  inTimeZone(newYork, () => {
    const source = { ...historicWindow, rules: { maxVisits: 1, allowedIpCidrs: [], requirePassword: false } };
    const expiresAt = webForm.localExpiration(source.expiresAt);
    const form = {
      expiresAt, ...editableWindow(webForm, source), maxVisits: "2", cidrs: "",
      requirePassword: false, password: "", passwordStrategy: "fixed_cooldown",
    };
    const clock = new Date(now);
    assert.deepEqual(webForm.buildRulesPatch(source, expiresAt, form, clock), { maxVisits: 2 });
    assert.deepEqual(webForm.buildRulesPatch(source, expiresAt, { ...form, activeUntil: "" }, clock), { maxVisits: 2, activeUntil: null });
  });
});

// As in the phase 2.1 tests, transpile the actual request boundary. Inject only
// platform/browser dependencies so every request goes through the real client.
async function compileSource(path) {
  const filename = new URL(`../../../${path}`, import.meta.url);
  const source = (await readFile(filename, "utf8")).replaceAll("import.meta.env.VITE_API_BASE_URL", '""');
  const { outputText } = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  });
  const execute = compileFunction(outputText, ["exports", "require", "fetch", "localStorage", "window", "document"], {
    filename: fileURLToPath(filename),
  });
  return (imports, { fetch, localStorage, window, document }) => {
    const exports = {};
    execute(exports, (specifier) => {
      assert.ok(Object.hasOwn(imports, specifier), `Unexpected runtime import ${specifier} in ${path}`);
      return imports[specifier];
    }, fetch, localStorage, window, document);
    return exports;
  };
}

async function compileFrontend(dir, name, appId) {
  const [client, notifications, hook] = await Promise.all([
    compileSource(`${dir}/src/services/api/client.ts`),
    compileSource(`${dir}/src/services/notification-api.ts`),
    compileSource(`${dir}/src/hooks/use-notifications.ts`),
  ]);
  return { name, appId, client, notifications, hook };
}

const [web, app, appApi] = await Promise.all([
  compileFrontend("front", "Web", "shortlink_web"),
  compileFrontend("app", "App", "shortlink_app"),
  compileSource("app/src/services/api/index.ts"),
]);

const token = "local-test-token";
const appPlatform = {
  "@capacitor/core": { Capacitor: { isNativePlatform: () => false } },
  "@/lib/format": { getServerBaseURL: () => assert.fail("Expected the App's relative API route in web mode") },
};

function offline(frontend, browser = {}) {
  const requests = [];
  const events = [];
  const values = new Map();
  const globals = {
    document: browser.document,
    window: { ...browser.window, dispatchEvent: (event) => { events.push({ type: event.type, detail: event.detail }); return true; } },
    localStorage: {
      getItem: (key) => values.get(key) ?? null,
      setItem: (key, value) => values.set(key, value),
      removeItem: (key) => values.delete(key),
    },
    fetch(url, init) {
      // Deliberately let callers complete even aborted requests to exercise late-response guards.
      const { promise, resolve } = Promise.withResolvers();
      requests.push({ url, init, reply: (data, status = 200) => resolve(status === 204 ? new Response(null, { status }) : Response.json(data, { status })) });
      return promise;
    },
  };
  const load = (module, imports) => module(imports, globals);
  const client = load(frontend.client, frontend === app ? appPlatform : {});
  client.setStoredToken(token);
  const notifications = load(frontend.notifications, { "./api/client": client });
  return {
    client, notifications, notificationsApi: notifications.notificationsApi, events, requests, load,
    take(url, method = "GET", expectedToken = token) {
      const request = requests.shift();
      assert.ok(request, `Expected ${method} ${url}`);
      assert.equal(request.url, url);
      assert.equal(request.init.method ?? "GET", method);
      assert.equal(request.init.headers.get("Authorization"), expectedToken ? `Bearer ${expectedToken}` : null);
      assert.equal(request.init.headers.get("X-Client-App"), frontend.appId);
      return request;
    },
  };
}

test("App sends the actual create request with UTC window fields and the selected expiry", async (t) => {
  t.mock.method(Date, "now", () => now);
  const payload = inTimeZone(newYork, () => appForm.buildCreateLinkRequest({
    ...appForm.initialCreateForm({ presetUrl: "https://example.test/delivery" }),
    title: " Delivery ", expireHours: "168", activeFrom: "2024-01-02T09:30:11.250", activeUntil: "2024-01-03T10:45",
  }));
  const http = offline(app);
  const { linksApi } = http.load(appApi, { ...appPlatform, "./client": http.client });
  const create = linksApi.create(payload);
  const request = http.take("/api/v1/links", "POST");
  assert.equal(request.init.headers.get("Content-Type"), "application/json");
  assert.deepEqual(JSON.parse(request.init.body), {
    title: "Delivery", tags: [], groupId: null, target: { type: "url", url: "https://example.test/delivery" },
    expiresAt: "2024-01-08T00:00:00.000Z", activeFrom: "2024-01-02T14:30:11.250Z", activeUntil: "2024-01-03T15:45:00.000Z",
    ruleSource: { type: "inline", rules: { maxVisits: -1, allowedIpCidrs: [], requirePassword: false } },
  });
  request.reply({ linkId: "new-link" }, 201);
  assert.deepEqual(await create, { linkId: "new-link" });
});

test("App copy form rejects an ended window and carries a cleared or changed cutoff into the create payload", (t) => {
  t.mock.method(Date, "now", () => Date.parse(copyWindow.activeUntil) + 1);
  inTimeZone(newYork, () => {
    const source = {
      ...copyWindow, target: { type: "url", url: "https://example.test/delivery" },
      rules: { maxVisits: 2, allowedIpCidrs: [], requirePassword: false },
    };
    const form = appForm.initialCreateForm({ source });
    assert.equal(form.activeUntil, appForm.localWindowTime(source.activeUntil));
    assert.throws(() => appForm.buildCreateLinkRequest(form), /晚于当前时间/);
    for (const [activeUntil, expected] of [["", null], ["2024-06-01T09:50", "2024-06-01T13:50:00.000Z"]]) {
      const payload = appForm.buildCreateLinkRequest({ ...form, activeUntil });
      assert.equal(payload.activeFrom, source.activeFrom);
      assert.equal(payload.activeUntil, expected);
      assert.equal(payload.expiresAt, source.expiresAt);
    }
  });
});

const listRoute = (page = 1, unread = false) => `/api/v1/notifications?page=${page}&pageSize=20${unread ? "&unread=true" : ""}`;
const countRoute = "/api/v1/notifications/unread-count";
const settingsRoute = "/api/v1/me/notification-settings";
const settings = { expiryEnabled: true, visitsEnabled: true };
const notification = (id) => ({
  notificationId: id, workspaceId: "workspace-test", linkId: "link-test", kind: "expiry_24h",
  title: id, summary: "Test reminder", createdAt: "2024-01-01T00:00:00Z", readAt: null,
});
const inboxPage = (page, items, total = items.length) => ({ items, pagination: { page, pageSize: 20, total } });
const unauthorized = { error: { code: "unauthorized", message: "Test session expired", requestId: "req-test" } };

for (const frontend of [web, app]) {
  const { name } = frontend;
  const check401 = (client) => (error) => error instanceof client.ApiError && error.status === 401 &&
    error.code === unauthorized.error.code && error.message === unauthorized.error.message && error.requestId === unauthorized.error.requestId;

  test(`${name} notification list uses default pagination and sends unread=true only when requested`, async () => {
    const http = offline(frontend);
    const all = http.notificationsApi.list();
    const first = inboxPage(1, [notification("first")], 22);
    http.take(listRoute()).reply(first);
    assert.deepEqual(await all, first);
    const { signal } = new AbortController();
    const unread = http.notificationsApi.list(2, true, signal);
    const request = http.take(listRoute(2, true));
    assert.equal(request.init.signal, signal);
    const second = inboxPage(2, [notification("older")], 21);
    request.reply(second);
    assert.deepEqual(await unread, second);
  });

  test(`${name} notification count and settings reads forward abort signals`, async () => {
    const http = offline(frontend);
    for (const [operation, route, response] of [["unreadCount", countRoute, { unreadCount: 3 }], ["settings", settingsRoute, settings]]) {
      const { signal } = new AbortController();
      const result = http.notificationsApi[operation](signal);
      const request = http.take(route);
      assert.equal(request.init.signal, signal);
      request.reply(response);
      assert.deepEqual(await result, response);
    }
  });

  test(`${name} notification read mutations use the encoded item PATCH and read-all POST`, async () => {
    const http = offline(frontend);
    const read = http.notificationsApi.markRead("note/雪? #");
    const item = http.take("/api/v1/notifications/note%2F%E9%9B%AA%3F%20%23/read", "PATCH");
    assert.equal(item.init.body, undefined);
    item.reply(undefined, 204);
    assert.equal(await read, undefined);
    const all = http.notificationsApi.readAll();
    const request = http.take("/api/v1/notifications/read-all", "POST");
    assert.equal(request.init.body, undefined);
    request.reply({ markedCount: 3 });
    assert.deepEqual(await all, { markedCount: 3 });
  });

  test(`${name} notification settings PATCH preserves explicit false without sending the other setting`, async () => {
    const http = offline(frontend);
    for (const field of ["expiryEnabled", "visitsEnabled"]) {
      const update = http.notificationsApi.updateSettings({ [field]: false });
      const request = http.take(settingsRoute, "PATCH");
      assert.equal(request.init.headers.get("Content-Type"), "application/json");
      assert.deepEqual(JSON.parse(request.init.body), { [field]: false });
      const saved = { ...settings, [field]: false };
      request.reply(saved);
      assert.deepEqual(await update, saved);
    }
  });

  test(`${name} notification 401 clears and signals only the session that sent the request`, async () => {
    const http = offline(frontend);
    const oldFailure = assert.rejects(http.notificationsApi.unreadCount(), check401(http.client));
    const old = http.take(countRoute);
    const newToken = "replacement-test-token";
    http.client.setStoredToken(newToken);
    old.reply(unauthorized, 401);
    await oldFailure;
    assert.equal(http.client.getStoredToken(), newToken);
    assert.deepEqual(http.events, []);

    const currentFailure = assert.rejects(http.notificationsApi.unreadCount(), check401(http.client));
    http.take(countRoute, "GET", newToken).reply(unauthorized, 401);
    await currentFailure;
    assert.equal(http.client.getStoredToken(), null);
    assert.deepEqual(http.events, [{ type: "stub:unauthorized", detail: unauthorized.error.message }]);
  });
}

// This is a hook runner, not a React renderer: preserve hook slots and dependency
// identities, commit effects after render, and explicitly rerender to read state.
function hookRunner() {
  const slots = [];
  const effects = [];
  let cursor = 0;
  const next = () => (slots[cursor++] ??= {});
  const changed = (slot, deps) => !slot.deps || slot.deps.length !== deps.length || deps.some((dep, i) => !Object.is(dep, slot.deps[i]));
  return {
    react: {
      useCallback(callback, deps) {
        const slot = next();
        if (changed(slot, deps)) Object.assign(slot, { callback, deps });
        return slot.callback;
      },
      useEffect(effect, deps) {
        const slot = next();
        if (changed(slot, deps)) effects.push(() => {
          slot.cleanup?.();
          slot.deps = deps;
          slot.cleanup = effect();
        });
      },
      useRef(value) { return (next().ref ??= { current: value }); },
      useState(initial) {
        const slot = next();
        if (!slot.set) {
          slot.value = typeof initial === "function" ? initial() : initial;
          slot.set = (value) => { slot.value = typeof value === "function" ? value(slot.value) : value; };
        }
        return [slot.value, slot.set];
      },
    },
    render(hook) {
      cursor = 0;
      const value = hook();
      for (const commit of effects.splice(0)) commit();
      return value;
    },
    unmount() { for (const slot of slots.splice(0)) slot.cleanup?.(); },
  };
}

function mountInbox(t, frontend, hidden = false) {
  const document = Object.assign(new EventTarget(), { hidden });
  const timers = new Map();
  let elapsed = 0;
  let nextId = 0;
  const window = {
    setTimeout(callback, delay) { timers.set(++nextId, { callback, due: elapsed + delay }); return nextId; },
    clearTimeout(id) { timers.delete(id); },
  };
  const http = offline(frontend, { document, window });
  const runner = hookRunner();
  const { useNotifications } = http.load(frontend.hook, {
    react: runner.react, "../services/api/client": http.client, "../services/notification-api": http.notifications,
  });
  let sessionToken = token;
  const render = () => runner.render(() => useNotifications(sessionToken));
  t.after(() => runner.unmount());
  render();
  return {
    ...http, render, timers,
    unmount: () => runner.unmount(),
    signIn(nextToken) { sessionToken = nextToken; http.client.setStoredToken(nextToken); render(); },
    setHidden(hidden) { document.hidden = hidden; document.dispatchEvent(new Event("visibilitychange")); },
    advance(ms) {
      elapsed += ms;
      for (const [id, { callback, due }] of [...timers]) {
        if (due <= elapsed && timers.delete(id)) callback();
      }
    },
  };
}

for (const frontend of [web, app]) {
  const { name } = frontend;

  test(`${name} notification polling pauses while hidden and resumes immediately, then waits one minute`, async (t) => {
    const inbox = mountInbox(t, frontend, true);
    inbox.advance(120_000);
    assert.equal(inbox.requests.length, 0);
    assert.equal(inbox.timers.size, 0);

    inbox.setHidden(false);
    inbox.take(countRoute).reply({ unreadCount: 3 });
    await setImmediate();
    assert.equal(inbox.render().unreadCount, 3);
    assert.equal(inbox.timers.size, 1);
    inbox.setHidden(true);
    assert.equal(inbox.timers.size, 0);
    inbox.advance(120_000);
    assert.equal(inbox.requests.length, 0);

    inbox.setHidden(false);
    inbox.take(countRoute).reply({ unreadCount: 5 });
    await setImmediate();
    inbox.advance(59_999);
    assert.equal(inbox.requests.length, 0);
    inbox.advance(1);
    const pending = inbox.take(countRoute);
    inbox.unmount();
    assert.equal(pending.init.signal.aborted, true);
    pending.reply({ unreadCount: 99 });
    await setImmediate();
    inbox.setHidden(false);
    inbox.advance(120_000);
    assert.equal(inbox.requests.length, 0);
    assert.equal(inbox.timers.size, 0);
  });

  test(`${name} notification refresh shares an in-flight poll and ignores an aborted count on resume`, async (t) => {
    const inbox = mountInbox(t, frontend);
    const beforeHide = inbox.take(countRoute);
    inbox.render().refresh(true);
    const list = inbox.take(listRoute(1, true));
    const preferences = inbox.take(settingsRoute);
    inbox.render().retry();
    inbox.advance(120_000);
    assert.equal(inbox.requests.length, 0, "no duplicate count, list, or settings request while pending");

    inbox.setHidden(true);
    assert.equal(beforeHide.init.signal.aborted, true);
    inbox.setHidden(false);
    inbox.take(countRoute).reply({ unreadCount: 4 });
    list.reply(inboxPage(1, []));
    preferences.reply(settings);
    await setImmediate();
    beforeHide.reply({ unreadCount: 99 });
    await setImmediate();
    assert.equal(inbox.render().unreadCount, 4);
    assert.equal(inbox.timers.size, 1, "the old poll must not restart its timer");
  });

  test(`${name} notification hook ignores late count, list, and settings responses from the previous token`, async (t) => {
    const inbox = mountInbox(t, frontend);
    const oldCount = inbox.take(countRoute);
    inbox.render().refresh(true);
    const oldPage = inbox.take(listRoute(1, true));
    const oldSettings = inbox.take(settingsRoute);

    const newToken = "next-hook-session";
    inbox.signIn(newToken);
    for (const request of [oldCount, oldPage, oldSettings]) assert.equal(request.init.signal.aborted, true);
    assert.deepEqual(inbox.render().items, []);
    assert.equal(inbox.render().settings, null);
    assert.equal(inbox.render().unreadCount, 0);

    inbox.take(countRoute, "GET", newToken).reply({ unreadCount: 2 });
    inbox.render().refresh(false);
    const freshItem = notification("new-session-item");
    const freshSettings = { expiryEnabled: false, visitsEnabled: true };
    inbox.take(listRoute(), "GET", newToken).reply(inboxPage(1, [freshItem]));
    inbox.take(settingsRoute, "GET", newToken).reply(freshSettings);
    await setImmediate();
    oldCount.reply({ unreadCount: 99 });
    oldPage.reply(inboxPage(1, [notification("old-session-item")]));
    oldSettings.reply({ expiryEnabled: true, visitsEnabled: false });
    await setImmediate();
    const current = inbox.render();
    assert.deepEqual(current.items, [freshItem]);
    assert.deepEqual(current.settings, freshSettings);
    assert.equal(current.unreadCount, 2);
    assert.equal(inbox.timers.size, 1);
  });

  test(`${name} notification retry keeps the unread filter and existing page while recovering failed requests`, async (t) => {
    const inbox = mountInbox(t, frontend);
    const failed = (request, message) => request.reply({ error: { code: "test_unavailable", message } }, 503);
    failed(inbox.take(countRoute), "Count temporarily unavailable");
    inbox.render().refresh(true);
    const first = Array.from({ length: 20 }, (_, index) => notification(`first-${index}`));
    inbox.take(listRoute(1, true)).reply(inboxPage(1, first, 21));
    failed(inbox.take(settingsRoute), "Settings temporarily unavailable");
    await setImmediate();
    assert.equal(inbox.render().hasMore, true);
    inbox.render().loadMore();
    const next = inbox.take(listRoute(2, true));
    inbox.render().loadMore();
    assert.equal(inbox.requests.length, 0, "a second loadMore must not duplicate the pending page");
    failed(next, "Page temporarily unavailable");
    await setImmediate();
    const previous = inbox.render();
    assert.deepEqual(previous.items, first);
    assert.equal(previous.page, 1);
    assert.equal(previous.error, "Page temporarily unavailable");
    assert.equal(previous.countError, "Count temporarily unavailable");
    assert.equal(previous.settingsError, "Settings temporarily unavailable");

    previous.retry();
    const last = notification("last");
    inbox.take(listRoute(2, true)).reply(inboxPage(2, [last], 21));
    inbox.take(settingsRoute).reply(settings);
    inbox.take(countRoute).reply({ unreadCount: 21 });
    await setImmediate();
    const current = inbox.render();
    assert.deepEqual(current.items, [...first, last]);
    assert.equal(current.page, 2);
    assert.equal(current.hasMore, false);
    assert.equal(current.loading, false);
    assert.equal(current.unreadCount, 21);
    assert.deepEqual(current.settings, settings);
    assert.deepEqual([current.error, current.countError, current.settingsError], ["", "", ""]);
  });
}
