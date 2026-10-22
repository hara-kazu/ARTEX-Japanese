type ApprovalSourceInput = {
  decision_source?: "model" | "rule" | "unknown" | "";
  rule_id?: number | null;
  reason?: string;
};

const modelReasonPrefix = /^\[(?:模型|モデル)\]\s*/;

export function approvalDecisionSource(row: ApprovalSourceInput) {
  if (row.decision_source) return row.decision_source;
  if (row.rule_id) return "rule";
  return modelReasonPrefix.test(row.reason ?? "") ? "model" : "unknown";
}

export function stripModelReasonPrefix(reason?: string) {
  return reason?.replace(modelReasonPrefix, "");
}
