package mcp

import "fmt"

// SQL supported by MCP is deliberately narrower than the explorer dialect.
// Resolve each SELECT independently; names introduced by another scope never
// authorize source columns. Unknown functions and unsupported shapes fail closed.
var pureFunctions = map[string]bool{
	"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true,
	"ARGMAX": true, "ARGMIN": true, "COALESCE": true, "NULLIF": true,
	"ABS": true, "ROUND": true, "FLOOR": true, "CEIL": true, "CEILING": true,
	"LOWER": true, "UPPER": true, "LENGTH": true, "CONCAT": true,
	"SUBSTRING": true, "TRIM": true, "EXTRACT": true, "DATE_TRUNC": true,
	"CAST": true,
}

type columnSet map[string]bool

func parenEnd(ts []token, start int) (int, error) {
	depth := 0
	for i := start; i < len(ts); i++ {
		if ts[i].text == "(" {
			depth++
		}
		if ts[i].text == ")" {
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("unbalanced SQL parentheses")
}

func checkScope(ts []token, inherited map[string]columnSet) (columnSet, error) {
	ctes := map[string]columnSet{}
	for k, v := range inherited {
		ctes[k] = v
	}
	for len(ts) > 0 && isWord(ts, 0, "EXPLAIN") {
		ts = ts[1:]
	}
	if isWord(ts, 0, "WITH") {
		i := 1
		for {
			if i >= len(ts) || ts[i].kind != tokIdent || !isWord(ts, i+1, "AS") || next(ts, i+2) == nil || ts[i+2].text != "(" {
				return nil, fmt.Errorf("unsupported MCP CTE syntax")
			}
			name := ts[i].text
			if _, ok := allowedTables[name]; ok {
				return nil, fmt.Errorf("CTE cannot shadow a readable table")
			}
			end, err := parenEnd(ts, i+2)
			if err != nil {
				return nil, err
			}
			cols, err := checkScope(ts[i+3:end], ctes)
			if err != nil {
				return nil, err
			}
			ctes[name] = cols
			i = end + 1
			if next(ts, i) != nil && ts[i].text == "," {
				i++
				continue
			}
			ts = ts[i:]
			break
		}
	}
	if !isWord(ts, 0, "SELECT") {
		return nil, fmt.Errorf("MCP SQL must be a SELECT query")
	}
	// Separate set-operation arms rather than unioning their input privileges.
	depth := 0
	for i, t := range ts {
		if t.text == "(" {
			depth++
		}
		if t.text == ")" {
			depth--
		}
		if depth == 0 && (t.upper == "UNION" || t.upper == "INTERSECT" || t.upper == "EXCEPT") {
			cols, err := checkScope(ts[:i], ctes)
			if err != nil {
				return nil, err
			}
			j := i + 1
			if isWord(ts, j, "ALL") || isWord(ts, j, "DISTINCT") {
				j++
			}
			_, err = checkScope(ts[j:], ctes)
			return cols, err
		}
	}
	skip := map[int]bool{}
	derived := map[int]columnSet{}
	// Validate subqueries before making their output columns available.
	for i := 0; i < len(ts); i++ {
		if ts[i].text != "(" {
			continue
		}
		end, err := parenEnd(ts, i)
		if err != nil {
			return nil, err
		}
		if isWord(ts, i+1, "SELECT") || isWord(ts, i+1, "WITH") {
			cols, err := checkScope(ts[i+1:end], ctes)
			if err != nil {
				return nil, err
			}
			derived[i] = cols
			for j := i; j <= end; j++ {
				skip[j] = true
			}
			i = end
		}
	}
	relations := map[string]columnSet{}
	from := -1
	depth = 0
	expecting := false
	inFrom := false
	for i := 0; i < len(ts); i++ {
		if skip[i] {
			if expecting && derived[i] != nil {
				end, _ := parenEnd(ts, i)
				j := end + 1
				if isWord(ts, j, "AS") {
					skip[j] = true
					j++
				}
				if next(ts, j) == nil || ts[j].kind != tokIdent || sqlKeywords[ts[j].upper] {
					return nil, fmt.Errorf("derived table requires an alias")
				}
				relations[ts[j].text] = derived[i]
				skip[j] = true
				expecting = false
				i = j
			}
			continue
		}
		t := ts[i]
		if t.text == "(" {
			depth++
		}
		if t.text == ")" {
			depth--
		}
		if depth != 0 {
			continue
		}
		if t.upper == "FROM" || t.upper == "JOIN" {
			if from < 0 {
				from = i
			}
			expecting = true
			inFrom = true
			continue
		}
		if t.upper == "WHERE" || t.upper == "GROUP" || t.upper == "ORDER" || t.upper == "HAVING" || t.upper == "LIMIT" {
			inFrom = false
		}
		if inFrom && t.text == "," {
			expecting = true
			continue
		}
		if !expecting {
			continue
		}
		if t.kind != tokIdent {
			return nil, fmt.Errorf("unsupported table reference")
		}
		if next(ts, i+1) != nil && ts[i+1].text == "." {
			return nil, fmt.Errorf("schema-qualified table references are not permitted over MCP")
		}
		cols, ok := ctes[t.text]
		if !ok {
			spec, allowed := allowedTables[t.text]
			if !allowed {
				return nil, fmt.Errorf("table %q is not readable over MCP", t.text)
			}
			cols = columnSet{}
			for _, c := range spec.columns {
				cols[c] = true
			}
		}
		skip[i] = true
		name := t.text
		j := i + 1
		if isWord(ts, j, "AS") {
			skip[j] = true
			j++
		}
		if next(ts, j) != nil && ts[j].kind == tokIdent && !sqlKeywords[ts[j].upper] {
			name = ts[j].text
			skip[j] = true
			i = j
		}
		if relations[name] != nil {
			return nil, fmt.Errorf("duplicate relation alias %q", name)
		}
		relations[name] = cols
		expecting = false
	}
	if expecting {
		return nil, fmt.Errorf("missing table reference")
	}
	// Projection aliases are outputs, never input capabilities. Only ORDER BY
	// and GROUP BY may reference them in this SELECT; derived scopes receive
	// them only after all source expressions have passed validation.
	outputs := columnSet{}
	projectionEnd := len(ts)
	if from >= 0 {
		projectionEnd = from
	}
	aliases := columnSet{}
	depth = 0
	for i := 1; i < projectionEnd; i++ {
		if skip[i] {
			continue
		}
		t := ts[i]
		if t.text == "(" {
			depth++
		}
		if t.text == ")" {
			depth--
		}
		if depth == 0 && t.upper == "AS" && next(ts, i+1) != nil && ts[i+1].kind == tokIdent {
			aliases[ts[i+1].text] = true
			outputs[ts[i+1].text] = true
			skip[i+1] = true
		}
	}
	clause := "SELECT"
	depth = 0
	for i := 0; i < len(ts); i++ {
		if skip[i] {
			continue
		}
		t := ts[i]
		if t.text == "(" {
			depth++
		}
		if t.text == ")" {
			depth--
		}
		if depth == 0 && !t.quoted && (t.upper == "FROM" || t.upper == "WHERE" || t.upper == "GROUP" || t.upper == "ORDER" || t.upper == "HAVING" || t.upper == "ON" || t.upper == "LIMIT") {
			clause = t.upper
		}
		if t.text == "*" && isWildcard(ts, i) {
			if i < 2 || ts[i-1].text != "(" || ts[i-2].upper != "COUNT" {
				return nil, fmt.Errorf("SELECT * is not permitted over MCP")
			}
		}
		if t.kind != tokIdent {
			continue
		}
		if !t.quoted && expressionKeywords[t.upper] {
			continue
		}
		if next(ts, i+1) != nil && ts[i+1].text == "(" {
			if !pureFunctions[t.upper] && t.upper != "IN" && t.upper != "EXISTS" {
				return nil, fmt.Errorf("function %q is not permitted over MCP", t.text)
			}
			continue
		}
		if next(ts, i+1) != nil && ts[i+1].text == "." {
			cols := relations[t.text]
			if next(ts, i+3) != nil && ts[i+3].text == "(" {
				return nil, fmt.Errorf("qualified functions are not permitted over MCP")
			}
			if cols == nil || next(ts, i+2) == nil || ts[i+2].kind != tokIdent || !cols[ts[i+2].text] {
				return nil, fmt.Errorf("qualified column is not readable over MCP")
			}
			if i < projectionEnd {
				outputs[ts[i+2].text] = true
			}
			i += 2
			continue
		}
		if !t.quoted && sqlKeywords[t.upper] {
			continue
		}
		if (clause == "ORDER" || clause == "GROUP") && aliases[t.text] {
			continue
		}
		matches := 0
		for _, cols := range relations {
			if cols[t.text] {
				matches++
			}
		}
		if matches != 1 {
			return nil, fmt.Errorf("column %q is withheld, unknown, or ambiguous over MCP", t.text)
		}
		if i < projectionEnd {
			outputs[t.text] = true
		}
	}
	// Only explicitly named projection outputs cross a derived-table boundary.
	// Inputs used inside functions aren't output columns without an AS alias.
	named := columnSet{}
	depth = 0
	for i := 1; i < projectionEnd; i++ {
		t := ts[i]
		if t.text == "(" {
			depth++
		}
		if t.text == ")" {
			depth--
		}
		if depth == 0 && t.kind == tokIdent && outputs[t.text] {
			named[t.text] = true
		}
	}
	for name := range aliases {
		named[name] = true
	}
	return named, nil
}

var expressionKeywords = map[string]bool{
	"SELECT": true, "DISTINCT": true, "WHERE": true, "ON": true, "AND": true, "FROM": true, "JOIN": true,
	"OR": true, "NOT": true, "WHEN": true, "THEN": true, "ELSE": true, "BY": true,
}
