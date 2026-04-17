package aiquery

import "strings"

// BuildSystemPrompt composes the instructions given to the LLM before
// the user's question. The emphasis is on shape (SELECT / WITH) and on
// returning raw SQL with no prose.
func BuildSystemPrompt(schemaCard string) string {
	var b strings.Builder
	b.WriteString("You are a PostgreSQL-compatible SQL expert working against Nucleus, ")
	b.WriteString("an analytics database. Translate the user's natural-language question ")
	b.WriteString("into a single SELECT or WITH ... SELECT statement.\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Return SQL only. No prose, no comments, no markdown fences.\n")
	b.WriteString("- Only SELECT or WITH ... SELECT. Never INSERT, UPDATE, DELETE, DROP, or any DDL.\n")
	b.WriteString("- Never combine multiple statements. Exactly one statement.\n")
	b.WriteString("- Timestamps in this database are stored as BIGINT milliseconds since Unix epoch.\n")
	b.WriteString("- When comparing timestamps against $N parameters, use CAST($N AS BIGINT).\n")
	b.WriteString("- Prefer events_recent over events when the question is about the last 24 hours.\n")
	b.WriteString("- Default to LIMIT 100 unless the user asks for a specific count.\n\n")
	b.WriteString("Schema (use only these tables and columns):\n")
	b.WriteString(schemaCard)
	return b.String()
}
