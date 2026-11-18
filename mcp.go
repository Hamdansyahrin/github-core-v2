// The MCP endpoint at /mcp: JSON-RPC 2.0 over streamable HTTP, answered as plain JSON. The server
// speaks the handshake era (2025-11-25 and earlier): `server/discover` is refused with JSON-RPC
// -32601, which is exactly how a pre-discover server announces itself, and the client then falls
// back to `initialize` + `notifications/initialized`. The six tools act on the same store as the
// REST surface, so objects created here read back over /api/v3 unchanged.

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

type mcpResult struct {
	status int
	text   string
}

var legacyRevisions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema mcpInputSchema `json:"inputSchema"`
}

type mcpInputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]mcpProperty `json:"properties"`
	Required   []string               `json:"required"`
}

type mcpProperty struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

func stringProp(description string) mcpProperty {
	return mcpProperty{Type: "string", Description: description}
}

// The tool list: a fixed value, so two tools/list answers are byte-for-byte identical.
var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository for the authenticated user",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProperty{
				"name":        stringProp("The name of the repository"),
				"description": stringProp("A short description of the repository"),
				"private":     {Type: "boolean", Description: "Whether the repository is private"},
			},
			Required: []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get details of a repository by owner and name",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProperty{
				"owner": stringProp("The repository owner login"),
				"repo":  stringProp("The repository name"),
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create a new issue in a repository",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProperty{
				"owner": stringProp("The repository owner login"),
				"repo":  stringProp("The repository name"),
				"title": stringProp("The issue title"),
				"body":  stringProp("The issue body"),
			},
			Required: []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List the issues of a repository, optionally filtered by state",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProperty{
				"owner": stringProp("The repository owner login"),
				"repo":  stringProp("The repository name"),
				"state": stringProp("The issue state to list: open, closed or all"),
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request from a head branch into a base branch",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProperty{
				"owner": stringProp("The repository owner login"),
				"repo":  stringProp("The repository name"),
				"title": stringProp("The pull request title"),
				"body":  stringProp("The pull request body"),
				"head":  stringProp("The head branch (the branch with the changes)"),
				"base":  stringProp("The base branch (the branch to merge into)"),
			},
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request by number",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProperty{
				"owner":        stringProp("The repository owner login"),
				"repo":         stringProp("The repository name"),
				"pull_number":  {Type: "integer", Description: "The pull request number"},
				"merge_method": stringProp("The merge method: merge, squash or rebase"),
			},
			Required: []string{"owner", "repo", "pull_number"},
		},
	},
}

// JSON-RPC ids keep the caller's type: a string, a number (rendered as in JSON), or null.
type rpcID struct {
	raw any
}

func (id rpcID) MarshalJSON() ([]byte, error) {
	if id.raw == nil {
		return []byte("null"), nil
	}
	return marshalJSON(id.raw), nil
}

func rpcResult(id rpcID, result any) mcpResult {
	return mcpResult{status: 200, text: string(marshalJSON(JObj{"jsonrpc": "2.0", "id": id, "result": result}))}
}

func rpcError(id rpcID, status, code int, message string) mcpResult {
	return mcpResult{status: status, text: string(marshalJSON(JObj{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   JObj{"code": code, "message": message},
	}))}
}

// A tool result: the payload as a JSON text block (and structured content for objects).
func toolOk(payload any) JObj {
	result := JObj{
		"content": []any{JObj{"type": "text", "text": string(marshalJSON(payload))}},
		"isError": false,
	}
	if isRecord(payload) {
		result["structuredContent"] = payload
	}
	return result
}

func toolError(message string) JObj {
	return JObj{
		"content": []any{JObj{"type": "text", "text": message}},
		"isError": true,
	}
}

// A JSON-RPC error raised inside a tool call, carrying its code.
type rpcFailure struct {
	code    int
	message string
}

func (e rpcFailure) Error() string { return e.message }

// ---------------------------------------------------------------------------------------------
// tool implementations

func mcpRepoArg(forge *Forge, args map[string]any) *RepoRec {
	owner, okOwner := args["owner"].(string)
	repo, okRepo := args["repo"].(string)
	if !okOwner || !okRepo {
		return nil
	}
	return forge.GetRepo(owner, repo)
}

func mcpStrArg(args map[string]any, key string) *string {
	if value, ok := args[key].(string); ok {
		return &value
	}
	return nil
}

func callTool(forge *Forge, name string, args map[string]any) (result JObj) {
	var tool *mcpToolDef
	for index := range mcpTools {
		if mcpTools[index].Name == name {
			tool = &mcpTools[index]
			break
		}
	}
	if tool == nil {
		panic(rpcFailure{code: -32602, message: "Unknown tool: " + name})
	}
	for _, required := range tool.InputSchema.Required {
		value, present := args[required]
		if !present || value == nil || value == "" {
			panic(rpcFailure{code: -32602, message: "Missing required argument: " + required})
		}
	}
	switch name {
	case "create_repo":
		repoName := args["name"].(string)
		if forge.GetRepo(ViewerLogin, repoName) != nil {
			return toolError(fmt.Sprintf("Repository %s already exists", repoName))
		}
		isPrivate := false
		if value, ok := args["private"].(bool); ok {
			isPrivate = value
		}
		description := ""
		if value, ok := args["description"].(string); ok {
			description = value
		}
		repo := forge.CreateRepo(ViewerLogin, repoName, isPrivate, description)
		return toolOk(repoJson(repo))
	case "get_repo":
		repo := mcpRepoArg(forge, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		return toolOk(repoJson(repo))
	case "create_issue":
		repo := mcpRepoArg(forge, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		var labels []string
		if list, ok := args["labels"].([]any); ok {
			labels = []string{}
			for _, entry := range list {
				if text, ok := entry.(string); ok {
					labels = append(labels, text)
				}
			}
		}
		created := createIssue(forge, repo, mcpStrArg(args, "title"), mcpStrArg(args, "body"), labels)
		if !created.OK {
			return toolError(created.Message)
		}
		return toolOk(issueJson(repo, created.Issue))
	case "list_issues":
		repo := mcpRepoArg(forge, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		state := "open"
		if value, ok := args["state"].(string); ok {
			state = strings.ToLower(value)
		}
		issues := []*IssueRec{}
		for _, issue := range repo.Issues {
			if state == "open" && issue.State != "open" {
				continue
			}
			if state == "closed" && issue.State != "closed" {
				continue
			}
			issues = append(issues, issue)
		}
		sortSlice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
		out := []any{}
		for _, issue := range issues {
			out = append(out, issueJson(repo, issue))
		}
		return toolOk(out)
	case "create_pull_request":
		repo := mcpRepoArg(forge, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		created := createPull(forge, repo, mcpStrArg(args, "title"), mcpStrArg(args, "body"), mcpStrArg(args, "head"), mcpStrArg(args, "base"))
		if !created.OK {
			return toolError(created.Message)
		}
		return toolOk(pullJson(forge, repo, created.Pull))
	case "merge_pull_request":
		repo := mcpRepoArg(forge, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		number := jsNumberValue(args["pull_number"])
		var pull *PullRec
		if !math.IsNaN(number) && number == math.Trunc(number) {
			pull = repo.Pulls[int(number)]
		}
		if pull == nil {
			return toolError(fmt.Sprintf("Pull request #%s not found", jsString(args["pull_number"])))
		}
		if pull.State != "open" {
			return toolError("Pull request is not open")
		}
		_, problem := forge.MergePull(repo, pull)
		if problem == "conflict" {
			return toolError("Pull request has merge conflicts")
		}
		if problem != "" {
			return toolError("Pull request is not mergeable")
		}
		return toolOk(pullJson(forge, repo, pull))
	}
	panic(rpcFailure{code: -32602, message: "Unknown tool: " + name})
}

// JavaScript's Number(value) for the JSON values an arguments object can hold.
func jsNumberValue(value any) float64 {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Float64()
		if err != nil {
			return math.NaN()
		}
		return parsed
	case float64:
		return number
	case string:
		trimmed := strings.TrimSpace(number)
		if trimmed == "" {
			return 0
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return math.NaN()
		}
		return parsed
	case bool:
		if number {
			return 1
		}
		return 0
	}
	return math.NaN()
}

// JavaScript's String(value), for message interpolation.
func jsString(value any) string {
	switch text := value.(type) {
	case string:
		return text
	case json.Number:
		return text.String()
	case float64:
		return strconv.FormatFloat(text, 'f', -1, 64)
	case bool:
		if text {
			return "true"
		}
		return "false"
	case nil:
		return "undefined"
	}
	return fmt.Sprintf("%v", value)
}

// ---------------------------------------------------------------------------------------------
// the endpoint

func handleOneMcp(forge *Forge, body map[string]any) *mcpResult {
	id := rpcID{raw: body["id"]}
	method, _ := body["method"].(string)
	params := asRecord(body["params"])
	if params == nil {
		params = map[string]any{}
	}
	// A message with no id is a notification: accepted, never answered.
	if _, hasID := body["id"]; !hasID {
		return nil
	}
	switch method {
	case "server/discover":
		result := rpcError(id, 200, -32601, "Method not found: server/discover")
		return &result
	case "initialize":
		offered, _ := params["protocolVersion"].(string)
		negotiated := "2025-11-25"
		for _, revision := range legacyRevisions {
			if offered == revision {
				negotiated = offered
				break
			}
		}
		result := rpcResult(id, JObj{
			"protocolVersion": negotiated,
			"capabilities":    JObj{"tools": JObj{}},
			"serverInfo":      JObj{"name": "makefun-forge", "version": "1.0.0"},
		})
		return &result
	case "tools/list":
		result := rpcResult(id, JObj{"tools": mcpTools})
		return &result
	case "tools/call":
		name, _ := params["name"].(string)
		args := asRecord(params["arguments"])
		if args == nil {
			args = map[string]any{}
		}
		result := callToolGuarded(forge, name, args, id)
		return &result
	case "ping":
		result := rpcResult(id, JObj{})
		return &result
	default:
		result := rpcError(id, 200, -32601, "Method not found: "+method)
		return &result
	}
}

func callToolGuarded(forge *Forge, name string, args map[string]any, id rpcID) (result mcpResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if failure, ok := recovered.(rpcFailure); ok {
				result = rpcError(id, 200, failure.code, failure.message)
				return
			}
			panic(recovered)
		}
	}()
	return rpcResult(id, callTool(forge, name, args))
}

// Serve one POST /mcp body. Auth is the caller's job (the route answers 401 before this runs).
func handleMcp(forge *Forge, bodyText string) (result mcpResult) {
	parsed, err := parseJSON([]byte(bodyText))
	if err != nil {
		return rpcError(rpcID{}, 400, -32700, "Parse error")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(os.Stderr, "[mcp] %v\n", recovered)
			result = rpcError(rpcID{}, 200, -32603, "Internal error")
		}
	}()
	if list, ok := parsed.([]any); ok {
		answers := []any{}
		for _, entry := range list {
			record := asRecord(entry)
			if record == nil {
				continue
			}
			answer := handleOneMcp(forge, record)
			if answer != nil {
				parsedAnswer, err := parseJSON([]byte(answer.text))
				if err == nil {
					answers = append(answers, parsedAnswer)
				}
			}
		}
		return mcpResult{status: 200, text: string(marshalJSON(answers))}
	}
	record := asRecord(parsed)
	if record == nil {
		return rpcError(rpcID{}, 400, -32600, "Invalid request")
	}
	answer := handleOneMcp(forge, record)
	if answer == nil {
		return mcpResult{status: 202, text: ""}
	}
	return *answer
}
