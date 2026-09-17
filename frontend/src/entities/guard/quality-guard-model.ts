import { z } from "zod";
import { durationSeconds, formatNonNegativeDuration, parseDuration } from "@/shared/lib/duration";
import type { QualityGuardInput, QualityGuardPolicy } from "./quality-api";

// Guard owns the server-side limits and zero-as-default normalization. This
// form validates its own editable surface, independently from gateway settings.
const durationValue = z.object({ value: z.number().nonnegative(), unit: z.enum(["s", "m", "h", "d"]) });
const guardDuration = durationValue.refine((value) => durationSeconds(value) <= 86400);
const guardCooldown = durationValue.refine((value) => durationSeconds(value) <= 168 * 3600);
export const qualityGuardSchema = z.object({
  enabled: z.boolean(),
  guardedModels: z.array(z.string().trim().min(1)),
  maxAttempts: z.number().int().min(1).max(100),
  createdTimeout: guardDuration,
  evidenceTimeout: guardDuration,
  admissionTimeout: guardDuration,
  toolAdmissionTimeout: guardDuration,
  accountCooldown: guardCooldown,
  idleAccountCooldown: guardCooldown,
}).refine((value) => !value.enabled || value.guardedModels.length > 0, { path: ["guardedModels"], message: "Select at least one model" });
export type QualityGuardForm = z.infer<typeof qualityGuardSchema>;

export function toQualityGuardForm(policy: QualityGuardPolicy): QualityGuardForm {
  return { enabled: policy.enabled, guardedModels: [...policy.guarded_models], maxAttempts: policy.max_attempts,
    createdTimeout: parseDuration(policy.created_timeout), evidenceTimeout: parseDuration(policy.evidence_timeout),
    admissionTimeout: parseDuration(policy.admission_timeout), toolAdmissionTimeout: parseDuration(policy.tool_admission_timeout),
    accountCooldown: parseDuration(policy.account_cooldown), idleAccountCooldown: parseDuration(policy.idle_account_cooldown) };
}
export function toQualityGuardInput(revision: string, form: QualityGuardForm): QualityGuardInput {
  return { revision, enabled: form.enabled, guarded_models: [...form.guardedModels], max_attempts: form.maxAttempts,
    created_timeout: formatNonNegativeDuration(form.createdTimeout), evidence_timeout: formatNonNegativeDuration(form.evidenceTimeout),
    admission_timeout: formatNonNegativeDuration(form.admissionTimeout), tool_admission_timeout: formatNonNegativeDuration(form.toolAdmissionTimeout),
    account_cooldown: formatNonNegativeDuration(form.accountCooldown), idle_account_cooldown: formatNonNegativeDuration(form.idleAccountCooldown) };
}
