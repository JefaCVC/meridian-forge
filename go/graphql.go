// Meridian Forge — GraphQL v4 at /api/graphql.
//
// A small GraphQL parser + executor for the document subset the conformance
// suite sends: operations with variables, arguments (literals, variables,
// object literals, lists, enums), aliases, inline fragments, and the __type
// introspection field. Unknown fields produce HTTP 200 with an errors array
// naming the field; batch bodies (JSON arrays) return per-operation results
// in order.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ---- lexer ----

type gqlToken struct {
	kind string // "name" | "int" | "float" | "string" | "punct" | "eof"
	text string
}

type gqlLexer struct {
	s   string
	pos int
}

func (l *gqlLexer) skipIgnored() {
	for l.pos < len(l.s) {
		c := l.s[l.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			l.pos++
			continue
		}
		if c == '#' {
			for l.pos < len(l.s) && l.s[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		break
	}
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func (l *gqlLexer) next() gqlToken {
	l.skipIgnored()
	if l.pos >= len(l.s) {
		return gqlToken{kind: "eof"}
	}
	c := l.s[l.pos]
	switch {
	case c == '.' && strings.HasPrefix(l.s[l.pos:], "..."):
		l.pos += 3
		return gqlToken{kind: "punct", text: "..."}
	case c == '{' || c == '}' || c == '(' || c == ')' || c == ':' || c == '!' || c == '$' || c == '=' || c == '@' || c == '[' || c == ']':
		l.pos++
		return gqlToken{kind: "punct", text: string(c)}
	case c == '"':
		// string with escapes
		l.pos++
		var b strings.Builder
		for l.pos < len(l.s) {
			ch := l.s[l.pos]
			l.pos++
			if ch == '"' {
				return gqlToken{kind: "string", text: b.String()}
			}
			if ch == '\\' && l.pos < len(l.s) {
				esc := l.s[l.pos]
				l.pos++
				switch esc {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '"':
					b.WriteByte('"')
				case '\\':
					b.WriteByte('\\')
				case '/':
					b.WriteByte('/')
				case 'u':
					if l.pos+4 <= len(l.s) {
						n, _ := strconv.ParseInt(l.s[l.pos:l.pos+4], 16, 32)
						b.WriteRune(rune(n))
						l.pos += 4
					}
				default:
					b.WriteByte(esc)
				}
				continue
			}
			b.WriteByte(ch)
		}
		return gqlToken{kind: "string", text: b.String()}
	case c == '-' || (c >= '0' && c <= '9'):
		start := l.pos
		if c == '-' {
			l.pos++
		}
		isFloat := false
		for l.pos < len(l.s) && l.s[l.pos] >= '0' && l.s[l.pos] <= '9' {
			l.pos++
		}
		if l.pos < len(l.s) && l.s[l.pos] == '.' {
			isFloat = true
			l.pos++
			for l.pos < len(l.s) && l.s[l.pos] >= '0' && l.s[l.pos] <= '9' {
				l.pos++
			}
		}
		if l.pos < len(l.s) && (l.s[l.pos] == 'e' || l.s[l.pos] == 'E') {
			isFloat = true
			l.pos++
			if l.pos < len(l.s) && (l.s[l.pos] == '+' || l.s[l.pos] == '-') {
				l.pos++
			}
			for l.pos < len(l.s) && l.s[l.pos] >= '0' && l.s[l.pos] <= '9' {
				l.pos++
			}
		}
		kind := "int"
		if isFloat {
			kind = "float"
		}
		return gqlToken{kind: kind, text: l.s[start:l.pos]}
	case isNameStart(c):
		start := l.pos
		for l.pos < len(l.s) && isNameChar(l.s[l.pos]) {
			l.pos++
		}
		return gqlToken{kind: "name", text: l.s[start:l.pos]}
	default:
		l.pos++
		return gqlToken{kind: "punct", text: string(c)}
	}
}

// ---- parser ----

type gqlSelection struct {
	kind    string // "field" | "fragment"
	name    string // field name, or fragment type condition
	alias   string
	args    map[string]any
	selSet  []gqlSelection
}

type gqlVar struct{ name string }

type gqlOperation struct {
	opType string // "query" | "mutation"
	selSet []gqlSelection
}

type gqlParser struct {
	lex *gqlLexer
	cur gqlToken
}

func (p *gqlParser) advance() gqlToken {
	t := p.cur
	p.cur = p.lex.next()
	return t
}

func (p *gqlParser) expectPunct(text string) error {
	if p.cur.kind != "punct" || p.cur.text != text {
		return fmt.Errorf("expected %q, got %q", text, p.cur.text)
	}
	p.advance()
	return nil
}

func parseGraphQL(query string) ([]*gqlOperation, error) {
	p := &gqlParser{lex: &gqlLexer{s: query}}
	p.cur = p.lex.next()
	var ops []*gqlOperation
	for p.cur.kind != "eof" {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{opType: "query"}
	if p.cur.kind == "name" && (p.cur.text == "query" || p.cur.text == "mutation") {
		op.opType = p.cur.text
		p.advance()
		// optional operation name
		if p.cur.kind == "name" {
			p.advance()
		}
		// optional variable definitions
		if p.cur.kind == "punct" && p.cur.text == "(" {
			if err := p.skipParens(); err != nil {
				return nil, err
			}
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selSet = sel
	return op, nil
}

func (p *gqlParser) skipParens() error {
	depth := 0
	for {
		if p.cur.kind == "eof" {
			return fmt.Errorf("unterminated variable definitions")
		}
		if p.cur.kind == "punct" {
			if p.cur.text == "(" {
				depth++
			} else if p.cur.text == ")" {
				depth--
				p.advance()
				if depth == 0 {
					return nil
				}
				continue
			}
		}
		p.advance()
	}
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []gqlSelection
	for p.cur.kind != "eof" && !(p.cur.kind == "punct" && p.cur.text == "}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	// inline fragment: ... on Type { ... }
	if p.cur.kind == "punct" && p.cur.text == "..." {
		p.advance()
		if p.cur.kind == "name" && p.cur.text == "on" {
			p.advance()
			if p.cur.kind != "name" {
				return gqlSelection{}, fmt.Errorf("expected type name after ... on")
			}
			typ := p.cur.text
			p.advance()
			sel, err := p.parseSelectionSet()
			if err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{kind: "fragment", name: typ, selSet: sel}, nil
		}
		return gqlSelection{}, fmt.Errorf("named fragment spreads are not supported")
	}
	if p.cur.kind != "name" {
		return gqlSelection{}, fmt.Errorf("expected field name, got %q", p.cur.text)
	}
	name := p.cur.text
	p.advance()
	alias := ""
	if p.cur.kind == "punct" && p.cur.text == ":" {
		p.advance()
		if p.cur.kind != "name" {
			return gqlSelection{}, fmt.Errorf("expected field name after alias")
		}
		alias = name
		name = p.cur.text
		p.advance()
	}
	sel := gqlSelection{kind: "field", name: name, alias: alias}
	// arguments
	if p.cur.kind == "punct" && p.cur.text == "(" {
		args, err := p.parseArguments()
		if err != nil {
			return gqlSelection{}, err
		}
		sel.args = args
	}
	// directives (skip: @include/@skip not used by the suite)
	for p.cur.kind == "punct" && p.cur.text == "@" {
		p.advance()
		if p.cur.kind == "name" {
			p.advance()
		}
		if p.cur.kind == "punct" && p.cur.text == "(" {
			if err := p.skipParens(); err != nil {
				return gqlSelection{}, err
			}
		}
	}
	// sub-selection
	if p.cur.kind == "punct" && p.cur.text == "{" {
		sub, err := p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
		sel.selSet = sub
	}
	return sel, nil
}

func (p *gqlParser) parseArguments() (map[string]any, error) {
	args := map[string]any{}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for p.cur.kind != "eof" && !(p.cur.kind == "punct" && p.cur.text == ")") {
		if p.cur.kind != "name" {
			return nil, fmt.Errorf("expected argument name, got %q", p.cur.text)
		}
		name := p.cur.text
		p.advance()
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = v
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (any, error) {
	t := p.cur
	switch t.kind {
	case "int":
		p.advance()
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "float":
		p.advance()
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, err
		}
		return f, nil
	case "string":
		p.advance()
		return t.text, nil
	case "name":
		p.advance()
		switch t.text {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return t.text, nil // enum value
		}
	case "punct":
		switch t.text {
		case "$":
			p.advance()
			if p.cur.kind != "name" {
				return nil, fmt.Errorf("expected variable name after $")
			}
			name := p.cur.text
			p.advance()
			return gqlVar{name: name}, nil
		case "[":
			p.advance()
			var out []any
			for p.cur.kind != "eof" && !(p.cur.kind == "punct" && p.cur.text == "]") {
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			return out, nil
		case "{":
			p.advance()
			out := map[string]any{}
			for p.cur.kind != "eof" && !(p.cur.kind == "punct" && p.cur.text == "}") {
				if p.cur.kind != "name" {
					return nil, fmt.Errorf("expected object field name, got %q", p.cur.text)
				}
				name := p.cur.text
				p.advance()
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				out[name] = v
			}
			if err := p.expectPunct("}"); err != nil {
				return nil, err
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %q", t.text)
}

// ---- executor ----

// Internal value wrappers.
type gqlRepoV struct{ repo *Repo }
type gqlIssueV struct {
	repo *Repo
	iss  *Issue
}
type gqlPullV struct {
	repo *Repo
	p    *Pull
}
type gqlPageInfoV struct {
	hasNextPage bool
	endCursor   *string
}
type gqlConnV struct {
	nodes    []any
	pageInfo *gqlPageInfoV
	nodeType string
}
type gqlTypeV struct {
	name       string
	kind       string
	fields     []string
	enumValues []string
}
type gqlFieldV struct{ name string }
type gqlEnumV struct{ name string }
type gqlCommentEdgeV struct {
	repo *Repo
	iss  *Issue
	c    *Comment
}

type gqlExecutor struct {
	srv  *Server
	vars map[string]any
	errs []map[string]any
}

// resolveVars substitutes variables in an argument value.
func (e *gqlExecutor) resolveVars(v any) any {
	switch t := v.(type) {
	case gqlVar:
		if val, ok := e.vars[t.name]; ok {
			return val
		}
		return nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = e.resolveVars(x)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			out[k] = e.resolveVars(x)
		}
		return out
	default:
		return v
	}
}

func (e *gqlExecutor) resolveArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range args {
		out[k] = e.resolveVars(v)
	}
	return out
}

// fieldSet lists the fields each type exposes (for validation).
var gqlFieldSet = map[string]map[string]bool{
	"Query": {
		"viewer": true, "repository": true, "node": true, "__type": true,
	},
	"Mutation": {
		"createIssue": true, "closeIssue": true, "createPullRequest": true,
		"mergePullRequest": true, "addComment": true,
	},
	"User": {"login": true, "id": true, "name": true, "email": true},
	"Repository": {
		"id": true, "databaseId": true, "name": true, "nameWithOwner": true,
		"owner": true, "defaultBranchRef": true, "viewerPermission": true,
		"hasIssuesEnabled": true, "issue": true, "issues": true,
		"pullRequests": true, "pullRequest": true, "description": true,
	},
	"RepositoryOwner": {"login": true, "id": true},
	"Ref":             {"name": true},
	"Issue":           {"id": true, "number": true, "title": true, "state": true, "body": true},
	"IssueConnection": {"nodes": true, "pageInfo": true, "totalCount": true},
	"PageInfo":        {"hasNextPage": true, "endCursor": true, "hasPreviousPage": true, "startCursor": true},
	"PullRequest": {
		"id": true, "number": true, "headRefName": true, "baseRefName": true,
		"mergeable": true, "state": true, "merged": true, "files": true,
		"title": true, "body": true,
	},
	"PullRequestConnection": {"nodes": true, "pageInfo": true, "totalCount": true},
	"PullRequestChangedFileConnection": {"nodes": true, "pageInfo": true, "totalCount": true},
	"PullRequestChangedFile":           {"path": true},
	"CreateIssuePayload":               {"issue": true},
	"CloseIssuePayload":                {"issue": true},
	"CreatePullRequestPayload":         {"pullRequest": true},
	"MergePullRequestPayload":          {"pullRequest": true},
	"AddCommentPayload":                {"commentEdge": true},
	"IssueCommentEdge":                 {"node": true},
	"IssueComment":                     {"id": true, "body": true},
	"__Type":                           {"name": true, "kind": true, "fields": true, "enumValues": true},
	"__Field":                          {"name": true},
	"__EnumValue":                      {"name": true},
}

// executeSelectionSet runs a selection set against obj of the given type.
func (e *gqlExecutor) executeSelectionSet(sel []gqlSelection, obj any, typeName string) map[string]any {
	out := map[string]any{}
	for _, s := range sel {
		if s.kind == "fragment" {
			if s.name == typeName {
				sub := e.executeSelectionSet(s.selSet, obj, typeName)
				for k, v := range sub {
					out[k] = v
				}
			}
			continue
		}
		key := s.name
		if s.alias != "" {
			key = s.alias
		}
		if !gqlFieldSet[typeName][s.name] {
			e.errs = append(e.errs, map[string]any{
				"message": fmt.Sprintf("Cannot query field %q on type %q.", s.name, typeName),
			})
			continue
		}
		args := e.resolveArgs(s.args)
		val, childType, isList := e.resolveField(typeName, s.name, args, obj)
		if val == nil {
			out[key] = nil
			continue
		}
		if childType == "" {
			out[key] = val
			continue
		}
		if isList {
			items := val.([]any)
			arr := make([]any, 0, len(items))
			for _, item := range items {
				arr = append(arr, e.executeSelectionSet(s.selSet, item, childType))
			}
			out[key] = arr
			continue
		}
		out[key] = e.executeSelectionSet(s.selSet, val, childType)
	}
	return out
}

// resolveField resolves one field. Returns (value, childType, isList).
// childType == "" means a scalar (or nil) value.
func (e *gqlExecutor) resolveField(typeName, field string, args map[string]any, obj any) (any, string, bool) {
	switch typeName {
	case "Query":
		return e.resolveQueryField(field, args)
	case "Mutation":
		return e.resolveMutationField(field, args)
	case "User":
		if field == "login" {
			return viewerLogin, "", false
		}
		if field == "id" {
			return viewerID, "", false
		}
		if field == "name" {
			return viewerName, "", false
		}
		if field == "email" {
			return viewerEmail, "", false
		}
	case "Repository":
		return e.resolveRepositoryField(field, args, obj.(*gqlRepoV).repo)
	case "RepositoryOwner":
		if field == "login" {
			return viewerLogin, "", false
		}
		if field == "id" {
			return viewerID, "", false
		}
	case "Ref":
		if field == "name" {
			return obj.(string), "", false
		}
	case "Issue":
		iv := obj.(*gqlIssueV)
		switch field {
		case "id":
			return iv.repo.issueGlobalID(iv.iss), "", false
		case "number":
			return iv.iss.Number, "", false
		case "title":
			return iv.iss.Title, "", false
		case "state":
			if iv.iss.State == "closed" {
				return "CLOSED", "", false
			}
			return "OPEN", "", false
		case "body":
			return iv.iss.Body, "", false
		}
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection":
		cv := obj.(*gqlConnV)
		switch field {
		case "nodes":
			return cv.nodes, cv.nodeType, true
		case "pageInfo":
			return cv.pageInfo, "PageInfo", false
		case "totalCount":
			return len(cv.nodes), "", false
		}
	case "PageInfo":
		pv := obj.(*gqlPageInfoV)
		switch field {
		case "hasNextPage":
			return pv.hasNextPage, "", false
		case "endCursor":
			if pv.endCursor == nil {
				return nil, "", false
			}
			return *pv.endCursor, "", false
		case "hasPreviousPage":
			return false, "", false
		case "startCursor":
			return nil, "", false
		}
	case "PullRequest":
		pv := obj.(*gqlPullV)
		switch field {
		case "id":
			return pv.repo.pullGlobalID(pv.p), "", false
		case "number":
			return pv.p.Number, "", false
		case "title":
			return pv.p.Title, "", false
		case "body":
			return pv.p.Body, "", false
		case "headRefName":
			return pv.p.HeadRef, "", false
		case "baseRefName":
			return pv.p.BaseRef, "", false
		case "state":
			if pv.p.State == "closed" {
				return "CLOSED", "", false
			}
			return "OPEN", "", false
		case "merged":
			return pv.p.Merged, "", false
		case "mergeable":
			return pv.repo.mergeableStatus(pv.p), "", false
		case "files":
			first := argInt(args, "first", 100)
			baseTree := pv.repo.treeEntriesAt(pv.p.BaseSHA)
			headTree := pv.repo.treeEntriesAt(pv.p.HeadSHA)
			changes := pv.repo.diffTrees(baseTree, headTree)
			var nodes []any
			for i, c := range changes {
				if i >= first {
					break
				}
				nodes = append(nodes, c.Path)
			}
			return &gqlConnV{nodes: nodes, pageInfo: &gqlPageInfoV{}, nodeType: "PullRequestChangedFile"}, "PullRequestChangedFileConnection", false
		}
	case "PullRequestChangedFile":
		if field == "path" {
			return obj.(string), "", false
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		if field == "issue" {
			return obj, "Issue", false
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field == "pullRequest" {
			return obj, "PullRequest", false
		}
	case "AddCommentPayload":
		if field == "commentEdge" {
			return obj, "IssueCommentEdge", false
		}
	case "IssueCommentEdge":
		if field == "node" {
			return obj, "IssueComment", false
		}
	case "IssueComment":
		ev := obj.(*gqlCommentEdgeV)
		switch field {
		case "id":
			return encodeGlobalID("C", strconv.Itoa(ev.repo.ID), strconv.Itoa(ev.iss.Number), strconv.Itoa(ev.c.ID)), "", false
		case "body":
			return ev.c.Body, "", false
		}
	case "__Type":
		tv := obj.(*gqlTypeV)
		switch field {
		case "name":
			return tv.name, "", false
		case "kind":
			return tv.kind, "", false
		case "fields":
			if tv.fields == nil {
				return nil, "", false
			}
			var nodes []any
			for _, f := range tv.fields {
				nodes = append(nodes, &gqlFieldV{name: f})
			}
			return nodes, "__Field", true
		case "enumValues":
			if tv.enumValues == nil {
				return nil, "", false
			}
			var nodes []any
			for _, v := range tv.enumValues {
				nodes = append(nodes, &gqlEnumV{name: v})
			}
			return nodes, "__EnumValue", true
		}
	case "__Field":
		if field == "name" {
			return obj.(*gqlFieldV).name, "", false
		}
	case "__EnumValue":
		if field == "name" {
			return obj.(*gqlEnumV).name, "", false
		}
	}
	return nil, "", false
}

func (e *gqlExecutor) resolveQueryField(field string, args map[string]any) (any, string, bool) {
	switch field {
	case "viewer":
		return struct{}{}, "User", false
	case "repository":
		owner := argString(args, "owner", "")
		name := argString(args, "name", "")
		if owner != viewerLogin {
			return nil, "", false
		}
		repo := e.srv.store.getRepo(owner, name)
		if repo == nil {
			return nil, "", false
		}
		return &gqlRepoV{repo: repo}, "Repository", false
	case "node":
		id := argString(args, "id", "")
		parts := decodeGlobalID(id)
		if len(parts) >= 2 && parts[0] == "R" {
			dbid, err := strconv.Atoi(parts[1])
			if err == nil {
				if repo := e.srv.store.repoByID(dbid); repo != nil {
					return &gqlRepoV{repo: repo}, "Repository", false
				}
			}
		}
		return nil, "", false
	case "__type":
		name := argString(args, "name", "")
		if tv := gqlTypeInfo(name); tv != nil {
			return tv, "__Type", false
		}
		return nil, "", false
	}
	return nil, "", false
}

// gqlTypeInfo returns introspection data for the types the suite queries.
func gqlTypeInfo(name string) *gqlTypeV {
	switch name {
	case "Issue":
		return &gqlTypeV{name: "Issue", kind: "OBJECT", fields: []string{"id", "number", "title", "state", "body", "author", "createdAt", "comments"}}
	case "PullRequest":
		return &gqlTypeV{name: "PullRequest", kind: "OBJECT", fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body"}}
	case "Repository":
		return &gqlTypeV{name: "Repository", kind: "OBJECT", fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description"}}
	case "SearchType":
		return &gqlTypeV{name: "SearchType", kind: "ENUM", enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "MILESTONE"}}
	case "User":
		return &gqlTypeV{name: "User", kind: "OBJECT", fields: []string{"id", "login", "name", "email"}}
	}
	return nil
}

func (e *gqlExecutor) resolveRepositoryField(field string, args map[string]any, repo *Repo) (any, string, bool) {
	switch field {
	case "id":
		return repo.globalID(), "", false
	case "databaseId":
		return repo.ID, "", false
	case "name":
		return repo.Name, "", false
	case "nameWithOwner":
		return repo.fullName(), "", false
	case "description":
		return repo.Description, "", false
	case "owner":
		return struct{}{}, "RepositoryOwner", false
	case "defaultBranchRef":
		return repo.DefaultBranch, "Ref", false
	case "viewerPermission":
		return "ADMIN", "", false
	case "hasIssuesEnabled":
		return true, "", false
	case "issue":
		num := argInt(args, "number", 0)
		if iss, ok := repo.issues[num]; ok {
			return &gqlIssueV{repo: repo, iss: iss}, "Issue", false
		}
		return nil, "", false
	case "issues":
		return e.issuesConnection(repo, args), "IssueConnection", false
	case "pullRequests":
		first := argInt(args, "first", 100)
		var nodes []any
		for i := len(repo.pullOrder) - 1; i >= 0 && len(nodes) < first; i-- {
			nodes = append(nodes, &gqlPullV{repo: repo, p: repo.pulls[repo.pullOrder[i]]})
		}
		return &gqlConnV{nodes: nodes, pageInfo: &gqlPageInfoV{}, nodeType: "PullRequest"}, "PullRequestConnection", false
	case "pullRequest":
		num := argInt(args, "number", 0)
		if p, ok := repo.pulls[num]; ok {
			return &gqlPullV{repo: repo, p: p}, "PullRequest", false
		}
		return nil, "", false
	}
	return nil, "", false
}

func (e *gqlExecutor) issuesConnection(repo *Repo, args map[string]any) *gqlConnV {
	first := argInt(args, "first", 100)
	after := argString(args, "after", "")
	// states filter
	var states map[string]bool
	if raw, ok := args["states"]; ok {
		states = map[string]bool{}
		if list, ok := raw.([]any); ok {
			for _, s := range list {
				if str, ok := s.(string); ok {
					states[str] = true
				}
			}
		}
	}
	// order
	desc := true
	if raw, ok := args["orderBy"]; ok {
		if m, ok := raw.(map[string]any); ok {
			if d, ok := m["direction"].(string); ok && d == "ASC" {
				desc = false
			}
		}
	}
	// collect matching issue numbers in the requested order
	var nums []int
	if desc {
		for i := len(repo.issueOrder) - 1; i >= 0; i-- {
			nums = append(nums, repo.issueOrder[i])
		}
	} else {
		nums = append(nums, repo.issueOrder...)
	}
	var filtered []int
	for _, n := range nums {
		iss := repo.issues[n]
		if states != nil {
			st := "OPEN"
			if iss.State == "closed" {
				st = "CLOSED"
			}
			if !states[st] {
				continue
			}
		}
		filtered = append(filtered, n)
	}
	// apply the after cursor
	start := 0
	if after != "" {
		if num, ok := decodeCursor(after); ok {
			for i, n := range filtered {
				if n == num {
					start = i + 1
					break
				}
			}
		}
	}
	end := start + first
	hasNext := false
	if end < len(filtered) {
		hasNext = true
	} else if end > len(filtered) {
		end = len(filtered)
	}
	var nodes []any
	var endCursor *string
	for _, n := range filtered[start:end] {
		nodes = append(nodes, &gqlIssueV{repo: repo, iss: repo.issues[n]})
	}
	if len(filtered[start:end]) > 0 {
		c := encodeCursor(filtered[end-1])
		endCursor = &c
	}
	return &gqlConnV{nodes: nodes, pageInfo: &gqlPageInfoV{hasNextPage: hasNext, endCursor: endCursor}, nodeType: "Issue"}
}

func encodeCursor(num int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(num)))
}

func decodeCursor(c string) (int, bool) {
	b, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	s := strings.TrimPrefix(string(b), "cursor:")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---- mutations ----

func (e *gqlExecutor) resolveMutationField(field string, args map[string]any) (any, string, bool) {
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	s := e.srv.store
	switch field {
	case "createIssue":
		repoID := argString(input, "repositoryId", "")
		parts := decodeGlobalID(repoID)
		if len(parts) < 2 || parts[0] != "R" {
			e.errs = append(e.errs, map[string]any{"message": "repository not found"})
			return nil, "", false
		}
		dbid, _ := strconv.Atoi(parts[1])
		repo := s.repoByID(dbid)
		if repo == nil {
			e.errs = append(e.errs, map[string]any{"message": "repository not found"})
			return nil, "", false
		}
		iss := s.newIssue(repo, argString(input, "title", ""), argString(input, "body", ""), nil)
		return &gqlIssueV{repo: repo, iss: iss}, "CreateIssuePayload", false
	case "closeIssue":
		issueID := argString(input, "issueId", "")
		parts := decodeGlobalID(issueID)
		if len(parts) < 3 || parts[0] != "I" {
			e.errs = append(e.errs, map[string]any{"message": "issue not found"})
			return nil, "", false
		}
		dbid, _ := strconv.Atoi(parts[1])
		num, _ := strconv.Atoi(parts[2])
		repo := s.repoByID(dbid)
		if repo == nil {
			e.errs = append(e.errs, map[string]any{"message": "issue not found"})
			return nil, "", false
		}
		iss, ok := repo.issues[num]
		if !ok {
			e.errs = append(e.errs, map[string]any{"message": "issue not found"})
			return nil, "", false
		}
		iss.State = "closed"
		iss.UpdatedAt = s.nextTimeISO()
		repo.refreshIssueJSON(iss)
		return &gqlIssueV{repo: repo, iss: iss}, "CloseIssuePayload", false
	case "createPullRequest":
		repoID := argString(input, "repositoryId", "")
		parts := decodeGlobalID(repoID)
		if len(parts) < 2 || parts[0] != "R" {
			e.errs = append(e.errs, map[string]any{"message": "repository not found"})
			return nil, "", false
		}
		dbid, _ := strconv.Atoi(parts[1])
		repo := s.repoByID(dbid)
		if repo == nil {
			e.errs = append(e.errs, map[string]any{"message": "repository not found"})
			return nil, "", false
		}
		head := argString(input, "headRefName", "")
		base := argString(input, "baseRefName", "")
		headSHA, ok1 := repo.refs["refs/heads/"+head]
		baseSHA, ok2 := repo.refs["refs/heads/"+base]
		if !ok1 || !ok2 {
			e.errs = append(e.errs, map[string]any{"message": "head or base branch does not exist"})
			return nil, "", false
		}
		p := s.newPull(repo, argString(input, "title", ""), argString(input, "body", ""), head, base, headSHA, baseSHA)
		return &gqlPullV{repo: repo, p: p}, "CreatePullRequestPayload", false
	case "mergePullRequest":
		prID := argString(input, "pullRequestId", "")
		parts := decodeGlobalID(prID)
		if len(parts) < 3 || parts[0] != "P" {
			e.errs = append(e.errs, map[string]any{"message": "pull request not found"})
			return nil, "", false
		}
		dbid, _ := strconv.Atoi(parts[1])
		num, _ := strconv.Atoi(parts[2])
		repo := s.repoByID(dbid)
		if repo == nil {
			e.errs = append(e.errs, map[string]any{"message": "pull request not found"})
			return nil, "", false
		}
		p, ok := repo.pulls[num]
		if !ok {
			e.errs = append(e.errs, map[string]any{"message": "pull request not found"})
			return nil, "", false
		}
		if _, conflicted, msg := e.srv.mergePullRequest(repo, p); conflicted {
			e.errs = append(e.errs, map[string]any{"message": msg})
			return nil, "", false
		}
		return &gqlPullV{repo: repo, p: p}, "MergePullRequestPayload", false
	case "addComment":
		subjectID := argString(input, "subjectId", "")
		parts := decodeGlobalID(subjectID)
		if len(parts) < 3 || parts[0] != "I" {
			e.errs = append(e.errs, map[string]any{"message": "subject not found"})
			return nil, "", false
		}
		dbid, _ := strconv.Atoi(parts[1])
		num, _ := strconv.Atoi(parts[2])
		repo := s.repoByID(dbid)
		if repo == nil {
			e.errs = append(e.errs, map[string]any{"message": "subject not found"})
			return nil, "", false
		}
		iss, ok := repo.issues[num]
		if !ok {
			e.errs = append(e.errs, map[string]any{"message": "subject not found"})
			return nil, "", false
		}
		now := s.nextTimeISO()
		c := &Comment{ID: s.nextCommentID, Body: argString(input, "body", ""), CreatedAt: now, UpdatedAt: now}
		s.nextCommentID++
		iss.Comments = append(iss.Comments, c)
		iss.UpdatedAt = now
		repo.refreshIssueJSON(iss)
		return &gqlCommentEdgeV{repo: repo, iss: iss, c: c}, "AddCommentPayload", false
	}
	return nil, "", false
}

// ---- HTTP handler ----

// repoByID finds a repo by its database id. Caller must hold a lock.
func (s *Store) repoByID(dbid int) *Repo {
	for _, key := range s.repoOrder {
		if r := s.repos[key]; r != nil && r.ID == dbid {
			return r
		}
	}
	return nil
}

func argString(args map[string]any, name, def string) string {
	if v, ok := args[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func argInt(args map[string]any, name string, def int) int {
	if v, ok := args[name]; ok {
		switch n := v.(type) {
		case int64:
			return int(n)
		case float64:
			return int(n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return int(i)
			}
		case int:
			return n
		}
	}
	return def
}

// handleGraphQL serves POST /api/graphql (single or batch).
func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	var raw json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"errors": []map[string]any{{"message": "malformed JSON: " + err.Error()}},
		})
		return
	}
	// batch?
	if len(raw) > 0 && raw[0] == '[' {
		var batch []map[string]any
		if err := json.Unmarshal(raw, &batch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"errors": []map[string]any{{"message": "malformed JSON batch"}},
			})
			return
		}
		results := make([]any, 0, len(batch))
		for _, op := range batch {
			results = append(results, s.executeGraphQLRequest(op))
		}
		writeJSON(w, http.StatusOK, results)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"errors": []map[string]any{{"message": "malformed JSON"}},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.executeGraphQLRequest(req))
}

func (s *Server) executeGraphQLRequest(req map[string]any) any {
	query, _ := req["query"].(string)
	variables := map[string]any{}
	if v, ok := req["variables"].(map[string]any); ok {
		variables = v
	}
	ops, err := parseGraphQL(query)
	if err != nil {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]any{{"message": err.Error()}},
		}
	}
	op := ops[0]
	rootType := "Query"
	if op.opType == "mutation" {
		rootType = "Mutation"
	}
	// Mutations take the write lock; queries the read lock.
	if rootType == "Mutation" {
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
	} else {
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
	}
	ex := &gqlExecutor{srv: s, vars: variables}
	data := ex.executeSelectionSet(op.selSet, nil, rootType)
	if len(ex.errs) > 0 {
		return map[string]any{"data": nil, "errors": ex.errs}
	}
	return map[string]any{"data": data}
}
