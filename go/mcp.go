// Meridian Forge — MCP at /mcp (JSON-RPC 2.0, protocol era 2026-07-28).
//
// Methods: server/discover, initialize, notifications/initialized,
// tools/list (byte-identical across calls), tools/call. Tool results carry
// both a text JSON payload and structuredContent.
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// mcpTool is one tool definition.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func schema(props map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// mcpTools is the fixed tool list (built once, marshaled once).
var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a new repository for the authenticated user.",
		InputSchema: schema(map[string]any{
			"name":        strProp("Repository name"),
			"description": strProp("Repository description"),
			"private":     boolProp("Whether the repository is private"),
		}, []string{"name"}),
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Repository owner"),
			"repo":  strProp("Repository name"),
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Repository owner"),
			"repo":  strProp("Repository name"),
			"title": strProp("Issue title"),
			"body":  strProp("Issue body"),
		}, []string{"owner", "repo", "title"}),
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Repository owner"),
			"repo":  strProp("Repository name"),
			"state": strProp("Issue state filter: open, closed, or all"),
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request between two branches.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Repository owner"),
			"repo":  strProp("Repository name"),
			"title": strProp("Pull request title"),
			"head":  strProp("Head branch name"),
			"base":  strProp("Base branch name"),
			"body":  strProp("Pull request body"),
		}, []string{"owner", "repo", "title", "head", "base"}),
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request with a squash commit.",
		InputSchema: schema(map[string]any{
			"owner":       strProp("Repository owner"),
			"repo":        strProp("Repository name"),
			"pull_number": intProp("Pull request number"),
		}, []string{"owner", "repo", "pull_number"}),
	},
}

// mcpToolsJSON is the pre-marshaled tools array (byte-identical every call).
var mcpToolsJSON = func() []byte {
	b, _ := json.Marshal(mcpTools)
	return b
}()

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func mcpError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	})
}

func mcpResult(w http.ResponseWriter, id json.RawMessage, result any) {
	if id == nil {
		id = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	var req mcpRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		mcpError(w, http.StatusBadRequest, nil, -32700, "Parse error: "+err.Error())
		return
	}
	// Notifications (no id) get a bare 202.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "server/discover":
		serverInfo := map[string]any{"name": "meridian-forge", "version": "1.0.0"}
		mcpResult(w, req.ID, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-06-18", "2025-03-26"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": serverInfo,
			},
		})
	case "initialize":
		var params map[string]any
		_ = json.Unmarshal(req.Params, &params)
		version := "2026-07-28"
		if v, ok := params["protocolVersion"].(string); ok && v != "" {
			version = v
		}
		mcpResult(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "meridian-forge", "version": "1.0.0"},
		})
	case "tools/list":
		mcpResult(w, req.ID, map[string]any{
			"tools": json.RawMessage(mcpToolsJSON),
		})
	case "tools/call":
		s.mcpToolsCall(w, r, req)
	case "ping":
		mcpResult(w, req.ID, map[string]any{})
	default:
		mcpError(w, http.StatusOK, req.ID, -32601, "Method not found: "+req.Method)
	}
}

func (s *Server) mcpToolsCall(w http.ResponseWriter, r *http.Request, req mcpRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		mcpError(w, http.StatusOK, req.ID, -32602, "Invalid params: "+err.Error())
		return
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	// Dispatch; each tool returns (result object, error string).
	var obj any
	var errMsg string
	switch params.Name {
	case "create_repo":
		obj, errMsg = s.mcpCreateRepo(params.Arguments)
	case "get_repo":
		obj, errMsg = s.mcpGetRepo(params.Arguments)
	case "create_issue":
		obj, errMsg = s.mcpCreateIssue(params.Arguments)
	case "list_issues":
		obj, errMsg = s.mcpListIssues(params.Arguments)
	case "create_pull_request":
		obj, errMsg = s.mcpCreatePullRequest(params.Arguments)
	case "merge_pull_request":
		obj, errMsg = s.mcpMergePullRequest(params.Arguments)
	default:
		mcpError(w, http.StatusOK, req.ID, -32602, "Unknown tool: "+params.Name)
		return
	}
	if errMsg != "" {
		// Tool-level error: isError with a text payload.
		mcpResult(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": errMsg}},
			"isError": true,
		})
		return
	}
	text, _ := json.Marshal(obj)
	mcpResult(w, req.ID, map[string]any{
		"content":          []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": obj,
	})
}

func mcpStr(args map[string]any, name string) (string, bool) {
	v, ok := args[name]
	if !ok {
		return "", false
	}
	str, ok := v.(string)
	return str, ok && str != ""
}

func mcpInt(args map[string]any, name string) (int, bool) {
	v, ok := args[name]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i, true
		}
	}
	return 0, false
}

func (s *Server) mcpCreateRepo(args map[string]any) (any, string) {
	name, ok := mcpStr(args, "name")
	if !ok {
		return nil, "missing required argument: name"
	}
	desc, _ := args["description"].(string)
	private, _ := args["private"].(bool)
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.store.createRepo(name, private, desc)
	if repo == nil {
		return nil, "name already exists on this account"
	}
	return s.repoToJSON(repo), ""
}

func (s *Server) mcpGetRepo(args map[string]any) (any, string) {
	owner, ok1 := mcpStr(args, "owner")
	name, ok2 := mcpStr(args, "repo")
	if !ok1 || !ok2 {
		return nil, "missing required arguments: owner, repo"
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	if owner != viewerLogin {
		return nil, "repository not found"
	}
	repo := s.store.getRepo(owner, name)
	if repo == nil {
		return nil, "repository not found"
	}
	return s.repoToJSON(repo), ""
}

func (s *Server) mcpCreateIssue(args map[string]any) (any, string) {
	owner, ok1 := mcpStr(args, "owner")
	name, ok2 := mcpStr(args, "repo")
	title, ok3 := mcpStr(args, "title")
	if !ok1 || !ok2 || !ok3 {
		return nil, "missing required arguments: owner, repo, title"
	}
	body, _ := args["body"].(string)
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if owner != viewerLogin {
		return nil, "repository not found"
	}
	repo := s.store.getRepo(owner, name)
	if repo == nil {
		return nil, "repository not found"
	}
	iss := s.store.newIssue(repo, title, body, nil)
	return repo.issueToJSON(iss), ""
}

func (s *Server) mcpListIssues(args map[string]any) (any, string) {
	owner, ok1 := mcpStr(args, "owner")
	name, ok2 := mcpStr(args, "repo")
	if !ok1 || !ok2 {
		return nil, "missing required arguments: owner, repo"
	}
	state, _ := args["state"].(string)
	if state == "" {
		state = "open"
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	if owner != viewerLogin {
		return nil, "repository not found"
	}
	repo := s.store.getRepo(owner, name)
	if repo == nil {
		return nil, "repository not found"
	}
	var list []issueJSON
	for i := len(repo.issueOrder) - 1; i >= 0; i-- {
		iss := repo.issues[repo.issueOrder[i]]
		if state == "all" || iss.State == state {
			list = append(list, repo.issueToJSON(iss))
		}
	}
	if list == nil {
		list = []issueJSON{}
	}
	return map[string]any{"issues": list}, ""
}

func (s *Server) mcpCreatePullRequest(args map[string]any) (any, string) {
	owner, ok1 := mcpStr(args, "owner")
	name, ok2 := mcpStr(args, "repo")
	title, ok3 := mcpStr(args, "title")
	head, ok4 := mcpStr(args, "head")
	base, ok5 := mcpStr(args, "base")
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return nil, "missing required arguments: owner, repo, title, head, base"
	}
	body, _ := args["body"].(string)
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if owner != viewerLogin {
		return nil, "repository not found"
	}
	repo := s.store.getRepo(owner, name)
	if repo == nil {
		return nil, "repository not found"
	}
	headSHA, ok := repo.refs["refs/heads/"+head]
	if !ok {
		return nil, "head branch does not exist: " + head
	}
	baseSHA, ok := repo.refs["refs/heads/"+base]
	if !ok {
		return nil, "base branch does not exist: " + base
	}
	p := s.store.newPull(repo, title, body, head, base, headSHA, baseSHA)
	return repo.pullToJSON(p), ""
}

func (s *Server) mcpMergePullRequest(args map[string]any) (any, string) {
	owner, ok1 := mcpStr(args, "owner")
	name, ok2 := mcpStr(args, "repo")
	num, ok3 := mcpInt(args, "pull_number")
	if !ok1 || !ok2 || !ok3 {
		return nil, "missing required arguments: owner, repo, pull_number"
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if owner != viewerLogin {
		return nil, "repository not found"
	}
	repo := s.store.getRepo(owner, name)
	if repo == nil {
		return nil, "repository not found"
	}
	p, ok := repo.pulls[num]
	if !ok {
		return nil, "pull request not found"
	}
	mergeOID, conflicted, msg := s.mergePullRequest(repo, p)
	if conflicted {
		return nil, msg
	}
	return map[string]any{
		"merged": true, "sha": mergeOID, "number": p.Number,
		"state": p.State, "merge_commit_sha": mergeOID,
	}, ""
}
