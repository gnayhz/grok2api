import assert from "node:assert/strict";
import test from "node:test";

import { auditBilling, auditGuardState, auditOutcome, auditResult, completionTone } from "./audit-presentation.ts";

test("request result keeps an HTTP 200 stream failure distinct from success", () => {
  assert.equal(auditResult({ statusCode: 200, errorCode: "upstream_stream_interrupted" }), "failed");
  assert.equal(auditResult({ statusCode: 503 }), "failed");
  assert.equal(auditResult({ statusCode: 200 }), "success");
  assert.equal(auditResult({ statusCode: 0 }), "unknown");
  assert.equal(auditResult({ statusCode: 302 }), "unknown");
});

test("billing distinguishes missing metering from a confirmed zero cost", () => {
  const empty = { costInUsdTicks: 0, estimatedCostInUsdTicks: 0 };
  assert.equal(auditBilling(empty), undefined);
  assert.equal(auditBilling({ ...empty, pricingModel: "synthetic-model" })?.totalInUsdTicks, 0);
  const confirmedZero = { source: "upstream", method: "upstream_reported", components: [], totalInUsdTicks: 0 } as const;
  assert.equal(auditBilling({ ...empty, billing: { ...confirmedZero, components: [] } })?.totalInUsdTicks, 0);
});

test("stored billing takes precedence and legacy rows keep their original source", () => {
  const legacy = { costInUsdTicks: 123, estimatedCostInUsdTicks: 456, pricingModel: "synthetic-model" };
  assert.equal(auditBilling(legacy)?.source, "upstream");
  assert.equal(auditBilling({ ...legacy, costInUsdTicks: 0 })?.source, "official");
  assert.equal(auditBilling({
    ...legacy,
    billing: { source: "official", method: "stored_estimate", components: [], totalInUsdTicks: 789 },
  })?.totalInUsdTicks, 789);
});

test("completion styling does not turn missing, unnecessary or uncertain work into success", () => {
  for (const outcome of [undefined, "", "not_recorded", "not_required", "not_started", "future-state"]) {
    assert.equal(completionTone(outcome), "neutral");
  }
  for (const outcome of ["unconfirmed", "not_committed", "partial", "canceled"]) {
    assert.equal(completionTone(outcome), "warning");
  }
  assert.equal(completionTone("failed"), "failed");
  assert.equal(completionTone("committed"), "success");
});


test("row outcomes distinguish recovery and failure causes without treating a 200 error as success", () => {
  const request = { statusCode: 200, attemptCount: 0 };
  assert.equal(auditOutcome(request), "firstSuccess");
  assert.equal(auditOutcome({ ...request, attemptCount: 2 }), "retrySuccess");
  assert.equal(auditOutcome({ ...request, qualityFailOpen: true, attemptCount: 2 }), "qualityReleased");
  assert.equal(auditOutcome({ ...request, errorCode: "upstream_stream_interrupted" }), "streamInterrupted");
  assert.equal(auditOutcome({ ...request, statusCode: 503, errorCode: "quality_degraded" }), "qualityBlocked");
  assert.equal(auditOutcome({ ...request, statusCode: 429, errorCode: "upstream_rate_limited_resource_exhausted" }), "rateLimited");
  assert.equal(auditOutcome({ ...request, statusCode: 502, errorCode: "upstream_header_timeout" }), "timeout");
  assert.equal(auditOutcome({ ...request, statusCode: 499 }), "canceled");
  assert.equal(auditOutcome({ ...request, statusCode: 200, errorCode: "future_failure" }), "failed");
  assert.equal(auditOutcome({ ...request, statusCode: 0 }), "unknown");
});

test("guard marks require recorded evidence and preserve intervention separately from the request result", () => {
  assert.equal(auditGuardState({}), "unrecorded");
  assert.equal(auditGuardState({ errorCode: "upstream_timeout" }), "unrecorded");
  assert.equal(auditGuardState({ qualityExempt: "disabled" }), "bypassed");
  assert.equal(auditGuardState({ qualityRule: "thinking" }), "passed");
  assert.equal(auditGuardState({ qualityRule: "terminal_burst" }), "intervened");
  assert.equal(auditGuardState({ errorCode: "quality_evidence_timeout" }), "intervened");
  assert.equal(auditGuardState({ errorCode: "quality_degraded" }), "blocked");
  assert.equal(auditGuardState({ qualityFailOpen: true, qualityRule: "terminal_burst" }), "released");
  const recovered = { statusCode: 200, attemptCount: 3, qualityRule: "thinking" };
  assert.equal(auditOutcome(recovered), "retrySuccess");
  assert.equal(auditGuardState(recovered), "passed");
});
