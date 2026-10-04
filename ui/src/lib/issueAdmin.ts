// Pure helpers for the issue assignee / merge controls (errors route).

export const MAX_ASSIGNEE_LEN = 128;

// normalizeAssignee trims the input and returns null when the server would
// reject it (too long or containing control characters). "" is valid: it
// clears the assignee.
export function normalizeAssignee(raw: string): string | null {
  const v = raw.trim();
  if (new TextEncoder().encode(v).length > MAX_ASSIGNEE_LEN) return null;
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(v)) return null;
  return v;
}

// canMerge: a merge needs a target id that differs from the issue itself.
export function canMerge(issueId: string, targetId: string): boolean {
  const t = targetId.trim();
  return t.length > 0 && t !== issueId;
}
