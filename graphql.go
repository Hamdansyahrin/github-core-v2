// GraphQL v4 at POST /api/graphql: a small hand-rolled parser and executor for exactly the
// document shapes gh and github-mcp-server send — viewer and repository reads, issue and
// pull-request connections with cursor pagination, the create/close/merge/comment mutations,
// Relay node(id:) lookups, __type introspection, operation batching (a JSON array of documents
// answered in order), and variables, inline enum lists and input objects. An unknown field is a
// 200 with an `errors` array naming it, never an HTTP error; an unknown repository is
// data.repository = null with no errors.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------------
// lexer

type gqlToken struct {
	kind  string // 'name' | 'string' | 'int' | 'float' | 'punct'
	text  string
	value float64
}

func isNameStart(ch byte) bool {
	return ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'
}

func isNameChar(ch byte) bool {
	return isNameStart(ch) || ch >= '0' && ch <= '9'
}

func lexGraphQL(source string) []gqlToken {
	tokens := []gqlToken{}
	index := 0
	for index < len(source) {
		ch := source[index]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' || ch == 0xef && index+2 < len(source) && source[index+1] == 0xbb && source[index+2] == 0xbf {
			if ch == 0xef {
				index += 3
			} else {
				index++
			}
			continue
		}
		if ch == '#' {
			for index < len(source) && source[index] != '\n' {
				index++
			}
			continue
		}
		if strings.HasPrefix(source[index:], "...") {
			tokens = append(tokens, gqlToken{kind: "punct", text: "..."})
			index += 3
			continue
		}
		if strings.ContainsRune("!$&():=@[]{}|", rune(ch)) {
			tokens = append(tokens, gqlToken{kind: "punct", text: string(ch)})
			index++
			continue
		}
		if ch == '"' {
			if strings.HasPrefix(source[index:], "\"\"\"") {
				end := strings.Index(source[index+3:], "\"\"\"")
				if end < 0 {
					panic("unterminated block string")
				}
				tokens = append(tokens, gqlToken{kind: "string", text: source[index+3 : index+3+end]})
				index += 3 + end + 3
				continue
			}
			var value strings.Builder
			index++
			for {
				if index >= len(source) {
					panic("unterminated string")
				}
				current := source[index]
				if current == '"' {
					index++
					break
				}
				if current == '\\' {
					if index+1 >= len(source) {
						panic("unterminated string")
					}
					escape := source[index+1]
					switch escape {
					case '"':
						value.WriteByte('"')
						index += 2
					case '\\':
						value.WriteByte('\\')
						index += 2
					case '/':
						value.WriteByte('/')
						index += 2
					case 'b':
						value.WriteByte('\b')
						index += 2
					case 'f':
						value.WriteByte('\f')
						index += 2
					case 'n':
						value.WriteByte('\n')
						index += 2
					case 'r':
						value.WriteByte('\r')
						index += 2
					case 't':
						value.WriteByte('\t')
						index += 2
					case 'u':
						if index+6 > len(source) {
							panic("bad unicode escape")
						}
						code, err := strconv.ParseInt(source[index+2:index+6], 16, 32)
						if err != nil {
							panic("bad unicode escape")
						}
						value.WriteRune(rune(code))
						index += 6
					default:
						panic(fmt.Sprintf("bad escape \\%c", escape))
					}
					continue
				}
				value.WriteByte(current)
				index++
			}
			tokens = append(tokens, gqlToken{kind: "string", text: value.String()})
			continue
		}
		if isNameStart(ch) {
			start := index
			for index < len(source) && isNameChar(source[index]) {
				index++
			}
			tokens = append(tokens, gqlToken{kind: "name", text: source[start:index]})
			continue
		}
		if ch == '-' || ch >= '0' && ch <= '9' {
			start := index
			if source[index] == '-' {
				index++
			}
			for index < len(source) && source[index] >= '0' && source[index] <= '9' {
				index++
			}
			isFloat := false
			if index+1 < len(source) && source[index] == '.' && source[index+1] >= '0' && source[index+1] <= '9' {
				isFloat = true
				index++
				for index < len(source) && source[index] >= '0' && source[index] <= '9' {
					index++
				}
			}
			if index < len(source) && (source[index] == 'e' || source[index] == 'E') {
				probe := index + 1
				if probe < len(source) && (source[probe] == '+' || source[probe] == '-') {
					probe++
				}
				if probe < len(source) && source[probe] >= '0' && source[probe] <= '9' {
					isFloat = true
					index = probe
					for index < len(source) && source[index] >= '0' && source[index] <= '9' {
						index++
					}
				}
			}
			text := source[start:index]
			number, err := strconv.ParseFloat(text, 64)
			if err != nil {
				panic(fmt.Sprintf("bad number at %d", start))
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			tokens = append(tokens, gqlToken{kind: kind, text: text, value: number})
			continue
		}
		panic(fmt.Sprintf("unexpected character %q", string(ch)))
	}
	return tokens
}

// ---------------------------------------------------------------------------------------------
// parser

type valueNode struct {
	kind   string // 'variable' | 'string' | 'number' | 'boolean' | 'enum' | 'null' | 'list' | 'object'
	text   string
	number float64
	flag   bool
	values []*valueNode
	fields []valueField
}

type valueField struct {
	name  string
	value *valueNode
}

type fieldNode struct {
	alias        string
	hasAlias     bool
	name         string
	args         []valueField
	selectionSet []selectionNode
	hasSelection bool
}

type selectionNode struct {
	kind         string // 'field' | 'inline' | 'spread'
	field        *fieldNode
	typeCond     string
	hasTypeCond  bool
	name         string
	selectionSet []selectionNode
}

type operationNode struct {
	opType       string // 'query' | 'mutation'
	name         string
	hasName      bool
	selectionSet []selectionNode
}

type fragmentDef struct {
	typeCondition string
	selectionSet  []selectionNode
}

type documentNode struct {
	operations []operationNode
	fragments  map[string]fragmentDef
}

type gqlParser struct {
	tokens []gqlToken
	index  int
}

func (p *gqlParser) peek() *gqlToken {
	if p.index < len(p.tokens) {
		return &p.tokens[p.index]
	}
	return nil
}

func (p *gqlParser) next() gqlToken {
	if p.index >= len(p.tokens) {
		panic("unexpected end of document")
	}
	token := p.tokens[p.index]
	p.index++
	return token
}

func (p *gqlParser) atPunct(value string) bool {
	token := p.peek()
	return token != nil && token.kind == "punct" && token.text == value
}

func (p *gqlParser) eatPunct(value string) bool {
	if p.atPunct(value) {
		p.index++
		return true
	}
	return false
}

func (p *gqlParser) expectPunct(value string) {
	if !p.eatPunct(value) {
		panic(fmt.Sprintf("expected '%s'", value))
	}
}

func (p *gqlParser) expectName() string {
	token := p.next()
	if token.kind != "name" {
		panic("expected a name")
	}
	return token.text
}

func (p *gqlParser) parseDocument() documentNode {
	document := documentNode{operations: []operationNode{}, fragments: map[string]fragmentDef{}}
	for p.peek() != nil {
		if p.atPunct("{") {
			document.operations = append(document.operations, operationNode{opType: "query", selectionSet: p.parseSelectionSet()})
			continue
		}
		keyword := p.expectName()
		if keyword == "query" || keyword == "mutation" {
			operation := operationNode{opType: keyword}
			if token := p.peek(); token != nil && token.kind == "name" {
				operation.name = p.expectName()
				operation.hasName = true
			}
			if p.atPunct("(") {
				p.parseVariableDefinitions()
			}
			p.skipDirectives()
			operation.selectionSet = p.parseSelectionSet()
			document.operations = append(document.operations, operation)
		} else if keyword == "fragment" {
			fragmentName := p.expectName()
			if on := p.expectName(); on != "on" {
				panic("expected `on` in a fragment definition")
			}
			typeCondition := p.expectName()
			p.skipDirectives()
			document.fragments[fragmentName] = fragmentDef{typeCondition: typeCondition, selectionSet: p.parseSelectionSet()}
		} else {
			panic(fmt.Sprintf("unexpected definition %s", keyword))
		}
	}
	return document
}

func (p *gqlParser) parseVariableDefinitions() {
	p.expectPunct("(")
	for !p.eatPunct(")") {
		p.expectPunct("$")
		p.expectName()
		p.expectPunct(":")
		p.parseType()
		if p.eatPunct("=") {
			p.parseValue()
		}
		p.skipDirectives()
	}
}

func (p *gqlParser) parseType() {
	if p.eatPunct("[") {
		p.parseType()
		p.expectPunct("]")
	} else {
		p.expectName()
	}
	p.eatPunct("!")
}

func (p *gqlParser) skipDirectives() {
	for p.eatPunct("@") {
		p.expectName()
		if p.atPunct("(") {
			p.parseArguments()
		}
	}
}

func (p *gqlParser) parseArguments() []valueField {
	args := []valueField{}
	p.expectPunct("(")
	for !p.eatPunct(")") {
		name := p.expectName()
		p.expectPunct(":")
		args = append(args, valueField{name: name, value: p.parseValue()})
	}
	return args
}

func (p *gqlParser) parseSelectionSet() []selectionNode {
	selections := []selectionNode{}
	p.expectPunct("{")
	for !p.eatPunct("}") {
		if p.eatPunct("...") {
			token := p.peek()
			if token != nil && token.kind == "name" && token.text == "on" {
				p.next()
				typeCondition := p.expectName()
				p.skipDirectives()
				selections = append(selections, selectionNode{kind: "inline", typeCond: typeCondition, hasTypeCond: true, selectionSet: p.parseSelectionSet()})
			} else if token != nil && token.kind == "name" {
				name := p.expectName()
				p.skipDirectives()
				selections = append(selections, selectionNode{kind: "spread", name: name})
			} else {
				panic("expected a fragment condition or name after ...")
			}
			continue
		}
		field := &fieldNode{}
		name := p.expectName()
		if p.eatPunct(":") {
			field.alias = name
			field.hasAlias = true
			name = p.expectName()
		}
		field.name = name
		if p.atPunct("(") {
			field.args = p.parseArguments()
		}
		p.skipDirectives()
		if p.atPunct("{") {
			field.selectionSet = p.parseSelectionSet()
			field.hasSelection = true
		}
		selections = append(selections, selectionNode{kind: "field", field: field})
	}
	return selections
}

func (p *gqlParser) parseValue() *valueNode {
	token := p.next()
	switch token.kind {
	case "string":
		return &valueNode{kind: "string", text: token.text}
	case "int", "float":
		return &valueNode{kind: "number", number: token.value}
	case "name":
		switch token.text {
		case "true":
			return &valueNode{kind: "boolean", flag: true}
		case "false":
			return &valueNode{kind: "boolean", flag: false}
		case "null":
			return &valueNode{kind: "null"}
		}
		return &valueNode{kind: "enum", text: token.text}
	case "punct":
		switch token.text {
		case "$":
			return &valueNode{kind: "variable", text: p.expectName()}
		case "[":
			node := &valueNode{kind: "list", values: []*valueNode{}}
			for !p.eatPunct("]") {
				node.values = append(node.values, p.parseValue())
			}
			return node
		case "{":
			node := &valueNode{kind: "object", fields: []valueField{}}
			for !p.eatPunct("}") {
				name := p.expectName()
				p.expectPunct(":")
				node.fields = append(node.fields, valueField{name: name, value: p.parseValue()})
			}
			return node
		}
	}
	panic("unexpected value token")
}

// ---------------------------------------------------------------------------------------------
// executor

// A tagged value: the GraphQL type name plus the store record it wraps.
type gObj struct {
	typeName string
	source   any
}

func gql(typeName string, source any) gObj {
	return gObj{typeName: typeName, source: source}
}

type gqlError struct{ message string }

func (e gqlError) Error() string { return e.message }

type gqlCtx struct {
	forge     *Forge
	variables map[string]any
	fragments map[string]fragmentDef
	errors    []any
}

func evalValue(node *valueNode, variables map[string]any) any {
	switch node.kind {
	case "variable":
		if value, ok := variables[node.text]; ok {
			return value
		}
		return nil
	case "string", "enum":
		return node.text
	case "number":
		return node.number
	case "boolean":
		return node.flag
	case "null":
		return nil
	case "list":
		out := []any{}
		for _, value := range node.values {
			out = append(out, evalValue(value, variables))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, field := range node.fields {
			out[field.name] = evalValue(field.value, variables)
		}
		return out
	}
	return nil
}

func argsOf(field *fieldNode, ctx *gqlCtx) map[string]any {
	out := map[string]any{}
	for _, arg := range field.args {
		out[arg.name] = evalValue(arg.value, ctx.variables)
	}
	return out
}

func asInt(value any) *int {
	switch number := value.(type) {
	case float64:
		result := int(number)
		return &result
	case json.Number:
		parsed, err := number.Float64()
		if err != nil {
			return nil
		}
		result := int(parsed)
		return &result
	case int:
		return &number
	}
	return nil
}

func asString(value any) *string {
	if text, ok := value.(string); ok {
		return &text
	}
	return nil
}

func inputOf(args map[string]any) map[string]any {
	if record := asRecord(args["input"]); record != nil {
		return record
	}
	return args
}

// JS Number(text) for global-id parts: a non-number reads as "no such id" (-1).
func idPartNumber(text string) int {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return -1
	}
	return int(parsed)
}

// ---------------------------------------------------------------------------------------------
// connections and cursors

func cursorFor(repo *RepoRec, number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d:%d", repo.ID, number)))
}

func cursorNumber(cursor string) *int {
	decoded, ok := base64LenientDecode(cursor)
	if !ok {
		return nil
	}
	text := string(decoded)
	if !strings.HasPrefix(text, "cursor:") {
		return nil
	}
	parts := strings.Split(text[len("cursor:"):], ":")
	if len(parts) != 2 || !allDigits(parts[0]) || !allDigits(parts[1]) {
		return nil
	}
	number, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}
	return &number
}

type pageInfo struct {
	hasNextPage     bool
	hasPreviousPage bool
	endCursor       *string
	startCursor     *string
	totalCount      int
}

func paginateNumbers(repo *RepoRec, ordered []int, args map[string]any) ([]int, pageInfo) {
	first := asInt(args["first"])
	after := asString(args["after"])
	start := 0
	if after != nil && *after != "" {
		if afterNumber := cursorNumber(*after); afterNumber != nil {
			for index, number := range ordered {
				if number == *afterNumber {
					start = index + 1
					break
				}
			}
		}
	}
	limit := len(ordered)
	if first != nil && *first >= 0 {
		limit = *first
	}
	end := start + limit
	if end > len(ordered) {
		end = len(ordered)
	}
	items := ordered[start:end]
	info := pageInfo{
		hasNextPage:     start+len(items) < len(ordered),
		hasPreviousPage: start > 0,
		totalCount:      len(ordered),
	}
	if len(items) > 0 {
		endCursor := cursorFor(repo, items[len(items)-1])
		startCursor := cursorFor(repo, items[0])
		info.endCursor = &endCursor
		info.startCursor = &startCursor
	}
	return items, info
}

type issueConnSource struct {
	repo  *RepoRec
	items []*IssueRec
	page  pageInfo
}

type pullConnSource struct {
	repo  *RepoRec
	items []*PullRec
	page  pageInfo
}

type issueSource struct {
	repo  *RepoRec
	issue *IssueRec
}

type pullSource struct {
	repo *RepoRec
	pull *PullRec
}

type refSource struct {
	repo *RepoRec
	name string
}

func issuesConnection(repo *RepoRec, args map[string]any) gObj {
	list := []*IssueRec{}
	for _, issue := range repo.Issues {
		list = append(list, issue)
	}
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, state := range states {
			wanted[strings.ToUpper(fmt.Sprint(state))] = true
		}
		filtered := list[:0]
		for _, issue := range list {
			if wanted[strings.ToUpper(issue.State)] {
				filtered = append(filtered, issue)
			}
		}
		list = filtered
	}
	direction := 1
	field := "CREATED_AT"
	if orderBy := asRecord(args["orderBy"]); orderBy != nil {
		if value, ok := orderBy["direction"].(string); ok && strings.ToUpper(value) == "DESC" {
			direction = -1
		} else if orderBy["direction"] != nil {
			if fmt.Sprint(orderBy["direction"]) == "DESC" {
				direction = -1
			}
		}
		if value, ok := orderBy["field"].(string); ok {
			field = strings.ToUpper(value)
		}
	}
	sortIssues := func(i, j int) bool {
		a, b := list[i], list[j]
		var key int64
		if field == "UPDATED_AT" {
			key = a.UpdatedTs - b.UpdatedTs
			if key == 0 {
				key = int64(a.Number - b.Number)
			}
		} else {
			key = int64(a.Number - b.Number)
		}
		return key*int64(direction) < 0
	}
	sortSlice(list, sortIssues)
	numbers := make([]int, len(list))
	byNumber := map[int]*IssueRec{}
	for index, issue := range list {
		numbers[index] = issue.Number
		byNumber[issue.Number] = issue
	}
	pageNumbers, info := paginateNumbers(repo, numbers, args)
	items := []*IssueRec{}
	for _, number := range pageNumbers {
		items = append(items, byNumber[number])
	}
	return gql("IssueConnection", issueConnSource{repo: repo, items: items, page: info})
}

func pullRequestsConnection(repo *RepoRec, args map[string]any) gObj {
	list := []*PullRec{}
	for _, pull := range repo.Pulls {
		list = append(list, pull)
	}
	states := []any{"OPEN"}
	if given, ok := args["states"].([]any); ok && len(given) > 0 {
		states = given
	}
	wanted := map[string]bool{}
	for _, state := range states {
		wanted[strings.ToUpper(fmt.Sprint(state))] = true
	}
	filtered := list[:0]
	for _, pull := range list {
		if pull.Merged {
			if wanted["MERGED"] {
				filtered = append(filtered, pull)
			}
		} else if wanted[strings.ToUpper(pull.State)] {
			filtered = append(filtered, pull)
		}
	}
	list = filtered
	sortSlice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	numbers := make([]int, len(list))
	byNumber := map[int]*PullRec{}
	for index, pull := range list {
		numbers[index] = pull.Number
		byNumber[pull.Number] = pull
	}
	pageNumbers, info := paginateNumbers(repo, numbers, args)
	items := []*PullRec{}
	for _, number := range pageNumbers {
		items = append(items, byNumber[number])
	}
	return gql("PullRequestConnection", pullConnSource{repo: repo, items: items, page: info})
}

// ---------------------------------------------------------------------------------------------
// the field table (validation and introspection share this one source of truth)

type gqlResolver func(ctx *gqlCtx, source any, args map[string]any) any

type gqlFieldDef struct {
	name    string
	resolve gqlResolver
}

var gqlEnumValues = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
	"FileChangeType":         {"ADDED", "MODIFIED", "DELETED", "RENAMED", "COPIED"},
}

var gqlFields = map[string][]gqlFieldDef{}
var gqlFieldIndex = map[string]map[string]gqlResolver{}

func defField(typeName, fieldName string, resolve gqlResolver) {
	gqlFields[typeName] = append(gqlFields[typeName], gqlFieldDef{name: fieldName, resolve: resolve})
	if gqlFieldIndex[typeName] == nil {
		gqlFieldIndex[typeName] = map[string]gqlResolver{}
	}
	gqlFieldIndex[typeName][fieldName] = resolve
}

func decodeGidParts(id *string) []string {
	if id == nil {
		return nil
	}
	return gidDecode(*id)
}

func init() {
	defField("Query", "viewer", func(ctx *gqlCtx, source any, args map[string]any) any {
		return gql("User", nil)
	})
	defField("Query", "repository", func(ctx *gqlCtx, source any, args map[string]any) any {
		owner := asString(args["owner"])
		name := asString(args["name"])
		if owner == nil || name == nil {
			return nil
		}
		repo := ctx.forge.GetRepo(*owner, *name)
		if repo == nil {
			return nil
		}
		return gql("Repository", repo)
	})
	defField("Query", "node", func(ctx *gqlCtx, source any, args map[string]any) any {
		decoded := decodeGidParts(asString(args["id"]))
		if decoded == nil {
			return nil
		}
		kind := decoded[0]
		switch kind {
		case "R":
			repo := ctx.forge.RepoById(idPartNumber(decoded[1]))
			if repo == nil {
				return nil
			}
			return gql("Repository", repo)
		case "U":
			if decoded[1] == ViewerLogin {
				return gql("User", nil)
			}
			return nil
		case "I", "P":
			repo := ctx.forge.RepoById(idPartNumber(decoded[1]))
			if repo == nil || len(decoded) < 3 {
				return nil
			}
			number := idPartNumber(decoded[2])
			if kind == "I" {
				issue := repo.Issues[number]
				if issue == nil {
					return nil
				}
				return gql("Issue", issueSource{repo: repo, issue: issue})
			}
			pull := repo.Pulls[number]
			if pull == nil {
				return nil
			}
			return gql("PullRequest", pullSource{repo: repo, pull: pull})
		}
		return nil
	})

	defField("Mutation", "createIssue", func(ctx *gqlCtx, source any, args map[string]any) any {
		input := inputOf(args)
		decoded := decodeGidParts(asString(input["repositoryId"]))
		var repo *RepoRec
		if decoded != nil && decoded[0] == "R" {
			repo = ctx.forge.RepoById(idPartNumber(decoded[1]))
		}
		if repo == nil {
			panic(gqlError{"repository not found"})
		}
		created := createIssue(ctx.forge, repo, asString(input["title"]), asString(input["body"]), nil)
		if !created.OK {
			panic(gqlError{created.Message})
		}
		return gql("CreateIssuePayload", issueSource{repo: repo, issue: created.Issue})
	})
	defField("Mutation", "closeIssue", func(ctx *gqlCtx, source any, args map[string]any) any {
		input := inputOf(args)
		decoded := decodeGidParts(asString(input["issueId"]))
		if decoded == nil || decoded[0] != "I" || len(decoded) < 3 {
			panic(gqlError{"issue not found"})
		}
		repo := ctx.forge.RepoById(idPartNumber(decoded[1]))
		var issue *IssueRec
		if repo != nil {
			issue = repo.Issues[idPartNumber(decoded[2])]
		}
		if repo == nil || issue == nil {
			panic(gqlError{"issue not found"})
		}
		issue.State = "closed"
		issue.UpdatedTs = ctx.forge.NextTs()
		return gql("CloseIssuePayload", issueSource{repo: repo, issue: issue})
	})
	defField("Mutation", "createPullRequest", func(ctx *gqlCtx, source any, args map[string]any) any {
		input := inputOf(args)
		decoded := decodeGidParts(asString(input["repositoryId"]))
		var repo *RepoRec
		if decoded != nil && decoded[0] == "R" {
			repo = ctx.forge.RepoById(idPartNumber(decoded[1]))
		}
		if repo == nil {
			panic(gqlError{"repository not found"})
		}
		created := createPull(ctx.forge, repo, asString(input["title"]), asString(input["body"]), asString(input["headRefName"]), asString(input["baseRefName"]))
		if !created.OK {
			panic(gqlError{created.Message})
		}
		return gql("CreatePullRequestPayload", pullSource{repo: repo, pull: created.Pull})
	})
	defField("Mutation", "mergePullRequest", func(ctx *gqlCtx, source any, args map[string]any) any {
		input := inputOf(args)
		decoded := decodeGidParts(asString(input["pullRequestId"]))
		if decoded == nil || decoded[0] != "P" || len(decoded) < 3 {
			panic(gqlError{"pull request not found"})
		}
		repo := ctx.forge.RepoById(idPartNumber(decoded[1]))
		var pull *PullRec
		if repo != nil {
			pull = repo.Pulls[idPartNumber(decoded[2])]
		}
		if repo == nil || pull == nil {
			panic(gqlError{"pull request not found"})
		}
		if pull.State != "open" {
			panic(gqlError{"pull request is not open"})
		}
		_, problem := ctx.forge.MergePull(repo, pull)
		if problem == "conflict" {
			panic(gqlError{"pull request has merge conflicts"})
		}
		if problem != "" {
			panic(gqlError{"pull request is not mergeable"})
		}
		return gql("MergePullRequestPayload", pullSource{repo: repo, pull: pull})
	})
	defField("Mutation", "addComment", func(ctx *gqlCtx, source any, args map[string]any) any {
		input := inputOf(args)
		decoded := decodeGidParts(asString(input["subjectId"]))
		if decoded == nil || decoded[0] != "I" || len(decoded) < 3 {
			panic(gqlError{"subject not found"})
		}
		repo := ctx.forge.RepoById(idPartNumber(decoded[1]))
		var issue *IssueRec
		if repo != nil {
			issue = repo.Issues[idPartNumber(decoded[2])]
		}
		if repo == nil || issue == nil {
			panic(gqlError{"subject not found"})
		}
		body := asString(input["body"])
		if body == nil {
			panic(gqlError{"body is required"})
		}
		comment := &CommentRec{ID: ctx.forge.NewCommentID(), Body: *body, CreatedTs: ctx.forge.NextTs()}
		issue.Comments = append(issue.Comments, comment)
		return gql("AddCommentPayload", commentSource{repo: repo, issue: issue, comment: comment})
	})

	defField("User", "id", func(ctx *gqlCtx, source any, args map[string]any) any { return gidEncode("U", ViewerLogin) })
	defField("User", "login", func(ctx *gqlCtx, source any, args map[string]any) any { return ViewerLogin })
	defField("User", "name", func(ctx *gqlCtx, source any, args map[string]any) any { return ViewerName })
	defField("User", "email", func(ctx *gqlCtx, source any, args map[string]any) any { return ViewerEmail })

	defField("Repository", "id", func(ctx *gqlCtx, source any, args map[string]any) any {
		return gidEncode("R", source.(*RepoRec).ID)
	})
	defField("Repository", "databaseId", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(*RepoRec).ID
	})
	defField("Repository", "name", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(*RepoRec).Name
	})
	defField("Repository", "nameWithOwner", func(ctx *gqlCtx, source any, args map[string]any) any {
		repo := source.(*RepoRec)
		return repo.Owner + "/" + repo.Name
	})
	defField("Repository", "description", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(*RepoRec).Description
	})
	defField("Repository", "isPrivate", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(*RepoRec).IsPrivate
	})
	defField("Repository", "owner", func(ctx *gqlCtx, source any, args map[string]any) any {
		return gql("User", nil)
	})
	defField("Repository", "defaultBranchRef", func(ctx *gqlCtx, source any, args map[string]any) any {
		repo := source.(*RepoRec)
		if ctx.forge.BranchTip(repo, repo.DefaultBranch) == "" {
			return nil
		}
		return gql("Ref", refSource{repo: repo, name: repo.DefaultBranch})
	})
	defField("Repository", "viewerPermission", func(ctx *gqlCtx, source any, args map[string]any) any { return "ADMIN" })
	defField("Repository", "hasIssuesEnabled", func(ctx *gqlCtx, source any, args map[string]any) any { return true })
	defField("Repository", "createdAt", func(ctx *gqlCtx, source any, args map[string]any) any {
		return isoOf(source.(*RepoRec).CreatedTs)
	})
	defField("Repository", "updatedAt", func(ctx *gqlCtx, source any, args map[string]any) any {
		return isoOf(source.(*RepoRec).UpdatedTs)
	})
	defField("Repository", "issue", func(ctx *gqlCtx, source any, args map[string]any) any {
		repo := source.(*RepoRec)
		number := asInt(args["number"])
		if number == nil {
			return nil
		}
		issue := repo.Issues[*number]
		if issue == nil {
			return nil
		}
		return gql("Issue", issueSource{repo: repo, issue: issue})
	})
	defField("Repository", "issues", func(ctx *gqlCtx, source any, args map[string]any) any {
		return issuesConnection(source.(*RepoRec), args)
	})
	defField("Repository", "pullRequest", func(ctx *gqlCtx, source any, args map[string]any) any {
		repo := source.(*RepoRec)
		number := asInt(args["number"])
		if number == nil {
			return nil
		}
		pull := repo.Pulls[*number]
		if pull == nil {
			return nil
		}
		return gql("PullRequest", pullSource{repo: repo, pull: pull})
	})
	defField("Repository", "pullRequests", func(ctx *gqlCtx, source any, args map[string]any) any {
		return pullRequestsConnection(source.(*RepoRec), args)
	})

	defField("Ref", "name", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(refSource).name
	})

	defField("Issue", "id", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(issueSource)
		return gidEncode("I", src.repo.ID, src.issue.Number)
	})
	defField("Issue", "number", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(issueSource).issue.Number
	})
	defField("Issue", "title", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(issueSource).issue.Title
	})
	defField("Issue", "body", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(issueSource).issue.Body
	})
	defField("Issue", "state", func(ctx *gqlCtx, source any, args map[string]any) any {
		return strings.ToUpper(source.(issueSource).issue.State)
	})
	defField("Issue", "closed", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(issueSource).issue.State == "closed"
	})
	defField("Issue", "author", func(ctx *gqlCtx, source any, args map[string]any) any { return gql("User", nil) })
	defField("Issue", "repository", func(ctx *gqlCtx, source any, args map[string]any) any {
		return gql("Repository", source.(issueSource).repo)
	})
	defField("Issue", "createdAt", func(ctx *gqlCtx, source any, args map[string]any) any {
		return isoOf(source.(issueSource).issue.CreatedTs)
	})
	defField("Issue", "updatedAt", func(ctx *gqlCtx, source any, args map[string]any) any {
		return isoOf(source.(issueSource).issue.UpdatedTs)
	})

	defField("PullRequest", "id", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(pullSource)
		return gidEncode("P", src.repo.ID, src.pull.Number)
	})
	defField("PullRequest", "number", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pullSource).pull.Number
	})
	defField("PullRequest", "title", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pullSource).pull.Title
	})
	defField("PullRequest", "body", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pullSource).pull.Body
	})
	defField("PullRequest", "state", func(ctx *gqlCtx, source any, args map[string]any) any {
		pull := source.(pullSource).pull
		if pull.Merged {
			return "MERGED"
		}
		return strings.ToUpper(pull.State)
	})
	defField("PullRequest", "merged", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pullSource).pull.Merged
	})
	defField("PullRequest", "mergedAt", func(ctx *gqlCtx, source any, args map[string]any) any {
		pull := source.(pullSource).pull
		if !pull.HasMergedAt {
			return nil
		}
		return isoOf(pull.MergedAtTs)
	})
	defField("PullRequest", "closed", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pullSource).pull.State == "closed"
	})
	defField("PullRequest", "mergeable", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(pullSource)
		return ctx.forge.MergeableState(src.repo, src.pull)
	})
	defField("PullRequest", "headRefName", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pullSource).pull.HeadRef
	})
	defField("PullRequest", "baseRefName", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pullSource).pull.BaseRef
	})
	defField("PullRequest", "headRef", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(pullSource)
		return gql("Ref", refSource{repo: src.repo, name: src.pull.HeadRef})
	})
	defField("PullRequest", "baseRef", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(pullSource)
		return gql("Ref", refSource{repo: src.repo, name: src.pull.BaseRef})
	})
	defField("PullRequest", "author", func(ctx *gqlCtx, source any, args map[string]any) any { return gql("User", nil) })
	defField("PullRequest", "repository", func(ctx *gqlCtx, source any, args map[string]any) any {
		return gql("Repository", source.(pullSource).repo)
	})
	defField("PullRequest", "createdAt", func(ctx *gqlCtx, source any, args map[string]any) any {
		return isoOf(source.(pullSource).pull.CreatedTs)
	})
	defField("PullRequest", "updatedAt", func(ctx *gqlCtx, source any, args map[string]any) any {
		return isoOf(source.(pullSource).pull.UpdatedTs)
	})
	defField("PullRequest", "files", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(pullSource)
		repo, pull := src.repo, src.pull
		headTip := ctx.forge.BranchTip(repo, pull.HeadRef)
		if headTip == "" {
			headTip = pull.HeadShaAtCreate
		}
		baseTip := ctx.forge.BranchTip(repo, pull.BaseRef)
		if baseTip == "" {
			baseTip = pull.BaseShaAtCreate
		}
		changes := []FileChange{}
		if headTip != "" && baseTip != "" {
			if base := mergeBaseOf(repo, baseTip, headTip); base != "" {
				changes = diffFiles(repo, base, headTip)
			}
		}
		total := len(changes)
		if first := asInt(args["first"]); first != nil && *first >= 0 && *first < len(changes) {
			changes = changes[:*first]
		}
		return gql("PullRequestChangedFileConnection", filesSource{changes: changes, totalCount: total})
	})

	defField("PullRequestChangedFileConnection", "nodes", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(filesSource)
		out := []any{}
		for _, change := range src.changes {
			out = append(out, gql("PullRequestChangedFile", change))
		}
		return out
	})
	defField("PullRequestChangedFileConnection", "totalCount", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(filesSource).totalCount
	})
	defField("PullRequestChangedFile", "path", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(FileChange).Filename
	})
	defField("PullRequestChangedFile", "additions", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(FileChange).Additions
	})
	defField("PullRequestChangedFile", "deletions", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(FileChange).Deletions
	})
	defField("PullRequestChangedFile", "changeType", func(ctx *gqlCtx, source any, args map[string]any) any {
		status := source.(FileChange).Status
		if status == "removed" {
			return "DELETED"
		}
		return strings.ToUpper(status)
	})

	defField("IssueConnection", "nodes", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(issueConnSource)
		out := []any{}
		for _, issue := range src.items {
			out = append(out, gql("Issue", issueSource{repo: src.repo, issue: issue}))
		}
		return out
	})
	defField("IssueConnection", "pageInfo", func(ctx *gqlCtx, source any, args map[string]any) any {
		return gql("PageInfo", source.(issueConnSource).page)
	})
	defField("IssueConnection", "totalCount", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(issueConnSource).page.totalCount
	})
	defField("PullRequestConnection", "nodes", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(pullConnSource)
		out := []any{}
		for _, pull := range src.items {
			out = append(out, gql("PullRequest", pullSource{repo: src.repo, pull: pull}))
		}
		return out
	})
	defField("PullRequestConnection", "pageInfo", func(ctx *gqlCtx, source any, args map[string]any) any {
		return gql("PageInfo", source.(pullConnSource).page)
	})
	defField("PullRequestConnection", "totalCount", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pullConnSource).page.totalCount
	})

	defField("PageInfo", "hasNextPage", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pageInfo).hasNextPage
	})
	defField("PageInfo", "hasPreviousPage", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(pageInfo).hasPreviousPage
	})
	defField("PageInfo", "endCursor", func(ctx *gqlCtx, source any, args map[string]any) any {
		if cursor := source.(pageInfo).endCursor; cursor != nil {
			return *cursor
		}
		return nil
	})
	defField("PageInfo", "startCursor", func(ctx *gqlCtx, source any, args map[string]any) any {
		if cursor := source.(pageInfo).startCursor; cursor != nil {
			return *cursor
		}
		return nil
	})

	defField("CreateIssuePayload", "issue", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(issueSource)
		return gql("Issue", src)
	})
	defField("CloseIssuePayload", "issue", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(issueSource)
		return gql("Issue", src)
	})
	defField("CreatePullRequestPayload", "pullRequest", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(pullSource)
		return gql("PullRequest", src)
	})
	defField("MergePullRequestPayload", "pullRequest", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(pullSource)
		return gql("PullRequest", src)
	})
	defField("AddCommentPayload", "commentEdge", func(ctx *gqlCtx, source any, args map[string]any) any {
		return gql("IssueCommentEdge", source)
	})
	defField("IssueCommentEdge", "node", func(ctx *gqlCtx, source any, args map[string]any) any {
		return gql("IssueComment", source)
	})
	defField("IssueComment", "id", func(ctx *gqlCtx, source any, args map[string]any) any {
		src := source.(commentSource)
		return gidEncode("C", src.repo.ID, src.comment.ID)
	})
	defField("IssueComment", "body", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(commentSource).comment.Body
	})

	defField("__Type", "name", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(string)
	})
	defField("__Type", "kind", func(ctx *gqlCtx, source any, args map[string]any) any {
		if _, isEnum := gqlEnumValues[source.(string)]; isEnum {
			return "ENUM"
		}
		return "OBJECT"
	})
	defField("__Type", "fields", func(ctx *gqlCtx, source any, args map[string]any) any {
		out := []any{}
		for _, field := range gqlFields[source.(string)] {
			out = append(out, gql("__Field", field.name))
		}
		return out
	})
	defField("__Type", "enumValues", func(ctx *gqlCtx, source any, args map[string]any) any {
		out := []any{}
		for _, name := range gqlEnumValues[source.(string)] {
			out = append(out, gql("__EnumValue", name))
		}
		return out
	})
	defField("__Field", "name", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(string)
	})
	defField("__EnumValue", "name", func(ctx *gqlCtx, source any, args map[string]any) any {
		return source.(string)
	})
}

type filesSource struct {
	changes    []FileChange
	totalCount int
}

type commentSource struct {
	repo    *RepoRec
	issue   *IssueRec
	comment *CommentRec
}

// A stable sort helper (the JS sorts this mirrors are stable).
func sortSlice[T any](slice []T, less func(i, j int) bool) {
	sort.SliceStable(slice, less)
}

// ---------------------------------------------------------------------------------------------
// selection execution

func executeSelectionSet(ctx *gqlCtx, typeName string, source any, selections []selectionNode, out map[string]any) {
	for _, selection := range selections {
		switch selection.kind {
		case "inline":
			if !selection.hasTypeCond || selection.typeCond == typeName {
				executeSelectionSet(ctx, typeName, source, selection.selectionSet, out)
			}
		case "spread":
			if fragment, ok := ctx.fragments[selection.name]; ok && fragment.typeCondition == typeName {
				executeSelectionSet(ctx, typeName, source, fragment.selectionSet, out)
			}
		case "field":
			field := selection.field
			key := field.name
			if field.hasAlias {
				key = field.alias
			}
			if field.name == "__typename" {
				out[key] = typeName
				continue
			}
			if field.name == "__type" && typeName == "Query" {
				name := asString(argsOf(field, ctx)["name"])
				known := false
				if name != nil {
					_, hasFields := gqlFields[*name]
					_, hasEnum := gqlEnumValues[*name]
					known = hasFields || hasEnum
				}
				if known {
					out[key] = completeValue(ctx, gql("__Type", *name), field)
				} else {
					out[key] = nil
				}
				continue
			}
			resolver, ok := gqlFieldIndex[typeName][field.name]
			if !ok {
				ctx.errors = append(ctx.errors, JObj{"message": fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field.name, typeName)})
				out[key] = nil
				continue
			}
			value, errMessage := callResolver(ctx, resolver, source, field)
			if errMessage != "" {
				ctx.errors = append(ctx.errors, JObj{"message": errMessage})
				out[key] = nil
				continue
			}
			out[key] = completeValue(ctx, value, field)
		}
	}
}

// Call one resolver, turning its failures into an error message (the executor's per-field catch).
func callResolver(ctx *gqlCtx, resolver gqlResolver, source any, field *fieldNode) (value any, errMessage string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			value = nil
			switch problem := recovered.(type) {
			case gqlError:
				errMessage = problem.message
			case error:
				errMessage = problem.Error()
			default:
				errMessage = fmt.Sprintf("%v", recovered)
			}
		}
	}()
	return resolver(ctx, source, argsOf(field, ctx)), ""
}

func completeValue(ctx *gqlCtx, value any, selection *fieldNode) any {
	if value == nil {
		return nil
	}
	if list, ok := value.([]any); ok {
		out := make([]any, len(list))
		for index, entry := range list {
			out[index] = completeValue(ctx, entry, selection)
		}
		return out
	}
	if object, ok := value.(gObj); ok {
		if !selection.hasSelection {
			return nil
		}
		out := map[string]any{}
		executeSelectionSet(ctx, object.typeName, object.source, selection.selectionSet, out)
		return out
	}
	return value
}

// ---------------------------------------------------------------------------------------------
// the endpoint

type graphQLResult struct {
	status int
	body   any
}

func executeOperation(forge *Forge, operation map[string]any) (result map[string]any) {
	queryText, _ := operation["query"].(string)
	var document documentNode
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				switch problem := recovered.(type) {
				case error:
					result = JObj{"data": nil, "errors": []any{JObj{"message": problem.Error()}}}
				default:
					result = JObj{"data": nil, "errors": []any{JObj{"message": fmt.Sprintf("%v", recovered)}}}
				}
			}
		}()
		document = (&gqlParser{tokens: lexGraphQL(queryText)}).parseDocument()
	}()
	if result != nil {
		return result
	}
	operationName, _ := operation["operationName"].(string)
	var chosen *operationNode
	for index := range document.operations {
		candidate := &document.operations[index]
		if operationName == "" || candidate.name == operationName {
			chosen = candidate
			break
		}
	}
	if chosen == nil && len(document.operations) > 0 {
		chosen = &document.operations[0]
	}
	if chosen == nil {
		return JObj{"data": nil, "errors": []any{JObj{"message": "no operation in document"}}}
	}
	variables := map[string]any{}
	if record := asRecord(operation["variables"]); record != nil {
		variables = record
	}
	ctx := &gqlCtx{forge: forge, variables: variables, fragments: document.fragments, errors: []any{}}
	rootType := "Query"
	if chosen.opType == "mutation" {
		rootType = "Mutation"
	}
	data := map[string]any{}
	executeSelectionSet(ctx, rootType, nil, chosen.selectionSet, data)
	result = JObj{"data": data}
	if len(ctx.errors) > 0 {
		result["errors"] = ctx.errors
	}
	return result
}

// Serve one POST /api/graphql body. Auth is the caller's job (the route answers 401 first).
func handleGraphQL(forge *Forge, bodyText string) graphQLResult {
	parsed, err := parseJSON([]byte(bodyText))
	if err != nil {
		return graphQLResult{status: 400, body: JObj{"errors": []any{JObj{"message": "Problems parsing JSON"}}}}
	}
	if list, ok := parsed.([]any); ok {
		out := []any{}
		for _, entry := range list {
			if record := asRecord(entry); record != nil {
				out = append(out, executeOperation(forge, record))
			} else {
				out = append(out, executeOperation(forge, map[string]any{}))
			}
		}
		return graphQLResult{status: 200, body: out}
	}
	record := asRecord(parsed)
	if record == nil {
		return graphQLResult{status: 400, body: JObj{"errors": []any{JObj{"message": "Invalid request body"}}}}
	}
	return graphQLResult{status: 200, body: executeOperation(forge, record)}
}
