// MCP: JSON-RPC 2.0 over Streamable HTTP (plain JSON responses, never SSE),
// stateless — no Mcp-Session-Id is ever issued. Both eras are implemented:
// the modern `server/discover` probe (which the client tries first) and the
// legacy `initialize` + `notifications/initialized` flow.
//
//   - Notifications (a request with no `id` member) get HTTP 202 with an empty body.
//   - A malformed JSON body gets HTTP 400 with JSON-RPC -32700 and id null.
//   - An unknown method gets -32601; a missing tool argument gets -32602.
//   - A tool that runs but fails (a repo that does not exist, a merge conflict) answers with a
//     normal result carrying `isError: true`, not a JSON-RPC error.
//   - Every tool result is {content: [{type: "text", text: <a JSON string>}], structuredContent}.
//   - The six tools are a module constant, so tools/list is byte-identical on every call.

package main

import (
	"encoding/json"
)

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"

// mcpTool is one tool definition (struct field order = JSON key order).
type mcpTool struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	InputSchema mcpToolInputSchema `json:"inputSchema"`
}

type mcpToolInputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]mcpToolProp `json:"properties"`
	Required   []string               `json:"required"`
}

type mcpToolProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

func strProp(desc string) mcpToolProp  { return mcpToolProp{Type: "string", Description: desc} }
func boolProp(desc string) mcpToolProp { return mcpToolProp{Type: "boolean", Description: desc} }
func intProp(desc string) mcpToolProp  { return mcpToolProp{Type: "integer", Description: desc} }

// The six canonical tools. Required properties are all fillable from the suite's semantic args.
var mcpTools = []mcpTool{
	{
		Name: "create_repo", Description: "Create a new repository owned by the authenticated user.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"name":        strProp("The repository name."),
			"description": strProp("A short description of the repository."),
			"private":     boolProp("Whether the repository is private."),
		}, Required: []string{"name"}},
	},
	{
		Name: "get_repo", Description: "Get a repository by owner and name.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner": strProp("The repository owner login."),
			"repo":  strProp("The repository name."),
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name: "create_issue", Description: "Create an issue in a repository.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner": strProp("The repository owner login."),
			"repo":  strProp("The repository name."),
			"title": strProp("The issue title."),
			"body":  strProp("The issue body."),
		}, Required: []string{"owner", "repo", "title"}},
	},
	{
		Name: "list_issues", Description: "List issues in a repository.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner": strProp("The repository owner login."),
			"repo":  strProp("The repository name."),
			"state": strProp(`Filter by state: "open", "closed", or "all".`),
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name: "create_pull_request", Description: "Create a pull request in a repository.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner": strProp("The repository owner login."),
			"repo":  strProp("The repository name."),
			"title": strProp("The pull request title."),
			"head":  strProp("The head branch name."),
			"base":  strProp("The base branch name."),
			"body":  strProp("The pull request body."),
		}, Required: []string{"owner", "repo", "title", "head", "base"}},
	},
	{
		Name: "merge_pull_request", Description: "Merge a pull request.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner":       strProp("The repository owner login."),
			"repo":        strProp("The repository name."),
			"pull_number": intProp("The pull request number."),
		}, Required: []string{"owner", "repo", "pull_number"}},
	},
}

// McpResponse is handleMcp's answer: the status, and the JSON-RPC response
// object (nil for a 202 notification: an empty body).
type McpResponse struct {
	Status int
	JSON   any
}

// mcpUnauthorized is the 401 body for /mcp: a JSON-RPC error.
func mcpUnauthorized() any {
	return rpcErrorBody(nil, -32000, "Unauthorized")
}

type rpcErrorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id"`
	Result  any          `json:"result,omitempty"`
	Error   *rpcErrorObj `json:"error,omitempty"`
}

func rpcResultBody(id, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcErrorBody(id any, code int, message string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcErrorObj{Code: code, Message: message}}
}

// toolOkJSON is a successful tool result from pre-built JSON bytes: the JSON
// string and the structured object carry the same data.
func toolOkJSON(jsonBytes []byte) map[string]any {
	var structured any
	_ = json.Unmarshal(jsonBytes, &structured)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(jsonBytes)}},
		"structuredContent": structured,
	}
}

// toolFailed is a tool that ran but failed: a normal result with isError, never a JSON-RPC error.
func toolFailed(message string) map[string]any {
	structured := map[string]any{"error": message}
	text, _ := json.Marshal(structured)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": structured,
		"isError":           true,
	}
}

func mcpRequiredString(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok && s != "" {
		return s
	}
	return ""
}

func mcpMissingArg(tool, key string) (int, string) {
	return -32602, `Missing required argument "` + key + `" for tool "` + tool + `"`
}

type toolOutcome struct {
	result  map[string]any
	errCode int
	errMsg  string
	hasErr  bool
}

// callTool runs one of the six tools.
func callTool(forge *Forge, name string, args map[string]any) toolOutcome {
	fail := func(code int, msg string) toolOutcome { return toolOutcome{errCode: code, errMsg: msg, hasErr: true} }
	switch name {
	case "create_repo":
		repoName := mcpRequiredString(args, "name")
		if repoName == "" {
			code, msg := mcpMissingArg(name, "name")
			return fail(code, msg)
		}
		if forge.GetRepo(viewer.Login, repoName) != nil {
			return toolOutcome{result: toolFailed("Repository " + viewer.Login + "/" + repoName + " already exists")}
		}
		description, _ := args["description"].(string)
		isPrivate := args["private"] == true
		repo := forge.CreateRepo(viewer.Login, repoName, isPrivate, description, "")
		return toolOutcome{result: toolOkJSON(forge.RepoJSON(repo))}
	case "get_repo":
		owner := mcpRequiredString(args, "owner")
		if owner == "" {
			code, msg := mcpMissingArg(name, "owner")
			return fail(code, msg)
		}
		repoName := mcpRequiredString(args, "repo")
		if repoName == "" {
			code, msg := mcpMissingArg(name, "repo")
			return fail(code, msg)
		}
		repo := forge.GetRepo(owner, repoName)
		if repo == nil {
			return toolOutcome{result: toolFailed("Repository " + owner + "/" + repoName + " not found")}
		}
		return toolOutcome{result: toolOkJSON(forge.RepoJSON(repo))}
	case "create_issue":
		owner := mcpRequiredString(args, "owner")
		if owner == "" {
			code, msg := mcpMissingArg(name, "owner")
			return fail(code, msg)
		}
		repoName := mcpRequiredString(args, "repo")
		if repoName == "" {
			code, msg := mcpMissingArg(name, "repo")
			return fail(code, msg)
		}
		title := mcpRequiredString(args, "title")
		if title == "" {
			code, msg := mcpMissingArg(name, "title")
			return fail(code, msg)
		}
		repo := forge.GetRepo(owner, repoName)
		if repo == nil {
			return toolOutcome{result: toolFailed("Repository " + owner + "/" + repoName + " not found")}
		}
		body, _ := args["body"].(string)
		issue := forge.CreateIssue(repo, title, body, nil)
		return toolOutcome{result: toolOkJSON(forge.IssueJSON(repo, issue))}
	case "list_issues":
		owner := mcpRequiredString(args, "owner")
		if owner == "" {
			code, msg := mcpMissingArg(name, "owner")
			return fail(code, msg)
		}
		repoName := mcpRequiredString(args, "repo")
		if repoName == "" {
			code, msg := mcpMissingArg(name, "repo")
			return fail(code, msg)
		}
		repo := forge.GetRepo(owner, repoName)
		if repo == nil {
			return toolOutcome{result: toolFailed("Repository " + owner + "/" + repoName + " not found")}
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		var issues []*Issue
		for _, issue := range repo.Issues {
			keep := false
			if state == "all" {
				keep = true
			} else if state == "closed" {
				keep = issue.State == "closed"
			} else {
				keep = issue.State == "open"
			}
			if keep {
				issues = append(issues, issue)
			}
		}
		// Sort by number ascending (insertion sort on the small slice is fine,
		// but sort.SliceStable is clearer).
		for i := 1; i < len(issues); i++ {
			for j := i; j > 0 && issues[j].Number < issues[j-1].Number; j-- {
				issues[j], issues[j-1] = issues[j-1], issues[j]
			}
		}
		var buf []byte
		buf = append(buf, `{"issues":[`...)
		for i, issue := range issues {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, forge.IssueJSON(repo, issue)...)
		}
		buf = append(buf, `]}`...)
		return toolOutcome{result: toolOkJSON(buf)}
	case "create_pull_request":
		owner := mcpRequiredString(args, "owner")
		if owner == "" {
			code, msg := mcpMissingArg(name, "owner")
			return fail(code, msg)
		}
		repoName := mcpRequiredString(args, "repo")
		if repoName == "" {
			code, msg := mcpMissingArg(name, "repo")
			return fail(code, msg)
		}
		title := mcpRequiredString(args, "title")
		if title == "" {
			code, msg := mcpMissingArg(name, "title")
			return fail(code, msg)
		}
		head := mcpRequiredString(args, "head")
		if head == "" {
			code, msg := mcpMissingArg(name, "head")
			return fail(code, msg)
		}
		base := mcpRequiredString(args, "base")
		if base == "" {
			code, msg := mcpMissingArg(name, "base")
			return fail(code, msg)
		}
		repo := forge.GetRepo(owner, repoName)
		if repo == nil {
			return toolOutcome{result: toolFailed("Repository " + owner + "/" + repoName + " not found")}
		}
		headSha, okH := repo.Refs["refs/heads/"+head]
		baseSha, okB := repo.Refs["refs/heads/"+base]
		if !okH {
			return toolOutcome{result: toolFailed("Branch " + head + " not found")}
		}
		if !okB {
			return toolOutcome{result: toolFailed("Branch " + base + " not found")}
		}
		duplicate := false
		for _, pull := range repo.Pulls {
			if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
				duplicate = true
				break
			}
		}
		if duplicate {
			return toolOutcome{result: toolFailed("An open pull request already exists for " + head + " into " + base)}
		}
		body, _ := args["body"].(string)
		pull := forge.CreatePull(repo, title, body, head, base, headSha, baseSha)
		return toolOutcome{result: toolOkJSON(forge.PullJSON(repo, pull))}
	case "merge_pull_request":
		owner := mcpRequiredString(args, "owner")
		if owner == "" {
			code, msg := mcpMissingArg(name, "owner")
			return fail(code, msg)
		}
		repoName := mcpRequiredString(args, "repo")
		if repoName == "" {
			code, msg := mcpMissingArg(name, "repo")
			return fail(code, msg)
		}
		rawNumber := args["pull_number"]
		pullNumber, ok := jsNumber(rawNumber)
		if !ok || !isIntegerFloat(pullNumber) {
			code, msg := mcpMissingArg(name, "pull_number")
			return fail(code, msg)
		}
		repo := forge.GetRepo(owner, repoName)
		if repo == nil {
			return toolOutcome{result: toolFailed("Repository " + owner + "/" + repoName + " not found")}
		}
		pull := repo.Pulls[int(pullNumber)]
		if pull == nil {
			return toolOutcome{result: toolFailed("Pull request #" + itoa(int(pullNumber)) + " not found")}
		}
		sha, conflict := forge.MergePull(repo, pull, "squash")
		if sha == "" {
			if conflict {
				return toolOutcome{result: toolFailed("Pull request #" + itoa(int(pullNumber)) + " has a merge conflict")}
			}
			return toolOutcome{result: toolFailed("Pull request #" + itoa(int(pullNumber)) + " cannot be merged")}
		}
		return toolOutcome{result: toolOkJSON(forge.PullJSON(repo, pull))}
	default:
		return fail(-32601, `Unknown tool "`+name+`"`)
	}
}

// handleMcp serves POST /mcp. rawBody is the request body as received; a
// malformed JSON body is answered with HTTP 400 and JSON-RPC -32700 (id null).
// Auth is checked by the caller.
func handleMcp(forge *Forge, rawBody []byte) McpResponse {
	parsed, ok := unmarshalJSON(rawBody)
	if !ok {
		return McpResponse{Status: 400, JSON: rpcErrorBody(nil, -32700, "Parse error")}
	}
	req := asRecord(parsed)
	if req == nil {
		return McpResponse{Status: 200, JSON: rpcErrorBody(nil, -32600, "Invalid Request: expected a JSON-RPC object")}
	}
	_, hasID := req["id"]
	id := req["id"]
	method, _ := req["method"].(string)
	if method == "" {
		return McpResponse{Status: 200, JSON: rpcErrorBody(id, -32600, "Invalid Request: a method name is required")}
	}
	// A request with no id is a notification: acknowledged, never answered.
	if !hasID {
		return McpResponse{Status: 202, JSON: nil}
	}
	params := asRecord(req["params"])
	if params == nil {
		params = map[string]any{}
	}
	serverInfo := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
	switch method {
	case "server/discover":
		return McpResponse{Status: 200, JSON: rpcResultBody(id, map[string]any{
			"resultType":        "discover",
			"supportedVersions": []any{"2026-07-28"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		})}
	case "initialize":
		offered, _ := params["protocolVersion"].(string)
		if offered == "" {
			offered = "2025-11-25"
		}
		return McpResponse{Status: 200, JSON: rpcResultBody(id, map[string]any{
			"protocolVersion": offered,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      serverInfo,
		})}
	case "ping":
		return McpResponse{Status: 200, JSON: rpcResultBody(id, map[string]any{})}
	case "tools/list":
		return McpResponse{Status: 200, JSON: rpcResultBody(id, map[string]any{"tools": mcpTools})}
	case "tools/call":
		toolName, _ := params["name"].(string)
		if toolName == "" {
			return McpResponse{Status: 200, JSON: rpcErrorBody(id, -32602, "Invalid params: a tool name is required")}
		}
		args := asRecord(params["arguments"])
		if args == nil {
			args = map[string]any{}
		}
		outcome := callTool(forge, toolName, args)
		if outcome.hasErr {
			return McpResponse{Status: 200, JSON: rpcErrorBody(id, outcome.errCode, outcome.errMsg)}
		}
		return McpResponse{Status: 200, JSON: rpcResultBody(id, outcome.result)}
	default:
		return McpResponse{Status: 200, JSON: rpcErrorBody(id, -32601, "Method not found: "+method)}
	}
}
