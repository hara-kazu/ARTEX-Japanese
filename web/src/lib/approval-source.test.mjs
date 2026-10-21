import { approvalDecisionSource, stripModelReasonPrefix } from "./approval-source.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("legacy and Japanese model approvals keep their source and display reason", () => {
  for (const reason of ["[模型] approval reason", "[モデル] approval reason"]) {
    assert.equal(approvalDecisionSource({ reason }), "model");
    assert.equal(stripModelReasonPrefix(reason), "approval reason");
  }
  assert.equal(approvalDecisionSource({ reason: "custom reason" }), "unknown");
  assert.equal(approvalDecisionSource({ decision_source: "", reason: "[模型] reason" }), "model");
  assert.equal(stripModelReasonPrefix("custom reason"), "custom reason");
});

test("recorded approval source takes precedence over a legacy reason", () => {
  assert.equal(approvalDecisionSource({ decision_source: "rule", reason: "[模型] reason" }), "rule");
  assert.equal(approvalDecisionSource({ rule_id: 4, reason: "[モデル] reason" }), "rule");
});
