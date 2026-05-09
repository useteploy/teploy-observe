import { useState, useEffect, useCallback } from "preact/hooks";
import { get, post } from "../api/helpers.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import Modal from "../components/shared/Modal.js";
import CodeBlock from "../components/shared/CodeBlock.js";
import ExportButton from "../components/shared/ExportButton.js";

export const config = { mode: "app" };

const BASE = "/api/v1/surveys";

interface Survey {
  survey_id: string;
  site_id: string;
  name: string;
  questions: string;
  appearance: string;
  targeting: string;
  status: string;
  created_at: string;
}

interface SurveyResponse {
  response_id: string;
  survey_id: string;
  site_id: string;
  user_id: string;
  answers: string;
  timestamp: number;
}

interface Question {
  id: string;
  type: string;
  text: string;
  required: boolean;
  choices?: string[];
}

const QUESTION_TYPES = [
  { value: "text", label: "Text" },
  { value: "rating", label: "Rating (1-5)" },
  { value: "nps", label: "NPS (0-10)" },
  { value: "choice", label: "Multiple Choice" },
];

function ResponsesView({ survey, onBack }: { survey: Survey; onBack: () => void }) {
  const [responses, setResponses] = useState<SurveyResponse[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    get<SurveyResponse[]>(`${BASE}/${survey.survey_id}/responses`)
      .then(d => setResponses(d || []))
      .catch(() => setResponses([]))
      .finally(() => setLoading(false));
  }, [survey.survey_id]);

  const questions: Question[] = (() => {
    try { return JSON.parse(survey.questions); } catch { return []; }
  })();

  return (
    <div>
      <button class="errors-back-btn" onClick={onBack} style={{ marginBottom: "16px" }}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">
          <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
        </svg>
        Back to surveys
      </button>

      <div class="obs-page-header">
        <h1 class="obs-page-title">{survey.name}</h1>
        <div class="obs-page-actions">
          <StatusBadge status={survey.status === "active" ? "enabled" : "disabled"} size="md" />
        </div>
      </div>

      <div style={{ fontSize: "13px", color: "var(--obs-text-secondary)", marginBottom: "12px" }}>
        {responses.length} response{responses.length !== 1 ? "s" : ""}
      </div>

      {loading ? (
        <div class="obs-empty-state">Loading...</div>
      ) : responses.length === 0 ? (
        <div class="obs-empty-state">No responses yet</div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: "1px", background: "var(--obs-border-subtle)", borderRadius: "var(--obs-radius-md)", overflow: "hidden" }}>
          {responses.map(r => {
            let parsed: Record<string, any> = {};
            try { parsed = JSON.parse(r.answers); } catch {}
            return (
              <div key={r.response_id} style={{ background: "var(--obs-surface)", padding: "12px 16px" }}>
                <div style={{ display: "flex", justifyContent: "space-between", marginBottom: "8px" }}>
                  <span style={{ fontSize: "11px", color: "var(--obs-text-muted)", fontFamily: "var(--obs-font-mono, monospace)" }}>
                    {r.user_id || "anonymous"}
                  </span>
                  <span style={{ fontSize: "11px", color: "var(--obs-text-muted)" }}>
                    {new Date(r.timestamp).toLocaleString()}
                  </span>
                </div>
                {questions.map(q => (
                  <div key={q.id} style={{ marginBottom: "6px" }}>
                    <div style={{ fontSize: "11px", color: "var(--obs-text-muted)" }}>{q.text}</div>
                    <div style={{ fontSize: "13px", color: "var(--obs-text)" }}>
                      {parsed[q.id] !== undefined ? String(parsed[q.id]) : "(no answer)"}
                    </div>
                  </div>
                ))}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

export default function SurveysPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [surveys, setSurveys] = useState<Survey[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [selected, setSelected] = useState<Survey | null>(null);

  const [formName, setFormName] = useState("");
  const [formQuestions, setFormQuestions] = useState<Question[]>([
    { id: "q1", type: "rating", text: "How satisfied are you?", required: true },
  ]);

  const fetch = useCallback(async () => {
    setLoading(true);
    try {
      const data = await get<Survey[]>(`${BASE}?site_id=${siteId}`);
      setSurveys(data || []);
    } catch { setSurveys([]); }
    finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetch(); }, [fetch]);

  const handleCreate = async () => {
    if (!formName.trim()) return;
    setCreating(true);
    try {
      await post<Survey>(BASE, {
        site_id: siteId,
        name: formName.trim(),
        questions: JSON.stringify(formQuestions),
        appearance: "{}",
        targeting: "{}",
      });
      setShowCreate(false);
      setFormName("");
      setFormQuestions([{ id: "q1", type: "rating", text: "How satisfied are you?", required: true }]);
      fetch();
    } catch (err) { console.error("Failed to create survey:", err); }
    finally { setCreating(false); }
  };

  const handleActivate = async (surveyId: string) => {
    try {
      await post<{ ok: boolean }>(`${BASE}/${surveyId}/activate`, {});
      fetch();
    } catch (err) { console.error("Failed to activate survey:", err); }
  };

  const addQuestion = () => {
    setFormQuestions(prev => [...prev, {
      id: `q${prev.length + 1}`,
      type: "text",
      text: "",
      required: false,
    }]);
  };

  const updateQuestion = (i: number, field: keyof Question, value: any) => {
    setFormQuestions(prev => prev.map((q, idx) => idx === i ? { ...q, [field]: value } : q));
  };

  const removeQuestion = (i: number) => {
    setFormQuestions(prev => prev.filter((_, idx) => idx !== i));
  };

  if (selected) {
    return <ResponsesView survey={selected} onBack={() => setSelected(null)} />;
  }

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Surveys</h1>
        <div class="obs-page-actions">
          <ExportButton
            filename={`surveys-${siteId}-${Date.now()}.csv`}
            rows={surveys}
            columns={[
              { key: "name", label: "name" },
              { key: "status", label: "status" },
              { key: "created_at", label: "created_at" },
            ]}
          />
          <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>New Survey</button>
        </div>
      </div>

      {loading ? (
        <div class="obs-empty-state">Loading...</div>
      ) : surveys.length === 0 ? (
        <div class="obs-empty-state">No surveys yet. Create one to collect user feedback.</div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: "1px", background: "var(--obs-border-subtle)", borderRadius: "var(--obs-radius-md)", overflow: "hidden" }}>
          {surveys.map(s => {
            const questions: Question[] = (() => {
              try { return JSON.parse(s.questions); } catch { return []; }
            })();
            return (
              <div key={s.survey_id}
                style={{ background: "var(--obs-surface)", padding: "14px 16px", cursor: "pointer", display: "flex", alignItems: "center", gap: "16px" }}
                onClick={() => setSelected(s)}>
                <StatusBadge status={s.status === "active" ? "enabled" : s.status === "closed" ? "disabled" : "draft"} size="sm" />
                <div style={{ flex: 1 }}>
                  <div style={{ fontSize: "14px", fontWeight: 500, color: "var(--obs-text)" }}>{s.name}</div>
                  <div style={{ fontSize: "12px", color: "var(--obs-text-muted)", marginTop: "2px" }}>
                    {questions.length} question{questions.length !== 1 ? "s" : ""}
                  </div>
                </div>
                {s.status !== "active" && (
                  <button class="obs-btn obs-btn--sm"
                    onClick={(e) => { e.stopPropagation(); handleActivate(s.survey_id); }}>
                    Activate
                  </button>
                )}
              </div>
            );
          })}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Create Survey">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="Customer Satisfaction Q4" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>

        <div class="obs-form-group">
          <label class="obs-label">Questions</label>
          <div style={{ display: "flex", flexDirection: "column", gap: "8px" }}>
            {formQuestions.map((q, i) => (
              <div key={i} style={{ padding: "8px", background: "var(--obs-surface-hover)", borderRadius: "var(--obs-radius)", display: "flex", flexDirection: "column", gap: "6px" }}>
                <div style={{ display: "flex", gap: "6px" }}>
                  <select class="obs-select" value={q.type} style={{ width: "120px" }}
                    onChange={(e) => updateQuestion(i, "type", (e.target as HTMLSelectElement).value)}>
                    {QUESTION_TYPES.map(t => <option key={t.value} value={t.value}>{t.label}</option>)}
                  </select>
                  <input class="obs-input" placeholder="Question text" value={q.text} style={{ flex: 1 }}
                    onInput={(e) => updateQuestion(i, "text", (e.target as HTMLInputElement).value)} />
                  {formQuestions.length > 1 && (
                    <button class="obs-btn obs-btn--sm" onClick={() => removeQuestion(i)}>x</button>
                  )}
                </div>
                {q.type === "choice" && (
                  <input class="obs-input" placeholder="Choices (comma-separated)" value={q.choices?.join(", ") || ""}
                    onInput={(e) => updateQuestion(i, "choices", (e.target as HTMLInputElement).value.split(",").map(s => s.trim()).filter(Boolean))} />
                )}
              </div>
            ))}
            <button class="obs-btn obs-btn--sm" style={{ alignSelf: "flex-start" }} onClick={addQuestion}>
              Add Question
            </button>
          </div>
        </div>

        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim() || formQuestions.some(q => !q.text.trim())}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>
    </div>
  );
}
