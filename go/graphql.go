// GraphQL: a small, complete implementation of the query language subset the
// suite uses — tokenizer, recursive-descent parser, schema validation, and
// execution — over the same Forge store the REST and MCP surfaces use.
//
//   - Operations: query/mutation, operation names, variable definitions with defaults, aliases,
//     inline fragments, named fragments (defined and spread), directives (parsed, ignored).
//   - Values: variables, strings (with escapes and block strings), ints, floats, booleans, null,
//     enums, lists, input objects.
//   - Validation runs before execution: the first unknown field fails the whole operation with
//     `Cannot query field "X" on type "Y".` and `data: null` (HTTP 200, never 400).
//   - Batch: a JSON array of operation documents returns an array of responses, in order.
//   - Global ids are opaque base64 `Type:key` values, decodable for node() and mutation inputs.

package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- schema

// fieldDef is one schema field: name -> return type (the element type for lists).
type fieldDef struct {
	name string
	typ  string
}

// fieldTypes lists every object type's fields IN ORDER (introspection's
// `fields` returns them in this order).
var fieldTypes = map[string][]fieldDef{
	"Query": {
		{"viewer", "User"}, {"repository", "Repository"}, {"node", "Node"}, {"__type", "__Type"},
	},
	"Mutation": {
		{"createIssue", "CreateIssuePayload"}, {"closeIssue", "CloseIssuePayload"},
		{"createPullRequest", "CreatePullRequestPayload"}, {"mergePullRequest", "MergePullRequestPayload"},
		{"addComment", "AddCommentPayload"},
	},
	"User": {{"id", "ID"}, {"login", "String"}, {"name", "String"}, {"email", "String"}},
	"Repository": {
		{"id", "ID"}, {"databaseId", "Int"}, {"name", "String"}, {"nameWithOwner", "String"},
		{"owner", "User"}, {"defaultBranchRef", "Ref"}, {"viewerPermission", "RepositoryPermission"},
		{"hasIssuesEnabled", "Boolean"}, {"issue", "Issue"}, {"issues", "IssueConnection"},
		{"pullRequests", "PullRequestConnection"}, {"pullRequest", "PullRequest"},
	},
	"Issue": {{"id", "ID"}, {"number", "Int"}, {"title", "String"}, {"state", "IssueState"}, {"body", "String"}},
	"PullRequest": {
		{"id", "ID"}, {"number", "Int"}, {"title", "String"}, {"state", "PullRequestState"},
		{"merged", "Boolean"}, {"headRefName", "String"}, {"baseRefName", "String"},
		{"mergeable", "MergeableState"}, {"files", "PullRequestChangedFileConnection"},
	},
	"IssueConnection":                  {{"nodes", "Issue"}, {"pageInfo", "PageInfo"}, {"totalCount", "Int"}},
	"PullRequestConnection":            {{"nodes", "PullRequest"}, {"pageInfo", "PageInfo"}, {"totalCount", "Int"}},
	"PullRequestChangedFileConnection": {{"nodes", "PullRequestChangedFile"}, {"pageInfo", "PageInfo"}, {"totalCount", "Int"}},
	"PullRequestChangedFile":           {{"path", "String"}, {"additions", "Int"}, {"deletions", "Int"}, {"changeType", "String"}},
	"PageInfo":                         {{"hasNextPage", "Boolean"}, {"hasPreviousPage", "Boolean"}, {"startCursor", "String"}, {"endCursor", "String"}},
	"Ref":                              {{"name", "String"}},
	"IssueComment":                     {{"id", "ID"}, {"body", "String"}},
	"IssueCommentEdge":                 {{"node", "IssueComment"}, {"cursor", "String"}},
	"CreateIssuePayload":               {{"issue", "Issue"}},
	"CloseIssuePayload":                {{"issue", "Issue"}},
	"CreatePullRequestPayload":         {{"pullRequest", "PullRequest"}},
	"MergePullRequestPayload":          {{"pullRequest", "PullRequest"}},
	"AddCommentPayload":                {{"commentEdge", "IssueCommentEdge"}},
	"__Type":                           {{"name", "String"}, {"kind", "String"}, {"fields", "__Field"}, {"enumValues", "__EnumValue"}, {"description", "String"}},
	"__Field":                          {{"name", "String"}, {"description", "String"}},
	"__EnumValue":                      {{"name", "String"}, {"description", "String"}},
	"Node":                             {{"id", "ID"}},
}

var fieldTypeLookup = func() map[string]map[string]string {
	out := make(map[string]map[string]string, len(fieldTypes))
	for typ, fields := range fieldTypes {
		m := make(map[string]string, len(fields))
		for _, f := range fields {
			m[f.name] = f.typ
		}
		out[typ] = m
	}
	return out
}()

var typeKind = map[string]string{
	"Query": "OBJECT", "Mutation": "OBJECT", "User": "OBJECT", "Repository": "OBJECT",
	"Issue": "OBJECT", "PullRequest": "OBJECT", "IssueConnection": "OBJECT",
	"PullRequestConnection": "OBJECT", "PullRequestChangedFileConnection": "OBJECT",
	"PullRequestChangedFile": "OBJECT", "PageInfo": "OBJECT", "Ref": "OBJECT",
	"IssueComment": "OBJECT", "IssueCommentEdge": "OBJECT",
	"CreateIssuePayload": "OBJECT", "CloseIssuePayload": "OBJECT",
	"CreatePullRequestPayload": "OBJECT", "MergePullRequestPayload": "OBJECT",
	"AddCommentPayload": "OBJECT", "__Type": "OBJECT", "__Field": "OBJECT",
	"__EnumValue": "OBJECT", "Node": "INTERFACE",
	"SearchType": "ENUM", "IssueState": "ENUM", "PullRequestState": "ENUM",
	"MergeableState": "ENUM", "RepositoryPermission": "ENUM",
	"IssueOrderField": "ENUM", "OrderDirection": "ENUM", "PullRequestMergeMethod": "ENUM",
	"CreateIssueInput": "INPUT_OBJECT", "CloseIssueInput": "INPUT_OBJECT",
	"CreatePullRequestInput": "INPUT_OBJECT", "MergePullRequestInput": "INPUT_OBJECT",
	"AddCommentInput": "INPUT_OBJECT", "IssueOrder": "INPUT_OBJECT",
	"String": "SCALAR", "Int": "SCALAR", "Boolean": "SCALAR", "ID": "SCALAR", "Float": "SCALAR",
}

var typeEnumValues = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
}

// ---------------------------------------------------------------- AST

type valueKind int

const (
	vVariable valueKind = iota
	vInt
	vFloat
	vString
	vBoolean
	vNull
	vEnum
	vList
	vObject
)

type valueNode struct {
	kind   valueKind
	name   string // variable
	num    float64
	str    string
	b      bool
	values []*valueNode
	fields []objectField
}

type objectField struct {
	name  string
	value *valueNode
}

type gqlArg struct {
	name  string
	value *valueNode
}

type selectionKind int

const (
	selField selectionKind = iota
	selSpread
	selInline
)

type selection struct {
	kind          selectionKind
	alias         string
	hasAlias      bool
	name          string
	args          []*gqlArg
	selections    []*selection
	typeCondition string
	hasTypeCond   bool
}

type variableDef struct {
	name         string
	defaultValue *valueNode
}

type operation struct {
	kind       string // "query" | "mutation" | "subscription"
	name       string
	hasName    bool
	variables  []*variableDef
	selections []*selection
}

type fragmentDef struct {
	name          string
	typeCondition string
	selections    []*selection
}

type gqlDocument struct {
	operations []*operation
	fragments  map[string]*fragmentDef
}

// ---------------------------------------------------------------- tokenizer

type tokenKind int

const (
	tokName tokenKind = iota
	tokInt
	tokFloat
	tokString
	tokPunct
	tokEOF
)

type token struct {
	kind  tokenKind
	value string
}

func tokenize(source string) ([]token, error) {
	var tokens []token
	i := 0
	n := len(source)
	for i < n {
		ch := source[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
			continue
		case ch == '#':
			for i < n && source[i] != '\n' {
				i++
			}
			continue
		case ch == '!' || ch == '$' || ch == '(' || ch == ')' || ch == ':' || ch == '=' || ch == '@' ||
			ch == '[' || ch == ']' || ch == '{' || ch == '}' || ch == '|' || ch == '&':
			tokens = append(tokens, token{kind: tokPunct, value: string(ch)})
			i++
			continue
		case ch == '.':
			if i+3 > n || source[i:i+3] != "..." {
				return nil, fmt.Errorf("Unexpected character \".\" at offset %d", i)
			}
			tokens = append(tokens, token{kind: tokPunct, value: "..."})
			i += 3
			continue
		case ch == '"':
			if i+3 <= n && source[i:i+3] == `"""` {
				end := strings.Index(source[i+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("Unterminated block string")
				}
				tokens = append(tokens, token{kind: tokString, value: source[i+3 : i+3+end]})
				i = i + 3 + end + 3
				continue
			}
			j := i + 1
			var sb strings.Builder
			closed := false
			for j < n {
				c := source[j]
				if c == '"' {
					closed = true
					j++
					break
				}
				if c == '\n' || c == '\r' {
					return nil, fmt.Errorf("Unterminated string (a newline inside a string literal)")
				}
				if c == '\\' {
					if j+1 >= n {
						return nil, fmt.Errorf("Unterminated string escape")
					}
					esc := source[j+1]
					switch esc {
					case '"':
						sb.WriteByte('"')
						j += 2
					case '\\':
						sb.WriteByte('\\')
						j += 2
					case '/':
						sb.WriteByte('/')
						j += 2
					case 'b':
						sb.WriteByte('\b')
						j += 2
					case 'f':
						sb.WriteByte('\f')
						j += 2
					case 'n':
						sb.WriteByte('\n')
						j += 2
					case 'r':
						sb.WriteByte('\r')
						j += 2
					case 't':
						sb.WriteByte('\t')
						j += 2
					case 'u':
						hexv := ""
						if j+6 <= n {
							hexv = source[j+2 : j+6]
						}
						if len(hexv) != 4 || !isHex4(hexv) {
							return nil, fmt.Errorf("Invalid unicode escape \"\\u%s\"", hexv)
						}
						code, _ := strconv.ParseInt(hexv, 16, 32)
						sb.WriteRune(rune(code))
						j += 6
					default:
						return nil, fmt.Errorf("Invalid string escape \"\\%c\"", esc)
					}
					continue
				}
				sb.WriteByte(c)
				j++
			}
			if !closed {
				return nil, fmt.Errorf("Unterminated string")
			}
			tokens = append(tokens, token{kind: tokString, value: sb.String()})
			i = j
			continue
		case ch >= '0' && ch <= '9' || ch == '-':
			// -?(?:0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
			start := i
			if ch == '-' {
				if i+1 >= n || source[i+1] < '0' || source[i+1] > '9' {
					return nil, fmt.Errorf("Unexpected character %q at offset %d", string(ch), i)
				}
				i++
			}
			if source[i] == '0' {
				i++
			} else {
				for i < n && source[i] >= '0' && source[i] <= '9' {
					i++
				}
			}
			isFloat := false
			if i+1 < n && source[i] == '.' && source[i+1] >= '0' && source[i+1] <= '9' {
				isFloat = true
				i++
				for i < n && source[i] >= '0' && source[i] <= '9' {
					i++
				}
			}
			if i < n && (source[i] == 'e' || source[i] == 'E') {
				j := i + 1
				if j < n && (source[j] == '+' || source[j] == '-') {
					j++
				}
				if j < n && source[j] >= '0' && source[j] <= '9' {
					isFloat = true
					i = j
					for i < n && source[i] >= '0' && source[i] <= '9' {
						i++
					}
				}
			}
			text := source[start:i]
			kind := tokInt
			if isFloat {
				kind = tokFloat
			}
			tokens = append(tokens, token{kind: kind, value: text})
			continue
		case ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z':
			start := i
			i++
			for i < n && (source[i] == '_' || source[i] >= '0' && source[i] <= '9' || source[i] >= 'A' && source[i] <= 'Z' || source[i] >= 'a' && source[i] <= 'z') {
				i++
			}
			tokens = append(tokens, token{kind: tokName, value: source[start:i]})
			continue
		default:
			return nil, fmt.Errorf("Unexpected character %q at offset %d", string(ch), i)
		}
	}
	tokens = append(tokens, token{kind: tokEOF})
	return tokens, nil
}

func describeToken(t token) string {
	if t.kind == tokEOF {
		return "the end of the document"
	}
	return strconv.Quote(t.value)
}

// ---------------------------------------------------------------- parser

type gqlParser struct {
	pos    int
	tokens []token
}

func (p *gqlParser) peek() token { return p.tokens[p.pos] }
func (p *gqlParser) next() token { t := p.tokens[p.pos]; p.pos++; return t }
func (p *gqlParser) expectPunct(value string) error {
	t := p.next()
	if t.kind != tokPunct || t.value != value {
		return fmt.Errorf("Expected %q but got %s", value, describeToken(t))
	}
	return nil
}

func (p *gqlParser) acceptPunct(value string) bool {
	t := p.peek()
	if t.kind == tokPunct && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) expectName(value ...string) (string, error) {
	t := p.next()
	if t.kind != tokName {
		return "", fmt.Errorf("Expected a name but got %s", describeToken(t))
	}
	if len(value) > 0 && t.value != value[0] {
		return "", fmt.Errorf("Expected %q but got %q", value[0], t.value)
	}
	return t.value, nil
}

func (p *gqlParser) acceptName(value string) bool {
	t := p.peek()
	if t.kind == tokName && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: map[string]*fragmentDef{}}
	for p.peek().kind != tokEOF {
		if p.acceptName("fragment") {
			frag, err := p.parseFragmentDefinition()
			if err != nil {
				return nil, err
			}
			doc.fragments[frag.name] = frag
		} else {
			op, err := p.parseOperation()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, op)
		}
	}
	return doc, nil
}

func (p *gqlParser) parseOperation() (*operation, error) {
	op := &operation{kind: "query"}
	t := p.peek()
	if t.kind == tokName && (t.value == "query" || t.value == "mutation" || t.value == "subscription") {
		op.kind = t.value
		p.next()
		if p.peek().kind == tokName {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			op.name, op.hasName = name, true
		}
	}
	vars, err := p.parseVariableDefinitions()
	if err != nil {
		return nil, err
	}
	op.variables = vars
	if err := p.parseDirectives(); err != nil {
		return nil, err
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selections = sels
	return op, nil
}

func (p *gqlParser) parseVariableDefinitions() ([]*variableDef, error) {
	var defs []*variableDef
	if !p.acceptPunct("(") {
		return defs, nil
	}
	for !p.acceptPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		if err := p.parseType(); err != nil {
			return nil, err
		}
		def := &variableDef{name: name}
		if p.acceptPunct("=") {
			v, err := p.parseValue(true)
			if err != nil {
				return nil, err
			}
			def.defaultValue = v
		}
		if err := p.parseDirectives(); err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
	return defs, nil
}

// parseType consumes a type reference (not retained: no type checking is done).
func (p *gqlParser) parseType() error {
	if p.acceptPunct("[") {
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	p.acceptPunct("!")
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*selection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []*selection
	for !p.acceptPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*selection, error) {
	if p.acceptPunct("...") {
		t := p.peek()
		if t.kind == tokName && t.value != "on" {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.parseDirectives(); err != nil {
				return nil, err
			}
			return &selection{kind: selSpread, name: name}, nil
		}
		sel := &selection{kind: selInline}
		if p.acceptName("on") {
			tc, err := p.expectName()
			if err != nil {
				return nil, err
			}
			sel.typeCondition, sel.hasTypeCond = tc, true
		}
		if err := p.parseDirectives(); err != nil {
			return nil, err
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selections = sels
		return sel, nil
	}
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	sel := &selection{kind: selField}
	if p.acceptPunct(":") {
		sel.alias, sel.hasAlias = name, true
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	sel.name = name
	args, err := p.parseArguments()
	if err != nil {
		return nil, err
	}
	sel.args = args
	if err := p.parseDirectives(); err != nil {
		return nil, err
	}
	if peeked := p.peek(); peeked.kind == tokPunct && peeked.value == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selections = sels
	}
	return sel, nil
}

func (p *gqlParser) parseArguments() ([]*gqlArg, error) {
	var args []*gqlArg
	if !p.acceptPunct("(") {
		return args, nil
	}
	for !p.acceptPunct(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue(false)
		if err != nil {
			return nil, err
		}
		args = append(args, &gqlArg{name: name, value: v})
	}
	return args, nil
}

func (p *gqlParser) parseValue(constant bool) (*valueNode, error) {
	t := p.peek()
	if t.kind == tokPunct && t.value == "$" {
		if constant {
			return nil, fmt.Errorf("A variable is not allowed in a constant value")
		}
		p.next()
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: vVariable, name: name}, nil
	}
	if t.kind == tokInt {
		p.next()
		f, _ := strconv.ParseFloat(t.value, 64)
		return &valueNode{kind: vInt, num: f}, nil
	}
	if t.kind == tokFloat {
		p.next()
		f, _ := strconv.ParseFloat(t.value, 64)
		return &valueNode{kind: vFloat, num: f}, nil
	}
	if t.kind == tokString {
		p.next()
		return &valueNode{kind: vString, str: t.value}, nil
	}
	if t.kind == tokName {
		p.next()
		switch t.value {
		case "true":
			return &valueNode{kind: vBoolean, b: true}, nil
		case "false":
			return &valueNode{kind: vBoolean, b: false}, nil
		case "null":
			return &valueNode{kind: vNull}, nil
		default:
			return &valueNode{kind: vEnum, str: t.value}, nil
		}
	}
	if t.kind == tokPunct && t.value == "[" {
		p.next()
		var values []*valueNode
		for !p.acceptPunct("]") {
			v, err := p.parseValue(constant)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		return &valueNode{kind: vList, values: values}, nil
	}
	if t.kind == tokPunct && t.value == "{" {
		p.next()
		var fields []objectField
		for !p.acceptPunct("}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue(constant)
			if err != nil {
				return nil, err
			}
			fields = append(fields, objectField{name: name, value: v})
		}
		return &valueNode{kind: vObject, fields: fields}, nil
	}
	return nil, fmt.Errorf("Expected a value but got %s", describeToken(t))
}

// parseDirectives parses directives and ignores them (the suite uses none).
func (p *gqlParser) parseDirectives() error {
	for p.acceptPunct("@") {
		if _, err := p.expectName(); err != nil {
			return err
		}
		if _, err := p.parseArguments(); err != nil {
			return err
		}
	}
	return nil
}

func (p *gqlParser) parseFragmentDefinition() (*fragmentDef, error) {
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectName("on"); err != nil {
		return nil, err
	}
	tc, err := p.expectName()
	if err != nil {
		return nil, err
	}
	if err := p.parseDirectives(); err != nil {
		return nil, err
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &fragmentDef{name: name, typeCondition: tc, selections: sels}, nil
}

func parseGQLDocument(source string) (*gqlDocument, error) {
	tokens, err := tokenize(source)
	if err != nil {
		return nil, err
	}
	return (&gqlParser{tokens: tokens}).parseDocument()
}

// ---------------------------------------------------------------- values

// gqlObject is a resolved GraphQL object: its concrete type plus whatever its
// resolvers need. Stored as a map with a "__typename" key.
type gqlObject map[string]any

func resolveValue(node *valueNode, variables map[string]any) any {
	switch node.kind {
	case vVariable:
		if v, ok := variables[node.name]; ok {
			return v
		}
		return nil
	case vInt, vFloat:
		return node.num
	case vString:
		return node.str
	case vBoolean:
		return node.b
	case vNull:
		return nil
	case vEnum:
		return node.str
	case vList:
		out := make([]any, len(node.values))
		for i, v := range node.values {
			out[i] = resolveValue(v, variables)
		}
		return out
	case vObject:
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = resolveValue(f.value, variables)
		}
		return out
	}
	return nil
}

func resolveGQLArgs(args []*gqlArg, variables map[string]any) map[string]any {
	out := map[string]any{}
	for _, arg := range args {
		out[arg.name] = resolveValue(arg.value, variables)
	}
	return out
}

// ---------------------------------------------------------------- validation

// validateOperation returns the first schema error in the operation, or "" when valid.
func validateOperation(doc *gqlDocument, op *operation, rootType string) string {
	return validateSelectionSet(doc, op.selections, rootType)
}

func validateSelectionSet(doc *gqlDocument, selections []*selection, parentType string) string {
	for _, sel := range selections {
		switch sel.kind {
		case selField:
			if sel.name == "__typename" {
				if len(sel.selections) > 0 {
					return `Field "__typename" must not have a selection of subfields.`
				}
				continue
			}
			returnType, ok := fieldTypeLookup[parentType][sel.name]
			if !ok {
				return fmt.Sprintf("Cannot query field %q on type %q.", sel.name, parentType)
			}
			_, isLeaf := fieldTypes[returnType]
			isLeaf = !isLeaf
			if isLeaf {
				if len(sel.selections) > 0 {
					return fmt.Sprintf("Field %q must not have a selection of subfields.", sel.name)
				}
			} else {
				if len(sel.selections) == 0 {
					return fmt.Sprintf("Field %q of type %q must have a selection of subfields.", sel.name, returnType)
				}
				if nested := validateSelectionSet(doc, sel.selections, returnType); nested != "" {
					return nested
				}
			}
		case selInline:
			typ := parentType
			if sel.hasTypeCond {
				typ = sel.typeCondition
				if _, ok := fieldTypes[sel.typeCondition]; !ok {
					return fmt.Sprintf("Unknown type %q.", sel.typeCondition)
				}
			}
			if nested := validateSelectionSet(doc, sel.selections, typ); nested != "" {
				return nested
			}
		case selSpread:
			frag, ok := doc.fragments[sel.name]
			if !ok {
				return fmt.Sprintf("Unknown fragment %q.", sel.name)
			}
			if nested := validateSelectionSet(doc, frag.selections, frag.typeCondition); nested != "" {
				return nested
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------- object builders

func toIssueValue(repo *Repo, issue *Issue) gqlObject {
	return gqlObject{"__typename": "Issue", "id": globalID("Issue", itoa(repo.ID)+":"+itoa(issue.Number)), "repo": repo, "issue": issue}
}

func toPullValue(repo *Repo, pull *Pull) gqlObject {
	return gqlObject{"__typename": "PullRequest", "id": globalID("PullRequest", itoa(repo.ID)+":"+itoa(pull.Number)), "repo": repo, "pull": pull}
}

func globalIDInt(key string) (int, bool) {
	f, ok := jsParseNumber(key)
	if !ok || !isIntegerFloat(f) {
		return 0, false
	}
	return int(f), true
}

func repoFromGlobalID(forge *Forge, id any) *Repo {
	s, ok := id.(string)
	if !ok {
		return nil
	}
	parsed := parseGlobalID(s)
	if parsed == nil || parsed.typ != "Repository" {
		return nil
	}
	n, ok := globalIDInt(parsed.key)
	if !ok {
		return nil
	}
	return forge.GetRepoByID(n)
}

func issueFromGlobalID(forge *Forge, id any) (*Repo, *Issue) {
	s, ok := id.(string)
	if !ok {
		return nil, nil
	}
	parsed := parseGlobalID(s)
	if parsed == nil || parsed.typ != "Issue" {
		return nil, nil
	}
	parts := strings.Split(parsed.key, ":")
	if len(parts) < 2 {
		return nil, nil
	}
	repoID, ok1 := globalIDInt(parts[0])
	number, ok2 := globalIDInt(parts[1])
	if !ok1 || !ok2 {
		return nil, nil
	}
	repo := forge.GetRepoByID(repoID)
	if repo == nil {
		return nil, nil
	}
	issue := repo.Issues[number]
	if issue == nil {
		return nil, nil
	}
	return repo, issue
}

func pullFromGlobalID(forge *Forge, id any) (*Repo, *Pull) {
	s, ok := id.(string)
	if !ok {
		return nil, nil
	}
	parsed := parseGlobalID(s)
	if parsed == nil || parsed.typ != "PullRequest" {
		return nil, nil
	}
	parts := strings.Split(parsed.key, ":")
	if len(parts) < 2 {
		return nil, nil
	}
	repoID, ok1 := globalIDInt(parts[0])
	number, ok2 := globalIDInt(parts[1])
	if !ok1 || !ok2 {
		return nil, nil
	}
	repo := forge.GetRepoByID(repoID)
	if repo == nil {
		return nil, nil
	}
	pull := repo.Pulls[number]
	if pull == nil {
		return nil, nil
	}
	return repo, pull
}

func resolveNode(forge *Forge, id string) gqlObject {
	parsed := parseGlobalID(id)
	if parsed == nil {
		return nil
	}
	switch parsed.typ {
	case "Repository":
		n, ok := globalIDInt(parsed.key)
		if !ok {
			return nil
		}
		repo := forge.GetRepoByID(n)
		if repo == nil {
			return nil
		}
		return gqlObject{"__typename": "Repository", "id": id, "repo": repo}
	case "Issue":
		repo, issue := issueFromGlobalID(forge, id)
		if repo == nil {
			return nil
		}
		return toIssueValue(repo, issue)
	case "PullRequest":
		repo, pull := pullFromGlobalID(forge, id)
		if repo == nil {
			return nil
		}
		return toPullValue(repo, pull)
	case "IssueComment":
		parts := strings.Split(parsed.key, ":")
		if len(parts) < 2 {
			return nil
		}
		repoID, ok1 := globalIDInt(parts[0])
		commentID, ok2 := globalIDInt(parts[1])
		if !ok1 || !ok2 {
			return nil
		}
		repo := forge.GetRepoByID(repoID)
		if repo == nil {
			return nil
		}
		for _, issue := range repo.Issues {
			for _, comment := range issue.Comments {
				if comment.ID == commentID {
					return gqlObject{"__typename": "IssueComment", "id": id, "body": comment.Body}
				}
			}
		}
		return nil
	case "User":
		return gqlObject{"__typename": "User", "login": viewer.Login}
	}
	return nil
}

// ---------------------------------------------------------------- cursors and connections

func encodeCursor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte(itoa(n)))
}

func decodeCursor(cursor string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	n, ok := jsParseInt(string(raw))
	return n, ok
}

func pageInfoOf(hasNextPage bool, endCursor, startCursor any) gqlObject {
	return gqlObject{
		"__typename":  "PageInfo",
		"hasNextPage": hasNextPage, "hasPreviousPage": false,
		"startCursor": startCursor, "endCursor": endCursor,
	}
}

func resolveIssuesConnection(repo *Repo, args map[string]any) gqlObject {
	var issues []*Issue
	for _, issue := range repo.Issues {
		issues = append(issues, issue)
	}
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, s := range states {
			if str, ok := s.(string); ok {
				wanted[str] = true
			}
		}
		filtered := issues[:0]
		for _, issue := range issues {
			state := "CLOSED"
			if issue.State == "open" {
				state = "OPEN"
			}
			if wanted[state] {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	direction := 1
	byUpdated := false
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		if orderBy["direction"] == "DESC" {
			direction = -1
		}
		byUpdated = orderBy["field"] == "UPDATED_AT"
	}
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		av, bv := a.Number, b.Number
		if byUpdated {
			av, bv = a.UpdatedSeq, b.UpdatedSeq
		}
		if direction < 0 {
			return av > bv
		}
		return av < bv
	})
	totalCount := len(issues)
	if after, ok := args["after"].(string); ok && after != "" {
		if num, ok := decodeCursor(after); ok {
			idx := -1
			for i, issue := range issues {
				if issue.Number == num {
					idx = i
					break
				}
			}
			if idx >= 0 {
				issues = issues[idx+1:]
			}
		}
	}
	page := issues
	hasNextPage := false
	if first, ok := args["first"].(float64); ok {
		n := int(first)
		if n < 0 {
			n = 0
		}
		if n < len(issues) {
			page = issues[:n]
		}
		hasNextPage = len(issues) > len(page)
	}
	var endCursor, startCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].Number)
		startCursor = encodeCursor(page[0].Number)
	}
	return gqlObject{
		"__typename": "IssueConnection", "repo": repo, "issues": page,
		"totalCount": totalCount, "hasNextPage": hasNextPage,
		"endCursor": endCursor, "startCursor": startCursor,
	}
}

func pullState(pull *Pull) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func resolvePullsConnection(repo *Repo, args map[string]any) gqlObject {
	var pulls []*Pull
	for _, pull := range repo.Pulls {
		pulls = append(pulls, pull)
	}
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, s := range states {
			if str, ok := s.(string); ok {
				wanted[str] = true
			}
		}
		filtered := pulls[:0]
		for _, pull := range pulls {
			if wanted[pullState(pull)] {
				filtered = append(filtered, pull)
			}
		}
		pulls = filtered
	}
	direction := 1
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		if orderBy["direction"] == "DESC" {
			direction = -1
		}
	}
	sort.SliceStable(pulls, func(i, j int) bool {
		if direction < 0 {
			return pulls[i].Number > pulls[j].Number
		}
		return pulls[i].Number < pulls[j].Number
	})
	totalCount := len(pulls)
	if after, ok := args["after"].(string); ok && after != "" {
		if num, ok := decodeCursor(after); ok {
			idx := -1
			for i, pull := range pulls {
				if pull.Number == num {
					idx = i
					break
				}
			}
			if idx >= 0 {
				pulls = pulls[idx+1:]
			}
		}
	}
	page := pulls
	hasNextPage := false
	if first, ok := args["first"].(float64); ok {
		n := int(first)
		if n < 0 {
			n = 0
		}
		if n < len(pulls) {
			page = pulls[:n]
		}
		hasNextPage = len(pulls) > len(page)
	}
	var endCursor, startCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].Number)
		startCursor = encodeCursor(page[0].Number)
	}
	return gqlObject{
		"__typename": "PullRequestConnection", "repo": repo, "pulls": page,
		"totalCount": totalCount, "hasNextPage": hasNextPage,
		"endCursor": endCursor, "startCursor": startCursor,
	}
}

// resolveFilesConnection returns the pull request's changed files:
// the diff between its merge-base and its head.
func resolveFilesConnection(forge *Forge, repo *Repo, pull *Pull, args map[string]any) gqlObject {
	lookup := forge.Lookup(repo)
	headSha, baseSha := forge.PullShas(repo, pull)
	headCommit := lookup(headSha)
	baseCommit := lookup(baseSha)
	var diffs []DiffEntry
	if headCommit != nil && baseCommit != nil {
		base := mergeBase(lookup, baseSha, headSha)
		baseTree := ""
		if base != "" {
			if o := lookup(base); o != nil {
				baseTree = parseCommit(o.Data).Tree
			}
		}
		diffs = diffTrees(lookup, baseTree, parseCommit(headCommit.Data).Tree)
	}
	totalCount := len(diffs)
	page := diffs
	hasNextPage := false
	if first, ok := args["first"].(float64); ok {
		n := int(first)
		if n < 0 {
			n = 0
		}
		if n < len(diffs) {
			page = diffs[:n]
		}
		hasNextPage = len(diffs) > len(page)
	}
	return gqlObject{
		"__typename": "PullRequestChangedFileConnection", "files": page,
		"totalCount": totalCount, "hasNextPage": hasNextPage,
		"endCursor": nil, "startCursor": nil,
	}
}

// ---------------------------------------------------------------- field resolvers

func resolveQuery(forge *Forge, field string, args map[string]any) any {
	switch field {
	case "viewer":
		return gqlObject{"__typename": "User", "login": viewer.Login}
	case "repository":
		owner, ok1 := args["owner"].(string)
		name, ok2 := args["name"].(string)
		if !ok1 || !ok2 {
			return nil
		}
		repo := forge.GetRepo(owner, name)
		if repo == nil {
			return nil
		}
		return gqlObject{"__typename": "Repository", "id": globalID("Repository", itoa(repo.ID)), "repo": repo}
	case "node":
		id, ok := args["id"].(string)
		if !ok {
			return nil
		}
		return resolveNode(forge, id)
	case "__type":
		name, ok := args["name"].(string)
		if !ok {
			return nil
		}
		if _, ok := typeKind[name]; !ok {
			return nil
		}
		return gqlObject{"__typename": "__Type", "name": name}
	}
	return nil
}

func resolveMutation(forge *Forge, field string, args map[string]any) any {
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch field {
	case "createIssue":
		repo := repoFromGlobalID(forge, input["repositoryId"])
		if repo == nil {
			return gqlObject{"__typename": "CreateIssuePayload", "issue": nil}
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		issue := forge.CreateIssue(repo, title, body, nil)
		return gqlObject{"__typename": "CreateIssuePayload", "issue": toIssueValue(repo, issue)}
	case "closeIssue":
		repo, issue := issueFromGlobalID(forge, input["issueId"])
		if repo == nil {
			return gqlObject{"__typename": "CloseIssuePayload", "issue": nil}
		}
		issue.State = "closed"
		issue.UpdatedSeq = forge.NextSeq()
		repo.touch()
		return gqlObject{"__typename": "CloseIssuePayload", "issue": toIssueValue(repo, issue)}
	case "createPullRequest":
		repo := repoFromGlobalID(forge, input["repositoryId"])
		if repo == nil {
			return gqlObject{"__typename": "CreatePullRequestPayload", "pullRequest": nil}
		}
		headRef, ok1 := input["headRefName"].(string)
		baseRef, ok2 := input["baseRefName"].(string)
		if !ok1 || !ok2 {
			return gqlObject{"__typename": "CreatePullRequestPayload", "pullRequest": nil}
		}
		headSha, okH := repo.Refs["refs/heads/"+headRef]
		baseSha, okB := repo.Refs["refs/heads/"+baseRef]
		if !okH || !okB {
			return gqlObject{"__typename": "CreatePullRequestPayload", "pullRequest": nil}
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		pull := forge.CreatePull(repo, title, body, headRef, baseRef, headSha, baseSha)
		return gqlObject{"__typename": "CreatePullRequestPayload", "pullRequest": toPullValue(repo, pull)}
	case "mergePullRequest":
		repo, pull := pullFromGlobalID(forge, input["pullRequestId"])
		if repo == nil {
			return gqlObject{"__typename": "MergePullRequestPayload", "pullRequest": nil}
		}
		method := "squash"
		if input["mergeMethod"] == "MERGE" {
			method = "merge"
		}
		forge.MergePull(repo, pull, method)
		return gqlObject{"__typename": "MergePullRequestPayload", "pullRequest": toPullValue(repo, pull)}
	case "addComment":
		body, _ := input["body"].(string)
		repo, issue := issueFromGlobalID(forge, input["subjectId"])
		if repo == nil {
			return gqlObject{"__typename": "AddCommentPayload", "commentEdge": nil}
		}
		comment := forge.CreateComment(repo, issue, body)
		node := gqlObject{
			"__typename": "IssueComment",
			"id":         globalID("IssueComment", itoa(repo.ID)+":"+itoa(comment.ID)),
			"body":       comment.Body,
		}
		return gqlObject{
			"__typename":  "AddCommentPayload",
			"commentEdge": gqlObject{"__typename": "IssueCommentEdge", "node": node, "cursor": encodeCursor(comment.ID)},
		}
	}
	return nil
}

func resolveUser(parent gqlObject, field string) any {
	login, _ := parent["login"].(string)
	isViewer := login == viewer.Login
	switch field {
	case "id":
		if isViewer {
			return globalID("User", itoa(viewer.ID))
		}
		return globalID("User", login)
	case "login":
		return login
	case "name":
		if isViewer {
			return viewer.Name
		}
		return login
	case "email":
		if isViewer {
			return viewer.Email
		}
		return ""
	}
	return nil
}

func resolveRepository(forge *Forge, parent gqlObject, field string, args map[string]any) any {
	repo := parent["repo"].(*Repo)
	switch field {
	case "id":
		return parent["id"]
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name
	case "owner":
		return gqlObject{"__typename": "User", "login": repo.Owner}
	case "defaultBranchRef":
		if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; !ok {
			return nil
		}
		return gqlObject{"__typename": "Ref", "name": repo.DefaultBranch}
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "issue":
		number, ok := args["number"].(float64)
		if !ok {
			return nil
		}
		issue := repo.Issues[int(number)]
		if issue == nil {
			return nil
		}
		return toIssueValue(repo, issue)
	case "issues":
		return resolveIssuesConnection(repo, args)
	case "pullRequests":
		return resolvePullsConnection(repo, args)
	case "pullRequest":
		number, ok := args["number"].(float64)
		if !ok {
			return nil
		}
		pull := repo.Pulls[int(number)]
		if pull == nil {
			return nil
		}
		return toPullValue(repo, pull)
	}
	return nil
}

func resolveIssue(parent gqlObject, field string) any {
	issue := parent["issue"].(*Issue)
	switch field {
	case "id":
		return parent["id"]
	case "number":
		return issue.Number
	case "title":
		return issue.Title
	case "state":
		if issue.State == "open" {
			return "OPEN"
		}
		return "CLOSED"
	case "body":
		return issue.Body
	}
	return nil
}

func resolvePullRequest(forge *Forge, parent gqlObject, field string, args map[string]any) any {
	repo := parent["repo"].(*Repo)
	pull := parent["pull"].(*Pull)
	switch field {
	case "id":
		return parent["id"]
	case "number":
		return pull.Number
	case "title":
		return pull.Title
	case "state":
		return pullState(pull)
	case "merged":
		return pull.Merged
	case "headRefName":
		return pull.HeadRef
	case "baseRefName":
		return pull.BaseRef
	case "mergeable":
		return forge.PullMergeable(repo, pull)
	case "files":
		return resolveFilesConnection(forge, repo, pull, args)
	}
	return nil
}

func resolveIssueConnection(parent gqlObject, field string) any {
	switch field {
	case "nodes":
		repo := parent["repo"].(*Repo)
		issues := parent["issues"].([]*Issue)
		out := make([]any, len(issues))
		for i, issue := range issues {
			out[i] = toIssueValue(repo, issue)
		}
		return out
	case "pageInfo":
		return pageInfoOf(parent["hasNextPage"].(bool), parent["endCursor"], parent["startCursor"])
	case "totalCount":
		return parent["totalCount"]
	}
	return nil
}

func resolvePullConnection(parent gqlObject, field string) any {
	switch field {
	case "nodes":
		repo := parent["repo"].(*Repo)
		pulls := parent["pulls"].([]*Pull)
		out := make([]any, len(pulls))
		for i, pull := range pulls {
			out[i] = toPullValue(repo, pull)
		}
		return out
	case "pageInfo":
		return pageInfoOf(parent["hasNextPage"].(bool), parent["endCursor"], parent["startCursor"])
	case "totalCount":
		return parent["totalCount"]
	}
	return nil
}

func resolveFileConnection(parent gqlObject, field string) any {
	switch field {
	case "nodes":
		files := parent["files"].([]DiffEntry)
		out := make([]any, len(files))
		for i, file := range files {
			changeType := "MODIFIED"
			if file.Status == "added" {
				changeType = "ADDED"
			} else if file.Status == "removed" {
				changeType = "REMOVED"
			}
			out[i] = gqlObject{
				"__typename": "PullRequestChangedFile",
				"path":       file.Path, "additions": file.Additions,
				"deletions": file.Deletions, "changeType": changeType,
			}
		}
		return out
	case "pageInfo":
		return pageInfoOf(parent["hasNextPage"].(bool), parent["endCursor"], parent["startCursor"])
	case "totalCount":
		return parent["totalCount"]
	}
	return nil
}

func resolveChangedFile(parent gqlObject, field string) any {
	switch field {
	case "path":
		return parent["path"]
	case "additions":
		return parent["additions"]
	case "deletions":
		return parent["deletions"]
	case "changeType":
		return parent["changeType"]
	}
	return nil
}

func resolvePageInfo(parent gqlObject, field string) any {
	switch field {
	case "hasNextPage":
		return parent["hasNextPage"]
	case "hasPreviousPage":
		return parent["hasPreviousPage"]
	case "startCursor":
		return parent["startCursor"]
	case "endCursor":
		return parent["endCursor"]
	}
	return nil
}

func resolveIntrospectionType(parent gqlObject, field string) any {
	name, _ := parent["name"].(string)
	switch field {
	case "name":
		return name
	case "kind":
		if kind, ok := typeKind[name]; ok {
			return kind
		}
		return nil
	case "description":
		return nil
	case "fields":
		fields, ok := fieldTypes[name]
		if !ok {
			return nil
		}
		// Meta-fields (__type) are not part of a type's introspected field list.
		var out []any
		for _, f := range fields {
			if strings.HasPrefix(f.name, "__") {
				continue
			}
			out = append(out, gqlObject{"__typename": "__Field", "name": f.name})
		}
		return out
	case "enumValues":
		values, ok := typeEnumValues[name]
		if !ok {
			return nil
		}
		out := make([]any, len(values))
		for i, v := range values {
			out[i] = gqlObject{"__typename": "__EnumValue", "name": v}
		}
		return out
	}
	return nil
}

func resolveGQLField(forge *Forge, parentType string, parent gqlObject, field *selection, variables map[string]any) any {
	if field.name == "__typename" {
		return parent["__typename"]
	}
	args := resolveGQLArgs(field.args, variables)
	switch parentType {
	case "Query":
		return resolveQuery(forge, field.name, args)
	case "Mutation":
		return resolveMutation(forge, field.name, args)
	case "User":
		return resolveUser(parent, field.name)
	case "Repository":
		return resolveRepository(forge, parent, field.name, args)
	case "Issue":
		return resolveIssue(parent, field.name)
	case "PullRequest":
		return resolvePullRequest(forge, parent, field.name, args)
	case "IssueConnection":
		return resolveIssueConnection(parent, field.name)
	case "PullRequestConnection":
		return resolvePullConnection(parent, field.name)
	case "PullRequestChangedFileConnection":
		return resolveFileConnection(parent, field.name)
	case "PullRequestChangedFile":
		return resolveChangedFile(parent, field.name)
	case "PageInfo":
		return resolvePageInfo(parent, field.name)
	case "Ref":
		if field.name == "name" {
			return parent["name"]
		}
		return nil
	case "IssueComment":
		if field.name == "id" {
			return parent["id"]
		}
		if field.name == "body" {
			return parent["body"]
		}
		return nil
	case "IssueCommentEdge":
		if field.name == "node" {
			return parent["node"]
		}
		if field.name == "cursor" {
			return parent["cursor"]
		}
		return nil
	case "CreateIssuePayload", "CloseIssuePayload":
		if field.name == "issue" {
			return parent["issue"]
		}
		return nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field.name == "pullRequest" {
			return parent["pullRequest"]
		}
		return nil
	case "AddCommentPayload":
		if field.name == "commentEdge" {
			return parent["commentEdge"]
		}
		return nil
	case "__Type":
		return resolveIntrospectionType(parent, field.name)
	case "__Field", "__EnumValue":
		if field.name == "name" {
			return parent["name"]
		}
		if field.name == "description" {
			return nil
		}
		return nil
	case "Node":
		if field.name == "id" {
			return parent["id"]
		}
		return nil
	}
	return nil
}

// ---------------------------------------------------------------- execution

func executeSelectionSet(forge *Forge, doc *gqlDocument, selections []*selection, parent gqlObject, parentType string, variables map[string]any) map[string]any {
	result := map[string]any{}
	for _, sel := range selections {
		switch sel.kind {
		case selField:
			key := sel.name
			if sel.hasAlias {
				key = sel.alias
			}
			value := resolveGQLField(forge, parentType, parent, sel, variables)
			if len(sel.selections) == 0 {
				result[key] = value
			} else if value == nil {
				result[key] = nil
			} else if arr, ok := value.([]any); ok {
				out := make([]any, len(arr))
				for i, item := range arr {
					if item == nil {
						out[i] = nil
						continue
					}
					itemObject := item.(gqlObject)
					out[i] = executeSelectionSet(forge, doc, sel.selections, itemObject, itemObject["__typename"].(string), variables)
				}
				result[key] = out
			} else {
				valueObject := value.(gqlObject)
				result[key] = executeSelectionSet(forge, doc, sel.selections, valueObject, valueObject["__typename"].(string), variables)
			}
		case selInline:
			if !sel.hasTypeCond || sel.typeCondition == parent["__typename"] {
				nested := executeSelectionSet(forge, doc, sel.selections, parent, parentType, variables)
				for k, v := range nested {
					result[k] = v
				}
			}
		case selSpread:
			if frag, ok := doc.fragments[sel.name]; ok && frag.typeCondition == parent["__typename"] {
				nested := executeSelectionSet(forge, doc, frag.selections, parent, parentType, variables)
				for k, v := range nested {
					result[k] = v
				}
			}
		}
	}
	return result
}

// ---------------------------------------------------------------- entry point

type gqlResponse struct {
	Data   any                `json:"data"`
	Errors []gqlErrorResponse `json:"errors,omitempty"`
}

type gqlErrorResponse struct {
	Message string `json:"message"`
}

func gqlError(message string) gqlResponse {
	return gqlResponse{Data: nil, Errors: []gqlErrorResponse{{Message: message}}}
}

func executeOneGQL(forge *Forge, request any) (resp gqlResponse) {
	defer func() {
		if r := recover(); r != nil {
			resp = gqlError(fmt.Sprintf("%v", r))
		}
	}()
	req, ok := request.(map[string]any)
	if !ok {
		return gqlError("A GraphQL request must be an object with a query string.")
	}
	query, isStr := req["query"].(string)
	if !isStr {
		return gqlError("A GraphQL request must include a query string.")
	}
	doc, err := parseGQLDocument(query)
	if err != nil {
		return gqlError(err.Error())
	}
	var op *operation
	if operationName, ok := req["operationName"].(string); ok && operationName != "" {
		for _, candidate := range doc.operations {
			if candidate.hasName && candidate.name == operationName {
				op = candidate
				break
			}
		}
		if op == nil {
			return gqlError(fmt.Sprintf("Unknown operation named %q.", operationName))
		}
	} else if len(doc.operations) == 1 {
		op = doc.operations[0]
	} else if len(doc.operations) == 0 {
		return gqlError("The document has no operations.")
	} else {
		return gqlError("Must provide an operation name when the document has multiple operations.")
	}
	provided := map[string]any{}
	if vars, ok := req["variables"].(map[string]any); ok {
		provided = vars
	}
	effective := map[string]any{}
	for _, def := range op.variables {
		if def.defaultValue != nil {
			effective[def.name] = resolveValue(def.defaultValue, provided)
		}
	}
	for k, v := range provided {
		effective[k] = v
	}
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	if validationError := validateOperation(doc, op, rootType); validationError != "" {
		return gqlError(validationError)
	}
	root := gqlObject{"__typename": rootType}
	data := executeSelectionSet(forge, doc, op.selections, root, rootType, effective)
	return gqlResponse{Data: data}
}

// handleGraphQL serves POST /api/graphql. body is the parsed JSON request:
// one operation document, or an array of them (a batch, answered in order).
// Auth and JSON parsing happen in the caller.
func handleGraphQL(forge *Forge, body any) any {
	if arr, ok := body.([]any); ok {
		out := make([]gqlResponse, len(arr))
		for i, item := range arr {
			out[i] = executeOneGQL(forge, item)
		}
		return out
	}
	return executeOneGQL(forge, body)
}
