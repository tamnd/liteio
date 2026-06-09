// SPDX-License-Identifier: Apache-2.0

package s3

// select_expr.go implements a minimal SQL subset for SelectObjectContent:
//
//   SELECT * | col [AS alias] [, ...]
//   FROM s3object
//   WHERE <predicate>
//   LIMIT N
//
// Predicates: =, !=, <>, <, >, <=, >=, AND, OR, NOT, IS NULL, IS NOT NULL.
// CAST(col AS type) in the column list.
//
// The implementation is intentionally minimal: it tokenizes the SQL string and
// builds a tiny AST that evalExpr walks for each row.

import (
	"strconv"
	"strings"
)

// selectCol is one projection column.
type selectCol struct {
	src   string // column name or positional reference (_1, _2, ...)
	alias string // AS alias (empty = use src)
	cast  string // CAST target type (empty = no cast)
}

// exprNode is one node in the predicate AST.
type exprNode struct {
	op    string    // AND, OR, NOT, =, !=, <, >, <=, >=, IS NULL, IS NOT NULL
	left  *exprNode // left child (or nil for leaf)
	right *exprNode // right child (or nil)
	col   string    // column reference (leaf)
	lit   string    // literal value (leaf, "" for column)
	isLit bool      // true when this node is a literal
}

// parsedSelect is the result of parseSelectExpr.
type parsedSelect struct {
	cols    []selectCol // nil = SELECT *
	where   *exprNode  // nil = no filter
	limit   int64      // 0 = no limit
	outJSON bool       // true when OutputSerialization is JSON
}

// parseSelectExpr does a best-effort parse of an S3 Select SQL expression.
// It handles the common cases; malformed SQL falls back to SELECT * / no WHERE.
func parseSelectExpr(sql string) parsedSelect {
	p := &sqlParser{tokens: tokenize(sql)}
	return p.parse()
}

// --- tokenizer ---------------------------------------------------------------

// strLitPrefix marks a token as a string literal so the parser can distinguish
// column references ("city") from string values ("NYC").
const strLitPrefix = "\x00"

// tokenize splits sql into word/punctuation tokens. String literals (single or
// double-quoted) are kept with their quotes stripped but prefixed by strLitPrefix
// so the parser knows they are values, not identifiers. Keywords and identifiers
// are lowercased; literals preserve their original case.
func tokenize(sql string) []string {
	var out []string
	s := strings.TrimSpace(sql)
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t\r\n")
		if len(s) == 0 {
			break
		}
		if s[0] == '\'' || s[0] == '"' {
			q := s[0]
			end := strings.IndexByte(s[1:], q)
			if end < 0 {
				out = append(out, strLitPrefix+s[1:])
				break
			}
			out = append(out, strLitPrefix+s[1:end+1])
			s = s[end+2:]
			continue
		}
		// two-char ops
		if len(s) >= 2 {
			two := s[:2]
			switch two {
			case "!=", "<>", "<=", ">=":
				out = append(out, two)
				s = s[2:]
				continue
			}
		}
		// single-char ops / punctuation
		switch s[0] {
		case '=', '<', '>', ',', '(', ')', '*':
			out = append(out, string(s[0]))
			s = s[1:]
			continue
		}
		// word / identifier / number
		end := strings.IndexAny(s, " \t\r\n=<>!,()* ")
		if end < 0 {
			end = len(s)
		}
		tok := s[:end]
		if tok != "" {
			out = append(out, strings.ToLower(strings.TrimRight(tok, " \t")))
		}
		s = s[end:]
	}
	return out
}

// --- parser ------------------------------------------------------------------

type sqlParser struct {
	tokens []string
	pos    int
}

func (p *sqlParser) peek() string {
	if p.pos >= len(p.tokens) {
		return ""
	}
	return p.tokens[p.pos]
}

func (p *sqlParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *sqlParser) eat(tok string) bool {
	if strings.EqualFold(p.peek(), tok) {
		p.pos++
		return true
	}
	return false
}

func (p *sqlParser) parse() parsedSelect {
	var ps parsedSelect
	if !p.eat("select") {
		return ps
	}
	// Column list.
	ps.cols = p.parseCols()
	// FROM s3object (skip)
	p.eat("from")
	_ = p.next() // table name
	// WHERE
	if p.eat("where") {
		ps.where = p.parseOr()
	}
	// LIMIT
	if p.eat("limit") {
		if n, err := strconv.ParseInt(p.next(), 10, 64); err == nil {
			ps.limit = n
		}
	}
	return ps
}

func (p *sqlParser) parseCols() []selectCol {
	if p.peek() == "*" {
		p.next()
		return nil // SELECT *
	}
	var cols []selectCol
	for {
		col := p.parseOneCol()
		cols = append(cols, col)
		if p.peek() != "," {
			break
		}
		p.next() // consume ","
	}
	return cols
}

func (p *sqlParser) parseOneCol() selectCol {
	// CAST(col AS type)
	if strings.EqualFold(p.peek(), "cast") {
		p.next()
		p.eat("(")
		colName := p.next()
		p.eat("as")
		typName := p.next()
		p.eat(")")
		alias := ""
		if p.eat("as") {
			alias = p.next()
		}
		return selectCol{src: colName, alias: alias, cast: typName}
	}
	name := p.next()
	alias := ""
	if p.eat("as") {
		alias = p.next()
	}
	return selectCol{src: name, alias: alias}
}

// parseOr: OR expression.
func (p *sqlParser) parseOr() *exprNode {
	left := p.parseAnd()
	for strings.EqualFold(p.peek(), "or") {
		p.next()
		right := p.parseAnd()
		left = &exprNode{op: "OR", left: left, right: right}
	}
	return left
}

// parseAnd: AND expression.
func (p *sqlParser) parseAnd() *exprNode {
	left := p.parseNot()
	for strings.EqualFold(p.peek(), "and") {
		p.next()
		right := p.parseNot()
		left = &exprNode{op: "AND", left: left, right: right}
	}
	return left
}

// parseNot: NOT expression.
func (p *sqlParser) parseNot() *exprNode {
	if strings.EqualFold(p.peek(), "not") {
		p.next()
		child := p.parsePrimary()
		return &exprNode{op: "NOT", left: child}
	}
	return p.parsePrimary()
}

// parsePrimary: comparison or parenthesized sub-expression.
func (p *sqlParser) parsePrimary() *exprNode {
	if p.peek() == "(" {
		p.next()
		node := p.parseOr()
		p.eat(")")
		return node
	}
	// column reference or literal
	lhs := p.parseValue()
	// IS NULL / IS NOT NULL
	if strings.EqualFold(p.peek(), "is") {
		p.next()
		not := p.eat("not")
		p.eat("null")
		op := "IS NULL"
		if not {
			op = "IS NOT NULL"
		}
		return &exprNode{op: op, left: lhs}
	}
	// comparison operators
	op := p.peek()
	switch op {
	case "=", "!=", "<>", "<", ">", "<=", ">=":
		p.next()
		rhs := p.parseValue()
		if op == "<>" {
			op = "!="
		}
		return &exprNode{op: op, left: lhs, right: rhs}
	}
	// bare column reference (boolean column, treated as IS NOT NULL)
	return lhs
}

func (p *sqlParser) parseValue() *exprNode {
	tok := p.next()
	// quoted string literal (marked by tokenizer)
	if strings.HasPrefix(tok, strLitPrefix) {
		return &exprNode{lit: tok[len(strLitPrefix):], isLit: true}
	}
	// numeric literal
	if _, err := strconv.ParseFloat(tok, 64); err == nil {
		return &exprNode{lit: tok, isLit: true}
	}
	// boolean literal
	lower := strings.ToLower(tok)
	if lower == "true" || lower == "false" {
		return &exprNode{lit: lower, isLit: true}
	}
	// everything else is a column reference (strip leading table alias like "s.")
	return &exprNode{col: tok}
}

// --- evaluator ---------------------------------------------------------------

// evalExpr evaluates a predicate node against a row.
func evalExpr(node *exprNode, row map[string]string) bool {
	if node == nil {
		return true
	}
	switch node.op {
	case "AND":
		return evalExpr(node.left, row) && evalExpr(node.right, row)
	case "OR":
		return evalExpr(node.left, row) || evalExpr(node.right, row)
	case "NOT":
		return !evalExpr(node.left, row)
	case "IS NULL":
		v := nodeValue(node.left, row)
		return v == ""
	case "IS NOT NULL":
		v := nodeValue(node.left, row)
		return v != ""
	case "=":
		return nodeValue(node.left, row) == nodeValue(node.right, row)
	case "!=":
		return nodeValue(node.left, row) != nodeValue(node.right, row)
	case "<":
		return cmpValues(nodeValue(node.left, row), nodeValue(node.right, row)) < 0
	case ">":
		return cmpValues(nodeValue(node.left, row), nodeValue(node.right, row)) > 0
	case "<=":
		return cmpValues(nodeValue(node.left, row), nodeValue(node.right, row)) <= 0
	case ">=":
		return cmpValues(nodeValue(node.left, row), nodeValue(node.right, row)) >= 0
	default:
		// bare column reference: true when non-empty
		if node.col != "" {
			return resolveCol(node.col, row) != ""
		}
		return false
	}
}

// nodeValue extracts the string value a node represents for comparison.
func nodeValue(node *exprNode, row map[string]string) string {
	if node == nil {
		return ""
	}
	if node.isLit {
		return node.lit
	}
	if node.col != "" {
		return resolveCol(node.col, row)
	}
	return ""
}

// cmpValues compares two strings, falling back to numeric comparison when both
// parse as float64.
func cmpValues(a, b string) int {
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea == nil && eb == nil {
		if fa < fb {
			return -1
		}
		if fa > fb {
			return 1
		}
		return 0
	}
	// lexicographic fallback
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
