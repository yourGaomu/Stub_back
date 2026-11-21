import assert from "node:assert/strict";
import test from "node:test";
import {
  buildCreateLinkRequest,
  initialCreateForm,
  linkMetadata,
  randomAccessPassword,
} from "../../../app/src/lib/link-form.ts";
import {
  buildRulesPatch,
  localExpiration,
  parseMaxVisits,
  renewalExpiration,
  validateAccessPassword,
} from "../../../front/src/lib/link-form.ts";

function sourceLink(overrides = {}) {
  return {
    title: "Protected delivery",
    tags: ["customer"],
    code: "existing-code",
    target: { type: "url", url: "https://example.test/brief" },
    expiresAt: new Date(Date.now() + 30 * 86400000).toISOString(),
    visitsUsed: 7,
    ruleTemplate: { templateId: "tpl_old", version: 1 },
    rules: {
      maxVisits: 10,
      allowedIpCidrs: ["10.0.0.0/8"],
      requirePassword: true,
      wrongPasswordPolicy: { strategy: "progressive_cooldown" },
    },
    ...overrides,
  };
}

function editableRules(source, overrides = {}) {
  return {
    expiresAt: localExpiration(source.expiresAt),
    maxVisits: String(source.rules.maxVisits),
    cidrs: source.rules.allowedIpCidrs.join("\n"),
    requirePassword: source.rules.requirePassword,
    password: "",
    passwordStrategy: source.rules.wrongPasswordPolicy?.strategy || "fixed_cooldown",
    ...overrides,
  };
}

test("App blank max visits means unlimited; explicit zero stays forbidden", () => {
  const form = initialCreateForm({ presetUrl: "https://example.test/default" });
  assert.equal(buildCreateLinkRequest(form).ruleSource.rules.maxVisits, -1);
  assert.equal(buildCreateLinkRequest({ ...form, maxVisits: "0" }).ruleSource.rules.maxVisits, 0);
  assert.equal(buildCreateLinkRequest({ ...form, maxVisits: "1" }).ruleSource.rules.maxVisits, 1);
});

test("App rejects invalid and unsafe visit limits before submitting", () => {
  const form = initialCreateForm({ presetUrl: "https://example.test/default" });
  for (const value of ["-2", "1.1", "NaN", "Infinity", "9007199254740992"]) {
    assert.throws(() => buildCreateLinkRequest({ ...form, maxVisits: value }), value);
  }
});

test("App copy initializes current inline rules and clears code/password", () => {
  const source = sourceLink();
  const form = initialCreateForm({ source });
  assert.equal(form.ruleSource, "inline");
  assert.equal(form.customCode, "");
  assert.equal(form.password, "");
  assert.throws(() => buildCreateLinkRequest(form), /口令/);
  const request = buildCreateLinkRequest({ ...form, password: " new secret " });
  assert.deepEqual(request.ruleSource, { type: "inline", rules: source.rules });
  assert.equal(request.accessPassword, " new secret ");
  assert.equal(request.code, undefined);
  assert.equal(request.expiresAt, source.expiresAt);
  assert.deepEqual(request.target, source.target);
  assert.equal("visitsUsed" in request, false);
});

test("App expired source requires a newly selected expiry", () => {
  const source = sourceLink({ expiresAt: new Date(Date.now() - 86400000).toISOString() });
  const form = initialCreateForm({ source });
  assert.equal(form.expirationMode, "hours");
  assert.throws(() => buildCreateLinkRequest({ ...form, password: "new secret" }));
  assert.ok(Date.parse(buildCreateLinkRequest({ ...form, expireHours: "24", password: "new secret" }).expiresAt) > Date.now());
});

test("App copy retains a source with no expiration", () => {
  const form = initialCreateForm({ source: sourceLink({ expiresAt: null }) });
  assert.equal(buildCreateLinkRequest({ ...form, password: "new secret" }).expiresAt, null);
});

test("App template requirements determine password payload, not stale form values", () => {
  const source = sourceLink();
  const form = { ...initialCreateForm({ presetUrl: source.target.url }), ruleSource: "template", templateId: "tpl_current", password: "stale" };
  assert.equal(buildCreateLinkRequest(form, { ...source.rules, requirePassword: false }).accessPassword, undefined);
  assert.throws(() => buildCreateLinkRequest({ ...form, password: "" }, source.rules), /口令/);
  assert.throws(() => buildCreateLinkRequest(form), /模板/);
});

test("App accepts the byte limit and rejects overlong passwords", () => {
  const form = initialCreateForm({ source: sourceLink() });
  assert.equal(buildCreateLinkRequest({ ...form, password: "a".repeat(1024) }).accessPassword.length, 1024);
  assert.throws(() => buildCreateLinkRequest({ ...form, password: "汉".repeat(342) }), /1024/);
});

test("App metadata preserves Unicode characters and normalizes tags", () => {
  assert.deepEqual(linkMetadata("  项目😀  ", [" One ", "one", "客户"], "新标签"), { title: "项目😀", tags: ["One", "客户", "新标签"] });
  assert.throws(() => linkMetadata("😀".repeat(121), []));
  assert.throws(() => linkMetadata("", ["x".repeat(33)]));
});

test("App random access password has a useful fixed minimum length", () => {
  const password = randomAccessPassword();
  assert.ok(password.startsWith("BURN-"));
  assert.ok(password.length >= 17);
});

test("Web unchanged past expiry does not block editing another rule", () => {
  const source = sourceLink({ expiresAt: new Date(Date.now() - 86400000).toISOString() });
  const initial = localExpiration(source.expiresAt);
  assert.deepEqual(buildRulesPatch(source, initial, editableRules(source, { maxVisits: "11" })), { maxVisits: 11 });
});

test("Web no-op rules edit emits no changes", () => {
  const source = sourceLink();
  assert.deepEqual(buildRulesPatch(source, localExpiration(source.expiresAt), editableRules(source)), {});
});

test("Web disabling password does not send hidden secret or strategy", () => {
  const source = sourceLink();
  const form = editableRules(source, { requirePassword: false, password: "stale", passwordStrategy: "fixed_cooldown" });
  assert.deepEqual(buildRulesPatch(source, localExpiration(source.expiresAt), form), { requirePassword: false });
});

test("Web replacement password retains intentional surrounding spaces", () => {
  const source = sourceLink();
  assert.deepEqual(buildRulesPatch(source, localExpiration(source.expiresAt), editableRules(source, { password: " new secret " })), { accessPassword: " new secret " });
});

test("Web explicitly cleared expiry sends null and changed past expiry is rejected", () => {
  const source = sourceLink();
  const initial = localExpiration(source.expiresAt);
  assert.deepEqual(buildRulesPatch(source, initial, editableRules(source, { expiresAt: "" })), { expiresAt: null });
  assert.throws(() => buildRulesPatch(source, initial, editableRules(source, { expiresAt: "2020-01-01T00:00" })));
});

test("Web enforces safe integers and the actual UTF-8 password byte limit", () => {
  assert.equal(parseMaxVisits("-1"), -1);
  assert.equal(parseMaxVisits("0"), 0);
  for (const value of ["", "-2", "1.5", "NaN", "9007199254740992"]) assert.throws(() => parseMaxVisits(value));
  assert.doesNotThrow(() => validateAccessPassword("a".repeat(1024)));
  assert.throws(() => validateAccessPassword("汉".repeat(342)));
  assert.throws(() => validateAccessPassword("   "));
});

test("Web default renewal extends a future expiration rather than shortening it", () => {
  const original = new Date(Date.now() + 30 * 86400000).toISOString();
  assert.ok(Date.parse(renewalExpiration(original)) > Date.parse(original));
});
