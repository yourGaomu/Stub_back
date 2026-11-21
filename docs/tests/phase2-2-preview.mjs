// Phase 2.2 GUI acceptance only. Build front/dist and app/dist first, with
// same-origin API settings (or the exact loopback origins below). Use fresh site
// storage or leave the App's custom server unset. No backend or public /r resolver.
// Launch: node back/docs/tests/phase2-2-preview.mjs
// Web: http://127.0.0.1:43122/login   App: http://127.0.0.1:43123/
// Login on both: phase22-preview@example.test / Phase22-Local-Only!
//
// Verify requests in the GUI Network panel or via the sanitized JSON console log:
// - First links page: 20 of 21; Scheduled Window is not_started and Ended Window
//   is window_ended. Open Expiry Reminder in the inbox: PATCH .../read is 204,
//   then GET /links/:linkId opens Off-page Notice Link (initially on page 2).
//   Unread count goes 2 -> 1; read-all returns markedCount: 1 and count becomes 0.
//   Turn off only expiry: PATCH settings sends only expiryEnabled:false.
//   GET settings returns expiryEnabled:false and visitsEnabled:true after a reload.
// - Try copying Protected Copy Source: its group, bounds and rules are prefilled,
//   but the password is blank. Submitting as-is must not send POST /links.
//   Use a new dummy value (e.g. Phase22-Copy-Only!) to create. The POST carries
//   the original window values including fractional seconds, requirePassword:true,
//   and a new password; the result has a different ID/code and visitsUsed:0.
// - Before or after copying, clear only the source's end time: PATCH /rules
//   should be exactly {"activeUntil":null}; the start, expiration and rules stay
//   intact. An unchanged save sends no PATCH. A changed local time is sent as UTC.
// - A fresh URL create with blank window sends both activeFrom and activeUntil
//   as null. Use https://example.test/phase22/new as an inert target.
// - Mutations on either port never affect the other. Restart to reset both.

import { randomUUID } from 'node:crypto';
import { once } from 'node:events';
import { readFile, realpath, stat } from 'node:fs/promises';
import { createServer } from 'node:http';
import { extname, isAbsolute, relative, resolve, sep } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HOST = '127.0.0.1';
const API = '/api/v1';
const ROOT = fileURLToPath(new URL('../../../', import.meta.url));
const EMAIL = 'phase22-preview@example.test';
const PASSWORD = 'Phase22-Local-Only!';
const MINUTE = 60_000;
const DAY = 24 * 60 * MINUTE;
const iso = (ms) => new Date(ms).toISOString();
const id = (prefix, n) => `${prefix}_00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const WS = id('ws', 1);
const GROUP = id('grp', 1);
const OFF_PAGE = id('link', 21);
const GROUP_NAME = 'Phase 2.2 Preview Group';
const TIME_FIELDS = ['expiresAt', 'activeFrom', 'activeUntil'];
const RULE_FIELDS = ['maxVisits', 'allowedIpCidrs', 'requirePassword', 'wrongPasswordPolicy'];
const STRATEGIES = ['fixed_cooldown', 'progressive_cooldown'];
const AVAILABILITY = ['available', 'not_started', 'window_ended', 'expired', 'disabled', 'visit_limit_reached'];
const PAGINATION = ['page', 'pageSize'];
const PREVIEWS = [
  { label: 'Web', dir: 'front', clientAppId: 'shortlink_web', port: 43122 },
  { label: 'App', dir: 'app', clientAppId: 'shortlink_app', port: 43123 },
];

class PreviewError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

function expect(condition, message) {
  if (!condition) throw new PreviewError(422, 'invalid_preview_payload', message);
}

function fields(value, allowed) {
  expect(value && typeof value === 'object' && !Array.isArray(value), 'Expected a JSON object.');
  expect(Object.keys(value).every((key) => allowed.includes(key)), 'Unexpected JSON field for this preview route.');
}

async function readJSON(req) {
  expect(/^application\/json(?:\s*;|$)/i.test(req.headers['content-type'] || ''), 'Expected application/json.');
  const limit = 32 * 1024;
  const tooLarge = () => new PreviewError(413, 'preview_body_too_large', 'Preview JSON body exceeds 32 KiB.');
  if (Number(req.headers['content-length']) > limit) throw tooLarge();
  const chunks = [];
  let size = 0;
  for await (const chunk of req.iterator({ destroyOnReturn: false })) {
    size += chunk.length;
    if (size > limit) throw tooLarge();
    chunks.push(chunk);
  }
  try { return JSON.parse(Buffer.concat(chunks).toString('utf8')); }
  catch { throw new PreviewError(422, 'invalid_preview_json', 'Expected valid JSON.'); }
}

function queryFields(query, allowed) {
  const seen = new Set();
  for (const key of query.keys()) {
    expect(allowed.includes(key) && !seen.has(key), 'Unexpected or repeated query parameter.');
    seen.add(key);
  }
}

function paged(items, query) {
  const rawPage = query.get('page') ?? '1';
  const rawSize = query.get('pageSize') ?? '20';
  const page = Number(rawPage), pageSize = Number(rawSize);
  expect(/^\d+$/.test(rawPage) && Number.isSafeInteger(page) && page >= 1 &&
    /^\d+$/.test(rawSize) && pageSize >= 1 && pageSize <= 100, 'Invalid page or pageSize.');
  const total = items.length;
  const offset = (page - 1) * pageSize;
  return { items: items.slice(offset, offset + pageSize), pagination: { page, pageSize, total } };
}

function seed(now) {
  const base = Math.floor(now / MINUTE) * MINUTE;
  const at = (delta) => iso(base + delta);
  const fixture = (n, code, title, options = {}) => ({
    linkId: id('link', n), code, title, tags: ['phase-2.2'], groupId: null,
    target: { type: 'url', url: `https://example.test/phase22/${code}` },
    status: 'enabled', activeFrom: null, activeUntil: null, expiresAt: null,
    visitsUsed: 0, rules: { maxVisits: -1, allowedIpCidrs: [], requirePassword: false },
    ruleTemplate: null, createdAt: at(-n * MINUTE), updatedAt: at(-n * MINUTE), ...options,
  });
  const links = [
    fixture(1, 'p22-not-started', 'Phase 2.2 Scheduled Window', {
      activeFrom: at(2 * DAY), activeUntil: at(5 * DAY), expiresAt: at(30 * DAY),
    }),
    fixture(2, 'p22-window-ended', 'Phase 2.2 Ended Window', {
      activeFrom: at(-3 * DAY), activeUntil: at(-DAY), expiresAt: at(30 * DAY),
    }),
    fixture(3, 'p22-protected-copy', 'Phase 2.2 Protected Copy Source', {
      groupId: GROUP, tags: ['phase-2.2', 'protected-copy'], visitsUsed: 8,
      activeFrom: at(-DAY + 17_123), activeUntil: at(14 * DAY + 29_456), expiresAt: at(30 * DAY + 53_789),
      // Protection is metadata only: no password, hash, or public resolver exists.
      rules: { maxVisits: 10, allowedIpCidrs: ['192.0.2.0/24'], requirePassword: true,
        wrongPasswordPolicy: { strategy: 'progressive_cooldown' } },
    }),
    ...Array.from({ length: 17 }, (_, i) => {
      const number = String(i + 1).padStart(2, '0');
      return fixture(i + 4, `p22-sample-${number}`, `Phase 2.2 Sample ${number}`);
    }),
    fixture(21, 'p22-notice-page-two', 'Phase 2.2 Off-page Notice Link', { expiresAt: at(DAY / 2) }),
  ];
  const notice = (n, linkId, kind, title, summary, readAt = null) => ({
    notificationId: id('ntf', n), workspaceId: WS, linkId, kind, title, summary,
    createdAt: at(-n * MINUTE), readAt,
  });
  const notifications = [
    notice(1, OFF_PAGE, 'expiry_24h', 'Phase 2.2 Expiry Reminder', 'Phase 2.2 Off-page Notice Link expires within 24 hours.'),
    notice(2, id('link', 3), 'visits_80', 'Phase 2.2 Visits Reminder', 'Phase 2.2 Protected Copy Source has used 8 of 10 visits.'),
    notice(3, null, 'expiry_24h', 'Phase 2.2 Read Reminder', 'Previously read synthetic reminder.', at(-MINUTE)),
  ];
  return {
    links: new Map(links.map((link) => [link.linkId, link])),
    notifications: new Map(notifications.map((item) => [item.notificationId, item])),
    settings: { expiryEnabled: true, visitsEnabled: true },
    user: { userId: id('usr', 1), email: EMAIL, role: 'user', defaultWorkspaceId: WS,
      invitedByUserId: null, createdAt: at(-31 * DAY), emailVerifiedAt: at(-31 * DAY) },
    group: { groupId: GROUP, workspaceId: WS, name: GROUP_NAME, description: 'Synthetic preview group',
      createdAt: at(-30 * DAY), updatedAt: at(-30 * DAY) },
  };
}

function availability(link, now) {
  if (link.status === 'disabled') return 'disabled';
  if (link.expiresAt && Date.parse(link.expiresAt) <= now) return 'expired';
  if (link.activeUntil && Date.parse(link.activeUntil) <= now) return 'window_ended';
  if (link.activeFrom && Date.parse(link.activeFrom) > now) return 'not_started';
  if (link.rules.maxVisits >= 0 && link.visitsUsed >= link.rules.maxVisits) return 'visit_limit_reached';
  return 'available';
}

// The GUI sends UTC ISO timestamps. Keep their exact wire precision in memory.
function isTimestamp(value) {
  const match = typeof value === 'string' && /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.\d{1,9})?Z$/.exec(value);
  const time = match && Date.parse(value);
  return Boolean(match && Number.isFinite(time) && iso(time).startsWith(match[1]));
}

function windowAfter(input, previous, now) {
  const window = {};
  for (const key of TIME_FIELDS) {
    const sent = Object.hasOwn(input, key);
    const value = sent ? input[key] : previous?.[key] ?? null;
    expect(value === null || isTimestamp(value), `${key} must be a UTC ISO timestamp or null.`);
    if (sent && value !== null && key !== 'activeFrom') expect(Date.parse(value) > now, `${key} must be in the future.`);
    window[key] = value;
  }
  if (window.activeFrom) {
    const start = Date.parse(window.activeFrom);
    expect(['activeUntil', 'expiresAt'].every((key) => !window[key] || start < Date.parse(window[key])),
      'activeFrom must be earlier than both deadlines.');
  }
  return window;
}

function rulesAfter(input, body, previous) {
  const maxVisits = Object.hasOwn(input, 'maxVisits') ? input.maxVisits : previous?.maxVisits ?? -1;
  const allowedIpCidrs = Object.hasOwn(input, 'allowedIpCidrs') ? input.allowedIpCidrs : previous?.allowedIpCidrs ?? [];
  const requirePassword = Object.hasOwn(input, 'requirePassword') ? input.requirePassword : previous?.requirePassword ?? false;
  expect(Number.isSafeInteger(maxVisits) && maxVisits >= -1, 'maxVisits must be a safe integer >= -1.');
  expect(Array.isArray(allowedIpCidrs) && allowedIpCidrs.every((item) => typeof item === 'string' && item.trim()) &&
    allowedIpCidrs.join(',').length <= 8192, 'allowedIpCidrs must be an array of non-empty strings.');
  expect(typeof requirePassword === 'boolean', 'requirePassword must be a boolean.');
  const result = { maxVisits, allowedIpCidrs, requirePassword };
  const sentPassword = Object.hasOwn(body, 'accessPassword');
  const sentPolicy = Object.hasOwn(input, 'wrongPasswordPolicy');
  if (requirePassword) {
    if (sentPassword || !previous?.requirePassword) {
      expect(typeof body.accessPassword === 'string' && body.accessPassword.trim() && Buffer.byteLength(body.accessPassword) <= 1024,
        'A new non-blank access password is required (up to 1024 bytes).');
    }
    if (sentPolicy) {
      fields(input.wrongPasswordPolicy, ['strategy']);
      expect(STRATEGIES.includes(input.wrongPasswordPolicy.strategy), 'Unknown password policy.');
    }
    result.wrongPasswordPolicy = { strategy: input.wrongPasswordPolicy?.strategy ?? previous?.wrongPasswordPolicy?.strategy ?? 'fixed_cooldown' };
  } else {
    expect(!sentPassword && !sentPolicy, 'Password and policy must be omitted when protection is off.');
  }
  return result;
}

// Diagnostics are constructed from allowlisted fields only. No raw request URL,
// query values (except checked enums/numbers), headers, text, passwords or tokens.
function timeTrace(body) {
  return Object.fromEntries(TIME_FIELDS.map((key) => [key, !Object.hasOwn(body, key) ? '<omitted>' :
    body[key] === null || isTimestamp(body[key]) ? body[key] : '<invalid>']));
}

function rulesTrace(rules) {
  return {
    maxVisits: Object.hasOwn(rules, 'maxVisits') ? Number.isSafeInteger(rules.maxVisits) ? rules.maxVisits : '<invalid>' : '<omitted>',
    allowedIpCidrCount: Object.hasOwn(rules, 'allowedIpCidrs') ? Array.isArray(rules.allowedIpCidrs) ? rules.allowedIpCidrs.length : '<invalid>' : '<omitted>',
    requirePassword: Object.hasOwn(rules, 'requirePassword') ? typeof rules.requirePassword === 'boolean' ? rules.requirePassword : '<invalid>' : '<omitted>',
    passwordStrategy: Object.hasOwn(rules, 'wrongPasswordPolicy')
      ? STRATEGIES.includes(rules.wrongPasswordPolicy?.strategy) ? rules.wrongPasswordPolicy.strategy : '<invalid>' : '<omitted>',
  };
}

function makeApi(config, origin, seededAt) {
  const { links, notifications, settings, user, group } = seed(seededAt);
  const sessions = new Map();
  let nextSession = 1;
  let nextLink = links.size + 1;
  const unreadCount = () => [...notifications.values()].filter((item) => item.readAt === null).length;
  const lookup = (items, key) => {
    const item = items.get(key);
    if (!item) throw new PreviewError(404, 'preview_fixture_not_found', 'No such resource in this preview.');
    return item;
  };
  const detail = (link, now) => ({ ...link, groupName: link.groupId ? GROUP_NAME : null,
    url: `${origin}/r/${link.code}`, availability: availability(link, now) });
  const row = (link, now) => {
    const { target, rules, ruleTemplate, updatedAt, ...item } = detail(link, now);
    return { ...item, targetType: target.type, targetUrl: target.url, maxVisits: rules.maxVisits };
  };
  const routes = [];
  const uuid = '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}';
  const route = (method, path, handler, options = {}) => routes.push({
    method, path: API + path, handler,
    pattern: new RegExp(`^${API}${path.replace(':linkId', `(link_${uuid})`).replace(':notificationId', `(ntf_${uuid})`)}$`),
    ...options,
  });
  const pageRoute = (path, handler, keys = []) => route('GET', path, handler, { query: [...PAGINATION, ...keys] });

  route('POST', '/auth/login', ({ body, now }) => {
    fields(body, ['email', 'password', 'deviceInstanceId', 'deviceName', 'platform']);
    if (typeof body.email !== 'string' || body.email.trim().toLowerCase() !== EMAIL || body.password !== PASSWORD) {
      throw new PreviewError(401, 'invalid_credentials', 'Use the synthetic phase 2.2 preview credentials.');
    }
    const accessToken = `preview-${config.port}-${randomUUID()}`;
    const expiresAt = iso(now + 7 * DAY);
    const session = { sessionId: id('ses', nextSession++), clientAppId: config.clientAppId,
      deviceName: `Phase 2.2 ${config.label}`, platform: config.label === 'Web' ? 'Web' : 'Mobile',
      createdAt: iso(now), lastSeenAt: iso(now), expiresAt, revokedAt: null, isCurrent: true, online: true };
    sessions.set(accessToken, session);
    return { accessToken, tokenType: 'Bearer', expiresAt, user, session };
  }, { public: true, json: true });
  route('POST', '/auth/logout', ({ token }) => { sessions.delete(token); }, { status: 204 });
  route('GET', '/me', () => user);
  route('GET', '/me/permissions', () => ({ isAdmin: false, items: [
    'links.read', 'links.create_url', 'links.create_asset', 'links.manage', 'assets.read', 'templates.read_use', 'logs.read_own',
  ].map((code) => ({ code, enabled: true })) }));
  route('GET', '/me/businesses', () => ({ items: [] }));
  route('GET', '/me/usage', () => ({
    storage: { maxSingleFileBytes: 10_000_000, quotaBytes: 100_000_000, usedBytes: 0, reservedBytes: 0, remainingBytes: 100_000_000 },
    urlLinks: { used: links.size, limit: 100, remaining: Math.max(0, 100 - links.size) },
    minioAssetLinks: { used: 0, limit: 50, remaining: 50 }, apiKeys: { used: 0, limit: 5, remaining: 5 },
  }));
  route('POST', '/me/sessions/heartbeat', ({ session, now }) => { session.lastSeenAt = iso(now); return session; });
  pageRoute('/me/sessions', ({ query, token, now }) => paged([...sessions.entries()]
    .filter(([, item]) => Date.parse(item.expiresAt) > now)
    .reverse().map(([key, item]) => ({ ...item, isCurrent: key === token })), query));

  pageRoute('/link-groups', ({ query }) => paged(!query.has('workspaceId') || query.get('workspaceId') === WS
    ? [{ ...group, linkCount: [...links.values()].filter((link) => link.groupId === GROUP).length }] : [], query), ['workspaceId']);
  // Assets/templates intentionally have no options. App Stats and Me also fetch these empty lists.
  for (const [path, keys] of [
    ['/assets', ['businessId', 'workspaceId', 'ownerUserId', 'keyword', 'includeDeleted']],
    ['/rule-templates', ['workspaceId', 'status', 'includeDeleted', 'keyword']],
    ['/logs/access', ['linkId', 'groupId', 'ungrouped', 'from', 'to', 'result', 'reason', 'workspaceId', 'businessId', 'linkOwnerUserId']],
    ['/logs/audit', ['workspaceId', 'actorUserId', 'businessId', 'resourceType', 'resourceId', 'action', 'result', 'from', 'to']],
    ['/invites', ['status', 'createdByUserId']],
    ['/api-keys', ['status', 'businessId']],
  ]) pageRoute(path, ({ query }) => paged([], query), keys);

  function analytics({ query, now }, link) {
    const range = query.get('range') ?? '7d';
    expect(['24h', '7d', '30d'].includes(range), 'Unexpected analytics range.');
    const timeZone = query.get('timeZone') ?? 'Asia/Shanghai';
    try { new Intl.DateTimeFormat('en', { timeZone }); }
    catch { throw new PreviewError(422, 'invalid_preview_query', 'Invalid analytics timeZone.'); }
    const failureReasons = [
      ...(link && link.linkId !== id('link', 1) ? [] : [{ reason: 'not_started', count: 2 }]),
      ...(link && link.linkId !== id('link', 2) ? [] : [{ reason: 'window_ended', count: 1 }]),
    ];
    const failureCount = failureReasons.reduce((sum, item) => sum + item.count, 0);
    const redirectCount = link?.visitsUsed ?? 8;
    const visitAttemptCount = redirectCount + failureCount;
    const totals = { requestCount: visitAttemptCount, visitAttemptCount, redirectCount, failureCount };
    return { range, timeZone, from: iso(now - ({ '24h': 1, '7d': 7, '30d': 30 }[range] * DAY)), to: iso(now),
      totals, failureReasons, series: [{ at: iso(now - 60 * MINUTE), ...totals }] };
  }
  route('GET', '/analytics/summary', (context) => analytics(context), { query: ['range', 'timeZone', 'workspaceId'] });
  route('GET', '/analytics/links/:linkId', (context) => {
    const link = lookup(links, context.resourceId);
    context.trace.linkId = link.linkId;
    return analytics(context, link);
  }, { query: ['range', 'timeZone'] });

  pageRoute('/links', ({ query, now, trace }) => {
    const q = Object.fromEntries(query);
    expect(!q.availability || AVAILABILITY.includes(q.availability), 'Unexpected availability filter.');
    expect(!q.status || ['enabled', 'disabled'].includes(q.status), 'Unexpected status filter.');
    expect(!q.targetType || ['url', 'minio_asset'].includes(q.targetType), 'Unexpected targetType filter.');
    expect(!q.ungrouped || ['true', 'false'].includes(q.ungrouped), 'Unexpected ungrouped filter.');
    expect(!q.groupId || q.ungrouped !== 'true', 'groupId cannot be combined with ungrouped.');
    expect(!q.maxVisits || (/^-?\d+$/.test(q.maxVisits) && Number(q.maxVisits) >= -1 && Number.isSafeInteger(Number(q.maxVisits))),
      'Unexpected maxVisits filter.');
    expect([q.createdFrom, q.createdTo].every((value) => !value || isTimestamp(value)), 'Invalid creation-time filter.');
    const keyword = q.keyword?.toLowerCase();
    const matches = [...links.values()].reverse().sort((a, b) => b.createdAt.localeCompare(a.createdAt)).filter((item) =>
      (!q.workspaceId || q.workspaceId === WS) && (!q.availability || availability(item, now) === q.availability) &&
      (!q.status || item.status === q.status) && (!q.targetType || item.target.type === q.targetType) &&
      (!q.groupId || item.groupId === q.groupId) && (q.ungrouped !== 'true' || item.groupId === null) &&
      (!q.code || item.code === q.code) && (!keyword || [item.title, item.code, ...item.tags].some((s) => s.toLowerCase().includes(keyword))) &&
      !q.assetId && (!q.maxVisits || Number(q.maxVisits) === item.rules.maxVisits) &&
      (!q.createdFrom || Date.parse(item.createdAt) >= Date.parse(q.createdFrom)) &&
      (!q.createdTo || Date.parse(item.createdAt) <= Date.parse(q.createdTo)));
    const page = paged(matches, query);
    trace.result = { ...page.pagination, returned: page.items.length, availabilityFilter: q.availability || null,
      includesOffPageFixture: page.items.some((item) => item.linkId === OFF_PAGE) };
    return { items: page.items.map((link) => row(link, now)), pagination: page.pagination };
  }, ['workspaceId', 'availability', 'status', 'targetType', 'groupId', 'ungrouped', 'code', 'keyword', 'assetId', 'maxVisits', 'createdFrom', 'createdTo']);

  route('GET', '/links/:linkId', ({ resourceId, now, trace }) => {
    const link = lookup(links, resourceId);
    trace.linkId = link.linkId;
    return detail(link, now);
  });
  route('POST', '/links', ({ body, now, trace }) => {
    fields(body, ['groupId', 'title', 'tags', 'code', 'target', 'ruleSource', 'accessPassword', ...TIME_FIELDS]);
    const requestedRules = body.ruleSource?.rules;
    trace.request = { ...timeTrace(body), ...rulesTrace(requestedRules ?? {}),
      groupId: !Object.hasOwn(body, 'groupId') ? '<omitted>' : body.groupId === GROUP || body.groupId === null ? body.groupId : '<invalid>',
      customCodeSent: Object.hasOwn(body, 'code'), accessPasswordSent: Object.hasOwn(body, 'accessPassword') };
    fields(body.target, ['type', 'url']);
    expect(body.target.type === 'url' && typeof body.target.url === 'string' && body.target.url.length <= 8192 &&
      /^https:\/\//i.test(body.target.url) && !/\s/.test(body.target.url), 'Preview create only supports HTTPS URL targets.');
    let target;
    try { target = new URL(body.target.url); } catch { /* handled by the assertion below */ }
    expect(target?.hostname && !target.username && !target.password, 'Use a valid HTTPS target without credentials.');
    fields(body.ruleSource, ['type', 'rules']);
    expect(body.ruleSource.type === 'inline', 'Preview create only supports inline rules.');
    fields(requestedRules, RULE_FIELDS);
    expect(!Object.hasOwn(body, 'groupId') || body.groupId === null || body.groupId === GROUP, 'Unknown preview group.');
    const title = Object.hasOwn(body, 'title') ? body.title : '';
    const tags = Object.hasOwn(body, 'tags') ? body.tags : [];
    expect(typeof title === 'string' && [...title].length <= 120, 'Invalid link title.');
    expect(Array.isArray(tags) && tags.length <= 20 && tags.every((tag) => typeof tag === 'string' && [...tag].length <= 32), 'Invalid link tags.');
    expect(!Object.hasOwn(body, 'code') || (typeof body.code === 'string' && /^[a-z0-9-]{4,64}$/.test(body.code)), 'Invalid custom short code.');
    const window = windowAfter(body, null, now);
    const rules = rulesAfter(requestedRules, body, null);
    const usedCodes = new Set([...links.values()].map((link) => link.code));
    if (usedCodes.has(body.code)) throw new PreviewError(409, 'code_conflict', 'That short code already exists in this preview.');
    while (!body.code && usedCodes.has(`p22-new-${nextLink}`)) nextLink++;
    const number = nextLink++;
    const link = { linkId: id('link', number), code: body.code ?? `p22-new-${number}`, title, tags,
      groupId: body.groupId ?? null, target: { type: 'url', url: body.target.url }, ...window, rules,
      status: 'enabled', ruleTemplate: null, visitsUsed: 0, createdAt: iso(now), updatedAt: iso(now) };
    links.set(link.linkId, link);
    trace.linkId = link.linkId;
    trace.result = { availability: availability(link, now), visitsUsed: 0 };
    return detail(link, now);
  }, { json: true, status: 201 });
  route('PATCH', '/links/:linkId/rules', ({ resourceId, body, now, trace }) => {
    const link = lookup(links, resourceId);
    trace.linkId = link.linkId;
    fields(body, [...TIME_FIELDS, ...RULE_FIELDS, 'accessPassword']);
    expect(Object.keys(body).length > 0, 'Send only changed rules or time boundaries.');
    trace.request = { ...timeTrace(body), ...rulesTrace(body), accessPasswordSent: Object.hasOwn(body, 'accessPassword') };
    const window = windowAfter(body, link, now);
    const rules = rulesAfter(body, body, link.rules);
    const updated = { ...link, ...window, rules, updatedAt: iso(now) };
    links.set(link.linkId, updated);
    trace.result = { ...window, ...rulesTrace(rules), availability: availability(updated, now) };
    return detail(updated, now);
  }, { json: true });

  route('GET', '/notifications/unread-count', ({ trace }) => (trace.result = { unreadCount: unreadCount() }));
  pageRoute('/notifications', ({ query, trace }) => {
    const unread = query.get('unread');
    expect(unread === null || unread === 'true' || unread === 'false', 'unread must be true or false.');
    const page = paged([...notifications.values()].filter((item) => unread !== 'true' || item.readAt === null), query);
    trace.result = { unreadOnly: unread === 'true', ...page.pagination, returned: page.items.length };
    return page;
  }, ['unread']);
  route('PATCH', '/notifications/:notificationId/read', ({ resourceId, now, trace }) => {
    const notice = lookup(notifications, resourceId);
    trace.notificationId = notice.notificationId;
    trace.linkId = notice.linkId;
    notice.readAt ??= iso(now);
    trace.result = { unreadCount: unreadCount() };
  }, { status: 204 });
  route('POST', '/notifications/read-all', ({ now, trace }) => {
    const markedCount = unreadCount();
    for (const notice of notifications.values()) notice.readAt ??= iso(now);
    trace.result = { markedCount, unreadCount: 0 };
    return { markedCount };
  });
  route('GET', '/me/notification-settings', ({ trace }) => (trace.result = { ...settings }));
  route('PATCH', '/me/notification-settings', ({ body, trace }) => {
    fields(body, ['expiryEnabled', 'visitsEnabled']);
    expect(Object.keys(body).length > 0 && Object.values(body).every((value) => typeof value === 'boolean'), 'Settings PATCH requires one or two booleans.');
    trace.request = { ...body };
    Object.assign(settings, body);
    return (trace.result = { ...settings });
  }, { json: true });

  return async (req, path, query, trace) => {
    const matching = routes.filter((entry) => entry.pattern.test(path));
    const endpoint = matching.find((entry) => entry.method === req.method);
    if (!endpoint) {
      if (matching.length) {
        trace.route = matching[0].path;
        throw new PreviewError(405, 'unexpected_preview_method', 'This API method is not enabled in the phase 2.2 preview.');
      }
      throw new PreviewError(404, 'unexpected_preview_route', 'No such API route in the phase 2.2 preview (no proxy).');
    }
    trace.route = endpoint.path;
    queryFields(query, endpoint.query ?? []);
    if (req.headers['x-client-app'] !== undefined) {
      expect(req.headers['x-client-app'] === config.clientAppId, 'Use the matching Web or App preview port.');
    }
    let token, session;
    const now = Date.now();
    if (!endpoint.public) {
      token = /^Bearer (\S+)$/.exec(req.headers.authorization || '')?.[1];
      session = sessions.get(token);
      if (!session || Date.parse(session.expiresAt) <= now) {
        throw new PreviewError(401, 'unauthorized', 'Sign in to this preview port.');
      }
    }
    if (!endpoint.json) {
      expect(req.headers['transfer-encoding'] === undefined && Number(req.headers['content-length'] ?? 0) === 0,
        'This preview route does not accept a body.');
    }
    const body = endpoint.json ? await readJSON(req) : undefined;
    const resourceId = endpoint.pattern.exec(path)[1];
    return { status: endpoint.status ?? 200, data: endpoint.handler({ body, query, trace, now, token, session, resourceId }) };
  };
}

const MIME = new Map([
  ['.js', 'text/javascript; charset=utf-8'], ['.mjs', 'text/javascript; charset=utf-8'], ['.css', 'text/css; charset=utf-8'],
  ['.json', 'application/json; charset=utf-8'], ['.svg', 'image/svg+xml'], ['.ico', 'image/x-icon'],
  ['.png', 'image/png'], ['.jpg', 'image/jpeg'], ['.jpeg', 'image/jpeg'], ['.webp', 'image/webp'], ['.woff2', 'font/woff2'],
]);
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; font-src 'self' data:; img-src 'self' data: blob:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'";
const WEB_PAGES = new Set(['/', '/index.html', '/login', '/register', '/reset-password', '/console', '/console/links',
  '/console/account', '/console/assets', '/console/templates', '/console/logs', '/console/invites', '/console/api-keys']);
const APP_PAGES = new Set(['/', '/index.html']);

function send(res, status, contentType, body, head = false) {
  res.statusCode = status;
  if (status === 204) return res.end();
  res.setHeader('Content-Type', contentType);
  res.setHeader('Content-Length', Buffer.byteLength(body));
  res.end(head ? undefined : body);
}

// Exported to support contract checks without opening a listening socket.
export async function preparePreview(config, seededAt = Date.now(), log = (line) => console.log(line)) {
  const authority = `${HOST}:${config.port}`;
  const origin = `http://${authority}`;
  const dist = resolve(ROOT, config.dir, 'dist');
  let dir, index;
  async function staticFile(path) {
    const file = await realpath(resolve(dir, path));
    const rel = relative(dir, file);
    if (isAbsolute(rel) || rel === '..' || rel.startsWith(`..${sep}`) || !(await stat(file)).isFile()) {
      throw new PreviewError(404, 'preview_not_found', 'No such preview file.');
    }
    return readFile(file);
  }
  try {
    dir = await realpath(dist);
    const html = (await staticFile('index.html')).toString('utf8');
    // Remove external stylesheet/preconnect links from the response, not dist.
    index = html.replace(/<link\b[^>]*\shref\s*=\s*["'](?:https?:)?\/\/[^>]*>/gi, '');
  } catch {
    throw new Error(`${config.label} build missing or unreadable: ${resolve(dist, 'index.html')}. Build it before launch.`);
  }
  const api = makeApi(config, origin, seededAt);
  const pages = config.dir === 'front' ? WEB_PAGES : APP_PAGES;
  let nextRequest = 1;
  return async (req, res) => {
    const requestId = `phase22-${config.port}-${nextRequest++}`;
    const raw = req.url ?? '';
    const queryStart = raw.indexOf('?');
    const rawPath = queryStart === -1 ? raw : raw.slice(0, queryStart);
    let isApi = /^\/api(?:\/|$)/i.test(rawPath);
    const method = ['GET', 'POST', 'PATCH', 'DELETE', 'PUT', 'HEAD', 'OPTIONS'].includes(req.method) ? req.method : 'OTHER';
    const trace = { preview: config.label, requestId, method, route: '<unhandled API>' };
    const head = method === 'HEAD';
    res.setHeader('Cache-Control', 'no-store');
    res.setHeader('X-Content-Type-Options', 'nosniff');
    res.setHeader('X-Request-Id', requestId);
    try {
      let path;
      try { path = decodeURIComponent(rawPath); }
      catch { throw new PreviewError(400, 'invalid_preview_path', 'Invalid preview request path.'); }
      isApi ||= /^\/api(?:\/|$)/i.test(path);
      if (req.headers.host !== authority || (req.headers.origin !== undefined && req.headers.origin !== origin)) {
        throw new PreviewError(403, 'preview_local_origin_only', 'Use this preview\'s exact 127.0.0.1 origin.');
      }
      // Only normal URL segments; no dot segments, hidden files, Windows drives, or ADS.
      if (!/^\/(?:[a-z\d_-][a-z\d_.-]*(?:\/[a-z\d_-][a-z\d_.-]*)*\/?)?$/i.test(path)) {
        throw new PreviewError(400, 'invalid_preview_path', 'Invalid preview request path.');
      }
      if (isApi) {
        const query = new URLSearchParams(queryStart === -1 ? '' : raw.slice(queryStart + 1));
        const { status, data } = await api(req, path, query, trace);
        return send(res, status, 'application/json; charset=utf-8', status === 204 ? '' : JSON.stringify(data), head);
      }
      if (method !== 'GET' && !head) throw new PreviewError(405, 'unexpected_preview_method', 'Only GET and HEAD serve preview files.');
      if (pages.has(path === '/' ? path : path.replace(/\/$/, ''))) {
        res.setHeader('Content-Security-Policy', CSP);
        return send(res, 200, 'text/html; charset=utf-8', index, head);
      }
      const contentType = MIME.get(extname(path).toLowerCase());
      if (!contentType || path.startsWith('/r/')) throw new PreviewError(404, 'preview_not_found', 'No such preview file or route.');
      return send(res, 200, contentType, await staticFile(path.slice(1)), head);
    } catch (error) {
      req.resume();
      const failure = error instanceof PreviewError ? error : ['ENOENT', 'ENOTDIR'].includes(error?.code) && !isApi
        ? new PreviewError(404, 'preview_not_found', 'No such preview file.')
        : new PreviewError(500, 'preview_internal_error', 'The local preview could not complete this request.');
      trace.error = failure.code;
      const payload = isApi ? JSON.stringify({ error: { code: failure.code, message: failure.message, requestId } }) : failure.message;
      return send(res, failure.status, isApi ? 'application/json; charset=utf-8' : 'text/plain; charset=utf-8', payload, head);
    } finally {
      if (isApi) log(JSON.stringify({ ...trace, status: res.statusCode }));
    }
  };
}

async function main() {
  const seededAt = Date.now();
  const handlers = await Promise.all(PREVIEWS.map((config) => preparePreview(config, seededAt)));
  const servers = handlers.map((handle) => createServer(handle));
  const started = await Promise.allSettled(servers.map((server, i) => {
    const ready = once(server, 'listening');
    server.listen(PREVIEWS[i].port, HOST);
    return ready;
  }));
  const failed = started.findIndex((result) => result.status === 'rejected');
  if (failed !== -1) {
    for (const server of servers) if (server.listening) server.close();
    throw new Error(`Cannot bind ${PREVIEWS[failed].label} at ${HOST}:${PREVIEWS[failed].port} (${started[failed].reason.code ?? 'listen error'}).`);
  }
  console.log(`Phase 2.2 preview: Web http://${HOST}:43122/login | App http://${HOST}:43123/`);
  console.log('The ports have independent in-memory fixtures. Restart to reset; Ctrl+C to stop.');
}

if (process.argv[1] && pathToFileURL(await realpath(process.argv[1])).href === import.meta.url) {
  try { await main(); }
  catch (error) { console.error(error.message); process.exitCode = 1; }
}
