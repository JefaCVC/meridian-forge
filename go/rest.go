// Meridian Forge — REST v3 API under /api/v3.
//
// Every reply is application/json with a non-empty X-GitHub-Request-Id.
// Unknown routes are 404 JSON, malformed bodies 400 JSON, and every 4xx
// body carries a string `message`.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeRawJSON(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// ---- response shapes ----

type userJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

func viewerJSON() userJSON { return userJSON{Login: viewerLogin, ID: viewerID, Type: "User"} }

type repoJSON struct {
	ID            int      `json:"id"`
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         userJSON `json:"owner"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	HasIssues     bool     `json:"has_issues"`
}

func (s *Server) repoToJSON(r *Repo) repoJSON {
	return repoJSON{
		ID: r.ID, NodeID: r.globalID(), Name: r.Name, FullName: r.fullName(),
		Owner: viewerJSON(), Private: r.Private, Description: r.Description,
		DefaultBranch: r.DefaultBranch, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		HasIssues: true,
	}
}

type labelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueJSON struct {
	ID        int         `json:"id"`
	NodeID    string      `json:"node_id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      userJSON    `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	Comments  int         `json:"comments"`
}

func (r *Repo) issueGlobalID(iss *Issue) string {
	return encodeGlobalID("I", strconv.Itoa(r.ID), strconv.Itoa(iss.Number))
}

func (r *Repo) issueToJSON(iss *Issue) issueJSON {
	labels := make([]labelJSON, 0, len(iss.Labels))
	for _, l := range iss.Labels {
		labels = append(labels, labelJSON{Name: l.Name, Color: l.Color})
	}
	return issueJSON{
		ID: iss.ID, NodeID: iss.nodeID, Number: iss.Number,
		Title: iss.Title, Body: iss.Body, State: iss.State, Labels: labels,
		User: viewerJSON(), CreatedAt: iss.CreatedAt, UpdatedAt: iss.UpdatedAt,
		Comments: len(iss.Comments),
	}
}

// viewerUserJSON is json.Marshal(viewerJSON()): the fixed user object every
// issue carries.
const viewerUserJSON = `{"login":"adaiversen","id":1000,"type":"User"}`

// appendJSONString appends s as a JSON string literal, byte-identical to
// encoding/json with HTML escaping on (what json.Marshal produces): " \ and
// control chars are escaped, < > & become \u003c \u003e \u0026, U+2028/2029
// are escaped, and invalid UTF-8 bytes become the literal U+FFFD rune.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '<':
				dst = append(dst, '\\', 'u', '0', '0', '3', 'c')
			case '>':
				dst = append(dst, '\\', 'u', '0', '0', '3', 'e')
			case '&':
				dst = append(dst, '\\', 'u', '0', '0', '2', '6')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexLower[c>>4], hexLower[c&0xf])
			}
			i++
			start = i
			continue
		}
		if c < 0x80 {
			i++
			continue
		}
		// Multibyte: validate UTF-8 the way encoding/json does.
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			dst = append(dst, "\uFFFD"...)
			i++
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			dst = append(dst, '\\', 'u', '2', '0', '2', '8'+byte(r-'\u2028'))
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// buildLabelsJSON pre-serializes an issue's labels array (labels never
// change after creation, so this is built once).
func buildLabelsJSON(labels []*Label) []byte {
	if len(labels) == 0 {
		return []byte("[]")
	}
	var b []byte
	b = append(b, '[')
	for i, l := range labels {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = appendJSONString(b, l.Name)
		b = append(b, `,"color":`...)
		b = appendJSONString(b, l.Color)
		b = append(b, '}')
	}
	return append(b, ']')
}

// refreshIssueJSON rebuilds the pre-serialized issue JSON after a mutation.
// Hand-rolled (no reflection, no intermediate structs); byte-identical to
// json.Marshal(issueToJSON(iss)).
func (r *Repo) refreshIssueJSON(iss *Issue) {
	b := make([]byte, 0, 256)
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, int64(iss.ID), 10)
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, iss.nodeID)
	b = append(b, `,"number":`...)
	b = strconv.AppendInt(b, int64(iss.Number), 10)
	b = append(b, `,"title":`...)
	b = appendJSONString(b, iss.Title)
	b = append(b, `,"body":`...)
	b = appendJSONString(b, iss.Body)
	b = append(b, `,"state":`...)
	b = appendJSONString(b, iss.State)
	b = append(b, `,"labels":`...)
	b = append(b, iss.labelsJSON...)
	b = append(b, `,"user":`...)
	b = append(b, viewerUserJSON...)
	b = append(b, `,"created_at":`...)
	b = appendJSONString(b, iss.CreatedAt)
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, iss.UpdatedAt)
	b = append(b, `,"comments":`...)
	b = strconv.AppendInt(b, int64(len(iss.Comments)), 10)
	b = append(b, '}')
	iss.jsonCache = b
}

type commentJSON struct {
	ID        int      `json:"id"`
	NodeID    string   `json:"node_id"`
	Body      string   `json:"body"`
	User      userJSON `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func (r *Repo) commentToJSON(iss *Issue, c *Comment) commentJSON {
	return commentJSON{
		ID: c.ID, NodeID: encodeGlobalID("C", strconv.Itoa(r.ID), strconv.Itoa(iss.Number), strconv.Itoa(c.ID)),
		Body: c.Body, User: viewerJSON(), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

type prRefJSON struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
}

type pullJSON struct {
	ID             int        `json:"id"`
	NodeID         string     `json:"node_id"`
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	Head           prRefJSON  `json:"head"`
	Base           prRefJSON  `json:"base"`
	Merged         bool       `json:"merged"`
	MergedAt       *string    `json:"merged_at"`
	MergeCommitSHA *string    `json:"merge_commit_sha"`
	User           userJSON   `json:"user"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
}

func (r *Repo) pullGlobalID(p *Pull) string {
	return encodeGlobalID("P", strconv.Itoa(r.ID), strconv.Itoa(p.Number))
}

func (r *Repo) pullToJSON(p *Pull) pullJSON {
	var mergedAt, mergeSHA *string
	if p.Merged {
		mergedAt = &p.MergedAt
		mergeSHA = &p.MergeCommitSHA
	}
	return pullJSON{
		ID: p.ID, NodeID: r.pullGlobalID(p), Number: p.Number,
		Title: p.Title, Body: p.Body, State: p.State,
		Head: prRefJSON{Ref: p.HeadRef, SHA: p.HeadSHA, Label: viewerLogin + ":" + p.HeadRef},
		Base: prRefJSON{Ref: p.BaseRef, SHA: p.BaseSHA, Label: viewerLogin + ":" + p.BaseRef},
		Merged: p.Merged, MergedAt: mergedAt, MergeCommitSHA: mergeSHA,
		User: viewerJSON(), CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// ---- routing ----

// handleREST dispatches /api/v3/* requests. The caller has already checked auth.
func (s *Server) handleREST(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	path = strings.TrimPrefix(path, "/")
	parts := []string{}
	if path != "" {
		parts = strings.Split(path, "/")
	}
	// Root and meta.
	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, user")
		base := "http://" + r.Host + "/api/v3"
		writeJSON(w, http.StatusOK, map[string]string{
			"current_user_url": base + "/user",
			"repository_url":   base + "/repos/{owner}/{repo}",
			"user_url":         base + "/users/{user}",
		})
		return
	}
	switch parts[0] {
	case "meta":
		if r.Method != http.MethodGet || len(parts) != 1 {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"installed_version": "1.0.0"})
		return
	case "user":
		s.handleUser(w, r, parts[1:])
		return
	case "repos":
		s.handleRepos(w, r, parts[1:])
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"login": viewerLogin, "id": viewerID, "name": viewerName,
			"email": viewerEmail, "type": "User",
		})
		return
	}
	if len(parts) == 1 && parts[0] == "repos" {
		switch r.Method {
		case http.MethodGet:
			s.listUserRepos(w, r)
		case http.MethodPost:
			s.createUserRepo(w, r)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *Server) listUserRepos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage := atoiDefault(q.Get("per_page"), 30)
	page := atoiDefault(q.Get("page"), 1)
	if perPage < 1 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var all []repoJSON
	for _, key := range s.store.repoOrder {
		if repo, ok := s.store.repos[key]; ok && repo.Owner == viewerLogin {
			all = append(all, s.repoToJSON(repo))
		}
	}
	start := (page - 1) * perPage
	if start > len(all) {
		start = len(all)
	}
	end := start + perPage
	if end > len(all) {
		end = len(all)
	}
	if end < len(all) {
		// absolute same-origin next link
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		next := fmt.Sprintf("%s://%s/api/v3/user/repos?per_page=%d&page=%d", scheme, r.Host, perPage, page+1)
		w.Header().Set("Link", "<"+next+">; rel=\"next\"")
	}
	writeJSON(w, http.StatusOK, all[start:end])
}

func (s *Server) createUserRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Private     bool   `json:"private"`
		Description string `json:"description"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.store.createRepo(body.Name, body.Private, body.Description)
	if repo == nil {
		writeErr(w, http.StatusUnprocessableEntity, "name already exists on this account")
		return
	}
	writeJSON(w, http.StatusCreated, s.repoToJSON(repo))
}

// handleRepos dispatches /api/v3/repos/{owner}/{repo}/...
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) < 2 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	owner, name := parts[0], parts[1]
	if owner != viewerLogin {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	rest := parts[2:]
	// Repo root: GET / PATCH / DELETE
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.store.mu.RLock()
			repo := s.store.getRepo(owner, name)
			if repo == nil {
				s.store.mu.RUnlock()
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			j := s.repoToJSON(repo)
			s.store.mu.RUnlock()
			writeJSON(w, http.StatusOK, j)
		case http.MethodPatch:
			var body struct {
				Description *string `json:"description"`
				Private     *bool   `json:"private"`
			}
			if err := decodeJSONBody(r, &body); err != nil {
				writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
				return
			}
			s.store.mu.Lock()
			repo := s.store.getRepo(owner, name)
			if repo == nil {
				s.store.mu.Unlock()
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			if body.Description != nil {
				repo.Description = *body.Description
			}
			if body.Private != nil {
				repo.Private = *body.Private
			}
			repo.UpdatedAt = s.store.nextTimeISO()
			j := s.repoToJSON(repo)
			s.store.mu.Unlock()
			writeJSON(w, http.StatusOK, j)
		case http.MethodDelete:
			s.store.mu.Lock()
			ok := s.store.deleteRepo(owner, name)
			s.store.mu.Unlock()
			if !ok {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	switch rest[0] {
	case "branches":
		s.handleBranches(w, r, owner, name, rest[1:])
	case "commits":
		s.handleCommits(w, r, owner, name, rest[1:])
	case "git":
		s.handleGit(w, r, owner, name, rest[1:])
	case "contents":
		s.handleContents(w, r, owner, name, rest[1:])
	case "labels":
		s.handleLabels(w, r, owner, name, rest[1:])
	case "issues":
		s.handleIssues(w, r, owner, name, rest[1:])
	case "pulls":
		s.handlePulls(w, r, owner, name, rest[1:])
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// repoOr404 loads a repo or writes a 404. Returns nil on 404.
func (s *Server) repoOr404(w http.ResponseWriter, owner, name string) *Repo {
	repo := s.store.getRepo(owner, name)
	if repo == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

// ---- branches ----

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request, owner, name string, parts []string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	if len(parts) == 0 {
		// list branches, sorted by name
		var names []string
		for ref := range repo.refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
			}
		}
		sort.Strings(names)
		out := make([]map[string]any, 0, len(names))
		for _, n := range names {
			out = append(out, map[string]any{
				"name":   n,
				"commit": map[string]string{"sha": repo.refs["refs/heads/"+n]},
			})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	branch := strings.Join(parts, "/")
	sha, ok := repo.refs["refs/heads/"+branch]
	if !ok {
		writeErr(w, http.StatusNotFound, "Branch not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":   branch,
		"commit": map[string]string{"sha": sha},
	})
}

// ---- commits ----

func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request, owner, name string, parts []string) {
	if r.Method != http.MethodGet || len(parts) != 0 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	q := r.URL.Query()
	perPage := atoiDefault(q.Get("per_page"), 30)
	page := atoiDefault(q.Get("page"), 1)
	if perPage < 1 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	ref := q.Get("sha")
	tip := ""
	if ref == "" {
		tip = repo.headOID()
	} else if sha, ok := repo.refs["refs/heads/"+ref]; ok {
		tip = sha
	} else if sha, ok := repo.refs[ref]; ok {
		tip = sha
	} else if obj := repo.objects[ref]; obj != nil && obj.Type == objCommit {
		tip = ref
	}
	out := []map[string]any{}
	if tip != "" {
		list := repo.commitList(tip)
		start := (page - 1) * perPage
		if start > len(list) {
			start = len(list)
		}
		end := start + perPage
		if end > len(list) {
			end = len(list)
		}
		for _, sha := range list[start:end] {
			msg := repo.commitMessage[sha]
			out = append(out, map[string]any{
				"sha": sha,
				"commit": map[string]any{
					"message": strings.TrimRight(msg, "\n"),
					"author":  map[string]string{"name": "Meridian Forge", "email": "forge@meridian-forge.local"},
				},
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- git refs ----

func (s *Server) handleGit(w http.ResponseWriter, r *http.Request, owner, name string, parts []string) {
	if len(parts) == 0 || parts[0] != "refs" {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPost {
		var body struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
			return
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		repo := s.repoOr404(w, owner, name)
		if repo == nil {
			return
		}
		if body.Ref == "" || !strings.HasPrefix(body.Ref, "refs/") {
			writeErr(w, http.StatusUnprocessableEntity, "ref must start with refs/")
			return
		}
		if _, exists := repo.refs[body.Ref]; exists {
			writeErr(w, http.StatusUnprocessableEntity, "Reference already exists")
			return
		}
		obj := repo.objects[body.SHA]
		if obj == nil {
			writeErr(w, http.StatusUnprocessableEntity, "Object does not exist")
			return
		}
		repo.refs[body.Ref] = body.SHA
		repo.gen++
		writeJSON(w, http.StatusCreated, map[string]any{
			"ref":    body.Ref,
			"object": map[string]string{"sha": body.SHA, "type": typeName(obj.Type)},
		})
		return
	}
	if len(parts) >= 2 && r.Method == http.MethodGet {
		ref := "refs/" + strings.Join(parts[1:], "/")
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
		repo := s.repoOr404(w, owner, name)
		if repo == nil {
			return
		}
		sha, ok := repo.refs[ref]
		if !ok {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		typ := ""
		if obj := repo.objects[sha]; obj != nil {
			typ = typeName(obj.Type)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ref":    ref,
			"object": map[string]string{"sha": sha, "type": typ},
		})
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// ---- contents ----

func (s *Server) handleContents(w http.ResponseWriter, r *http.Request, owner, name string, parts []string) {
	path := strings.Join(parts, "/")
	switch r.Method {
	case http.MethodGet:
		s.contentsGet(w, r, owner, name, path)
	case http.MethodPut:
		s.contentsPut(w, r, owner, name, path)
	case http.MethodDelete:
		s.contentsDelete(w, r, owner, name, path)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// resolveRefTip resolves a branch name or commit sha to a commit oid.
func (r *Repo) resolveRefTip(ref string) string {
	if ref == "" {
		return r.headOID()
	}
	if sha, ok := r.refs["refs/heads/"+ref]; ok {
		return sha
	}
	if sha, ok := r.refs[ref]; ok {
		return repoPeel(r, sha)
	}
	if obj := r.objects[ref]; obj != nil && obj.Type == objCommit {
		return ref
	}
	return ""
}

func repoPeel(r *Repo, oid string) string {
	if obj := r.objects[oid]; obj != nil && obj.Type == objTag {
		return r.peelToCommit(oid)
	}
	return oid
}

func (s *Server) contentsGet(w http.ResponseWriter, r *http.Request, owner, name, path string) {
	ref := r.URL.Query().Get("ref")
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	tip := repo.resolveRefTip(ref)
	if tip == "" {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	entries := repo.treeEntriesAt(tip)
	// file?
	if e, ok := entries[path]; ok {
		obj := repo.objects[e.OID]
		content := ""
		if obj != nil {
			content = base64.StdEncoding.EncodeToString(obj.Data)
		}
		size := 0
		if obj != nil {
			size = len(obj.Data)
		}
		base := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			base = path[i+1:]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type": "file", "encoding": "base64", "content": content,
			"sha": e.OID, "size": size, "name": base, "path": path,
		})
		return
	}
	// directory? collect direct children of path/
	prefix := path
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]bool{}
	var out []map[string]any
	for p, e := range entries {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		seg := rest
		isDir := false
		if i := strings.Index(rest, "/"); i >= 0 {
			seg = rest[:i]
			isDir = true
		}
		key := seg
		if seen[key] {
			continue
		}
		seen[key] = true
		typ := "file"
		sha := e.OID
		size := 0
		if isDir {
			typ = "dir"
			sha = ""
		} else if obj := repo.objects[e.OID]; obj != nil {
			size = len(obj.Data)
		}
		out = append(out, map[string]any{
			"type": typ, "name": seg, "path": prefix + seg, "sha": sha, "size": size,
		})
	}
	if len(out) == 0 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) contentsPut(w http.ResponseWriter, r *http.Request, owner, name, path string) {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if body.Message == "" {
		body.Message = "Update " + path
	}
	data, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "content is not valid base64")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch
	tip, branchExists := repo.refs[refName]
	if !branchExists {
		// unknown branch: create it from the default tip (forgiving)
		tip = repo.headOID()
	}
	// stale-sha check
	status := http.StatusCreated
	if tip != "" {
		if cur := repo.blobAt(tip, path); cur != "" {
			status = http.StatusOK
			if body.SHA != "" && body.SHA != cur {
				writeErr(w, http.StatusConflict, "sha does not match the current blob")
				return
			}
		}
	}
	blobOID := repo.putObject(objBlob, data)
	treeOID := ""
	if tip != "" {
		treeOID = repo.commitTree[tip]
	}
	newTree := repo.setPathInTree(treeOID, path, blobOID, "100644")
	epoch := s.store.nextTime()
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commitOID := repo.putObject(objCommit, buildCommit(newTree, parents, body.Message, epoch))
	repo.refs[refName] = commitOID
	repo.gen++
	writeJSON(w, status, map[string]any{
		"content": map[string]any{
			"name": path[strings.LastIndex(path, "/")+1:], "path": path,
			"sha": blobOID, "size": len(data), "encoding": "base64",
			"content": body.Content, "type": "file",
		},
		"commit": map[string]any{"sha": commitOID, "message": body.Message},
	})
}

func (s *Server) contentsDelete(w http.ResponseWriter, r *http.Request, owner, name, path string) {
	var body struct {
		Message string `json:"message"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch
	tip, ok := repo.refs[refName]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	cur := repo.blobAt(tip, path)
	if cur == "" {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.SHA != "" && body.SHA != cur {
		writeErr(w, http.StatusConflict, "sha does not match the current blob")
		return
	}
	if body.Message == "" {
		body.Message = "Delete " + path
	}
	newTree := repo.removePathFromTree(repo.commitTree[tip], path)
	epoch := s.store.nextTime()
	commitOID := repo.putObject(objCommit, buildCommit(newTree, []string{tip}, body.Message, epoch))
	repo.refs[refName] = commitOID
	repo.gen++
	writeJSON(w, http.StatusOK, map[string]any{
		"content": nil,
		"commit":  map[string]any{"sha": commitOID, "message": body.Message},
	})
}

// setPathInTree returns a new tree oid with path set to blobOID (creating
// intermediate trees). treeOID may be "" for an empty tree.
func (r *Repo) setPathInTree(treeOID, path, blobOID, mode string) string {
	seg, rest := splitPath(path)
	var entries []treeEntryRaw
	if treeOID != "" {
		if obj := r.objects[treeOID]; obj != nil && obj.Type == objTree {
			entries = parseTree(obj.Data)
		}
	}
	found := false
	for i, e := range entries {
		if e.Name != seg {
			continue
		}
		if rest == "" {
			entries[i].OID = blobOID
			entries[i].Mode = mode
		} else {
			if e.Mode != "40000" {
				entries[i].OID = r.setPathInTree("", rest, blobOID, mode)
				entries[i].Mode = "40000"
			} else {
				entries[i].OID = r.setPathInTree(e.OID, rest, blobOID, mode)
			}
		}
		found = true
		break
	}
	if !found {
		var oid, m string
		if rest == "" {
			oid, m = blobOID, mode
		} else {
			oid, m = r.setPathInTree("", rest, blobOID, mode), "40000"
		}
		entries = append(entries, treeEntryRaw{Mode: m, Name: seg, OID: oid})
	}
	return r.putObject(objTree, buildTree(entries))
}

// removePathFromTree returns a new tree oid with path removed.
func (r *Repo) removePathFromTree(treeOID, path string) string {
	seg, rest := splitPath(path)
	var entries []treeEntryRaw
	if treeOID != "" {
		if obj := r.objects[treeOID]; obj != nil && obj.Type == objTree {
			entries = parseTree(obj.Data)
		}
	}
	var out []treeEntryRaw
	for _, e := range entries {
		if e.Name != seg {
			out = append(out, e)
			continue
		}
		if rest != "" && e.Mode == "40000" {
			sub := r.removePathFromTree(e.OID, rest)
			// keep the (possibly empty) subtree; an empty tree serializes fine
			out = append(out, treeEntryRaw{Mode: "40000", Name: seg, OID: sub})
		}
		// else: drop the entry
	}
	return r.putObject(objTree, buildTree(out))
}

func splitPath(path string) (string, string) {
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i], path[i+1:]
	}
	return path, ""
}

// buildTreeFromPaths builds nested tree objects from a flat path map.
func (r *Repo) buildTreeFromPaths(paths map[string]treeEntry) string {
	// group by first segment
	type node struct {
		entries map[string]treeEntryRaw // direct entries (files) + subdirs (filled below)
		dirs    map[string]map[string]treeEntry
	}
	var build func(m map[string]treeEntry) string
	build = func(m map[string]treeEntry) string {
		files := map[string]treeEntryRaw{}
		dirs := map[string]map[string]treeEntry{}
		for p, e := range m {
			seg, rest := splitPath(p)
			if rest == "" {
				files[seg] = treeEntryRaw{Mode: e.Mode, Name: seg, OID: e.OID}
			} else {
				if dirs[seg] == nil {
					dirs[seg] = map[string]treeEntry{}
				}
				dirs[seg][rest] = e
			}
		}
		var entries []treeEntryRaw
		for _, f := range files {
			entries = append(entries, f)
		}
		for d, sub := range dirs {
			entries = append(entries, treeEntryRaw{Mode: "40000", Name: d, OID: build(sub)})
		}
		return r.putObject(objTree, buildTree(entries))
	}
	return build(paths)
}

// ---- labels ----

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request, owner, name string, parts []string) {
	if len(parts) != 0 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
		repo := s.repoOr404(w, owner, name)
		if repo == nil {
			return
		}
		out := make([]labelJSON, 0, len(repo.labelOrder))
		for _, n := range repo.labelOrder {
			l := repo.labels[n]
			out = append(out, labelJSON{Name: l.Name, Color: l.Color})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
			return
		}
		if body.Name == "" {
			writeErr(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		repo := s.repoOr404(w, owner, name)
		if repo == nil {
			return
		}
		if l, ok := repo.labels[body.Name]; ok {
			writeJSON(w, http.StatusCreated, labelJSON{Name: l.Name, Color: l.Color})
			return
		}
		l := &Label{Name: body.Name, Color: body.Color}
		repo.labels[body.Name] = l
		repo.labelOrder = append(repo.labelOrder, body.Name)
		writeJSON(w, http.StatusCreated, labelJSON{Name: l.Name, Color: l.Color})
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// ---- issues ----

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request, owner, name string, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.listIssues(w, r, owner, name)
		case http.MethodPost:
			s.createIssue(w, r, owner, name)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.getIssue(w, r, owner, name, num)
		case http.MethodPatch:
			s.patchIssue(w, r, owner, name, num)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	if len(parts) == 2 && parts[1] == "comments" {
		switch r.Method {
		case http.MethodGet:
			s.listComments(w, r, owner, name, num)
		case http.MethodPost:
			s.createComment(w, r, owner, name, num)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request, owner, name string) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage := atoiDefault(q.Get("per_page"), 30)
	page := atoiDefault(q.Get("page"), 1)
	if perPage < 1 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	var nums []int
	for _, n := range repo.issueOrder {
		iss := repo.issues[n]
		if state == "all" || iss.State == state {
			nums = append(nums, n)
		}
	}
	if direction == "desc" {
		for i, j := 0, len(nums)-1; i < j; i, j = i+1, j-1 {
			nums[i], nums[j] = nums[j], nums[i]
		}
	}
	start := (page - 1) * perPage
	if start > len(nums) {
		start = len(nums)
	}
	end := start + perPage
	if end > len(nums) {
		end = len(nums)
	}
	out := make([]issueJSON, 0, end-start)
	for _, n := range nums[start:end] {
		out = append(out, repo.issueToJSON(repo.issues[n]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request, owner, name string) {
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if body.Title == "" {
		writeErr(w, http.StatusUnprocessableEntity, "title is required")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	var labels []*Label
	for _, ln := range body.Labels {
		l, ok := repo.labels[ln]
		if !ok {
			writeErr(w, http.StatusUnprocessableEntity, "label does not exist: "+ln)
			return
		}
		labels = append(labels, l)
	}
	iss := s.store.newIssue(repo, body.Title, body.Body, labels)
	// serve the pre-serialized bytes (already built by newIssue)
	writeRawJSON(w, http.StatusCreated, iss.jsonCache)
}

// newIssue creates an issue (and its pre-serialized JSON). Caller holds the lock.
func (s *Store) newIssue(repo *Repo, title, body string, labels []*Label) *Issue {
	now := s.nextTimeISO()
	iss := &Issue{
		ID: s.nextIssueID, Number: repo.nextNumber,
		Title: title, Body: body, State: "open",
		Labels: labels, CreatedAt: now, UpdatedAt: now,
		CreatedSeq: s.clockSeq,
	}
	s.nextIssueID++
	repo.nextNumber++
	iss.nodeID = repo.issueGlobalID(iss)
	iss.labelsJSON = buildLabelsJSON(labels)
	repo.issues[iss.Number] = iss
	repo.issueOrder = append(repo.issueOrder, iss.Number)
	repo.refreshIssueJSON(iss)
	return iss
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request, owner, name string, num int) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	iss, ok := repo.issues[num]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	// hot path: serve the pre-serialized bytes verbatim
	writeRawJSON(w, http.StatusOK, iss.jsonCache)
}

func (s *Server) patchIssue(w http.ResponseWriter, r *http.Request, owner, name string, num int) {
	var body struct {
		State *string `json:"state"`
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	iss, ok := repo.issues[num]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.State != nil {
		iss.State = *body.State
	}
	if body.Title != nil {
		iss.Title = *body.Title
	}
	if body.Body != nil {
		iss.Body = *body.Body
	}
	iss.UpdatedAt = s.store.nextTimeISO()
	repo.refreshIssueJSON(iss)
	writeRawJSON(w, http.StatusOK, iss.jsonCache)
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request, owner, name string, num int) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	iss, ok := repo.issues[num]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	out := make([]commentJSON, 0, len(iss.Comments))
	for _, c := range iss.Comments {
		out = append(out, repo.commentToJSON(iss, c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request, owner, name string, num int) {
	var body struct {
		Body string `json:"body"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	iss, ok := repo.issues[num]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	now := s.store.nextTimeISO()
	c := &Comment{ID: s.store.nextCommentID, Body: body.Body, CreatedAt: now, UpdatedAt: now}
	s.store.nextCommentID++
	iss.Comments = append(iss.Comments, c)
	iss.UpdatedAt = now
	repo.refreshIssueJSON(iss)
	writeJSON(w, http.StatusCreated, repo.commentToJSON(iss, c))
}

// ---- pulls ----

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request, owner, name string, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.listPulls(w, r, owner, name)
		case http.MethodPost:
			s.createPull(w, r, owner, name)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(parts) == 1 {
		if r.Method == http.MethodGet {
			s.getPull(w, r, owner, name, num)
			return
		}
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(parts) == 2 && parts[1] == "files" && r.Method == http.MethodGet {
		s.pullFiles(w, r, owner, name, num)
		return
	}
	if len(parts) == 2 && parts[1] == "merge" && r.Method == http.MethodPut {
		s.mergePull(w, r, owner, name, num)
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request, owner, name string) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	perPage := atoiDefault(q.Get("per_page"), 30)
	page := atoiDefault(q.Get("page"), 1)
	if perPage < 1 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	var nums []int
	for i := len(repo.pullOrder) - 1; i >= 0; i-- {
		n := repo.pullOrder[i]
		p := repo.pulls[n]
		if state == "all" || p.State == state {
			nums = append(nums, n)
		}
	}
	start := (page - 1) * perPage
	if start > len(nums) {
		start = len(nums)
	}
	end := start + perPage
	if end > len(nums) {
		end = len(nums)
	}
	out := make([]pullJSON, 0, end-start)
	for _, n := range nums[start:end] {
		out = append(out, repo.pullToJSON(repo.pulls[n]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createPull(w http.ResponseWriter, r *http.Request, owner, name string) {
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	headSHA, ok := repo.refs["refs/heads/"+body.Head]
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "head branch does not exist: "+body.Head)
		return
	}
	baseSHA, ok := repo.refs["refs/heads/"+body.Base]
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "base branch does not exist: "+body.Base)
		return
	}
	p := s.store.newPull(repo, body.Title, body.Body, body.Head, body.Base, headSHA, baseSHA)
	writeJSON(w, http.StatusCreated, repo.pullToJSON(p))
}

// newPull creates a pull request. Caller holds the lock.
func (s *Store) newPull(repo *Repo, title, body, head, base, headSHA, baseSHA string) *Pull {
	now := s.nextTimeISO()
	p := &Pull{
		ID: s.nextPullID, Number: repo.nextNumber,
		Title: title, Body: body, State: "open",
		HeadRef: head, BaseRef: base, HeadSHA: headSHA, BaseSHA: baseSHA,
		CreatedAt: now, UpdatedAt: now, CreatedSeq: s.clockSeq,
	}
	s.nextPullID++
	repo.nextNumber++
	repo.pulls[p.Number] = p
	repo.pullOrder = append(repo.pullOrder, p.Number)
	return p
}

func (s *Server) getPull(w http.ResponseWriter, r *http.Request, owner, name string, num int) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	p, ok := repo.pulls[num]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, repo.pullToJSON(p))
}

// fileChange is one entry of a tree diff.
type fileChange struct {
	Path      string
	Status    string // added | modified | removed
	Additions int
	Deletions int
	OldOID    string
	NewOID    string
}

// diffTrees diffs two flat path maps (base -> head).
func (r *Repo) diffTrees(base, head map[string]treeEntry) []fileChange {
	var out []fileChange
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		b, inBase := base[p]
		h, inHead := head[p]
		switch {
		case inBase && !inHead:
			add, del := r.lineDiff(b.OID, "")
			out = append(out, fileChange{Path: p, Status: "removed", Additions: add, Deletions: del, OldOID: b.OID})
		case !inBase && inHead:
			add, del := r.lineDiff("", h.OID)
			out = append(out, fileChange{Path: p, Status: "added", Additions: add, Deletions: del, NewOID: h.OID})
		case b.OID != h.OID:
			add, del := r.lineDiff(b.OID, h.OID)
			out = append(out, fileChange{Path: p, Status: "modified", Additions: add, Deletions: del, OldOID: b.OID, NewOID: h.OID})
		}
	}
	return out
}

// lineDiff counts added/deleted lines between two blobs ("" = absent).
func (r *Repo) lineDiff(oldOID, newOID string) (additions, deletions int) {
	var oldLines, newLines []string
	if oldOID != "" {
		if obj := r.objects[oldOID]; obj != nil {
			oldLines = splitLines(string(obj.Data))
		}
	}
	if newOID != "" {
		if obj := r.objects[newOID]; obj != nil {
			newLines = splitLines(string(obj.Data))
		}
	}
	lcs := lcsLen(oldLines, newLines)
	return len(newLines) - lcs, len(oldLines) - lcs
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lcsLen is the length of the longest common subsequence of two line lists.
func lcsLen(a, b []string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func (s *Server) pullFiles(w http.ResponseWriter, r *http.Request, owner, name string, num int) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	p, ok := repo.pulls[num]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	baseTree := repo.treeEntriesAt(p.BaseSHA)
	headTree := repo.treeEntriesAt(p.HeadSHA)
	changes := repo.diffTrees(baseTree, headTree)
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		out = append(out, map[string]any{
			"filename": c.Path, "status": c.Status,
			"additions": c.Additions, "deletions": c.Deletions,
			"changes": c.Additions + c.Deletions,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// mergePullRequest performs the 3-way squash merge for pull p (shared by the
// REST and GraphQL surfaces). Caller must hold the write lock. Returns
// (mergeOID, conflicted, message).
func (s *Server) mergePullRequest(repo *Repo, p *Pull) (string, bool, string) {
	if p.State != "open" || p.Merged {
		return "", true, "pull request is already merged or closed"
	}
	baseTip := repo.refs["refs/heads/"+p.BaseRef]
	headTip := repo.refs["refs/heads/"+p.HeadRef]
	if baseTip == "" || headTip == "" {
		return "", true, "head or base branch is missing"
	}
	// 3-way merge: apply head's changes since the merge-base onto the base tip.
	mb := repo.mergeBase(baseTip, headTip)
	baseTree := repo.treeEntriesAt(baseTip)
	headTree := repo.treeEntriesAt(headTip)
	mbTree := map[string]treeEntry{}
	if mb != "" {
		mbTree = repo.treeEntriesAt(mb)
	}
	result := map[string]treeEntry{}
	for k, v := range baseTree {
		result[k] = v
	}
	conflicted := false
	headChanges := repo.diffTrees(mbTree, headTree)
	for _, c := range headChanges {
		mbEntry, inMB := mbTree[c.Path]
		baseEntry, inBase := baseTree[c.Path]
		// did the base side change this path since the merge-base?
		baseChanged := false
		if inMB != inBase {
			baseChanged = true
		} else if inMB && inBase && mbEntry.OID != baseEntry.OID {
			baseChanged = true
		}
		if baseChanged {
			// conflict unless both sides made the identical change
			headVersion, headHas := headTree[c.Path]
			if inBase != headHas {
				conflicted = true
				break
			}
			if inBase && headHas && baseEntry.OID != headVersion.OID {
				conflicted = true
				break
			}
			// identical change: nothing to apply
			continue
		}
		// apply head's version
		if c.Status == "removed" {
			delete(result, c.Path)
		} else {
			result[c.Path] = headTree[c.Path]
		}
	}
	if conflicted {
		return "", true, "pull request has merge conflicts"
	}
	newTree := repo.buildTreeFromPaths(result)
	epoch := s.store.nextTime()
	msg := p.Title + " (#" + strconv.Itoa(p.Number) + ")"
	mergeOID := repo.putObject(objCommit, buildCommit(newTree, []string{baseTip}, msg, epoch))
	repo.refs["refs/heads/"+p.BaseRef] = mergeOID
	repo.gen++
	now := s.store.nextTimeISO()
	p.State = "closed"
	p.Merged = true
	p.MergedAt = now
	p.MergeCommitSHA = mergeOID
	p.UpdatedAt = now
	return mergeOID, false, ""
}

func (s *Server) mergePull(w http.ResponseWriter, r *http.Request, owner, name string, num int) {
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	// body is optional-ish; tolerate empty/invalid
	_ = decodeJSONBody(r, &body)
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.repoOr404(w, owner, name)
	if repo == nil {
		return
	}
	p, ok := repo.pulls[num]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	mergeOID, conflicted, msg := s.mergePullRequest(repo, p)
	if conflicted {
		writeErr(w, http.StatusMethodNotAllowed, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sha": mergeOID, "merged": true, "message": "Pull Request successfully merged",
	})
}

// mergeableStatus reports the GraphQL mergeable enum for a pull.
func (r *Repo) mergeableStatus(p *Pull) string {
	if p.Merged || p.State != "open" {
		return "UNKNOWN"
	}
	baseTip := r.refs["refs/heads/"+p.BaseRef]
	headTip := r.refs["refs/heads/"+p.HeadRef]
	if baseTip == "" || headTip == "" {
		return "UNKNOWN"
	}
	mb := r.mergeBase(baseTip, headTip)
	baseTree := r.treeEntriesAt(baseTip)
	headTree := r.treeEntriesAt(headTip)
	mbTree := map[string]treeEntry{}
	if mb != "" {
		mbTree = r.treeEntriesAt(mb)
	}
	headChanges := r.diffTrees(mbTree, headTree)
	for _, c := range headChanges {
		mbEntry, inMB := mbTree[c.Path]
		baseEntry, inBase := baseTree[c.Path]
		baseChanged := false
		if inMB != inBase {
			baseChanged = true
		} else if inMB && inBase && mbEntry.OID != baseEntry.OID {
			baseChanged = true
		}
		if !baseChanged {
			continue
		}
		headVersion, headHas := headTree[c.Path]
		if inBase != headHas {
			return "CONFLICTING"
		}
		if inBase && headHas && baseEntry.OID != headVersion.OID {
			return "CONFLICTING"
		}
	}
	return "MERGEABLE"
}

// ---- misc helpers ----

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// decodeJSONBody decodes a JSON request body. An empty body decodes to the
// zero value (no error); malformed JSON returns an error.
func decodeJSONBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(v)
	if err != nil && err.Error() == "EOF" {
		return nil
	}
	return err
}
