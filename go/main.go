// Meridian Forge — server entrypoint: routing, auth, git smart-HTTP handlers,
// and the trial lifecycle (register / done-poll / standalone mode).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Server is the forge HTTP server.
type Server struct {
	store *Store
	token string // MAKEFUN_SHOP_TOKEN; "" accepts any non-empty bearer (local only)
}

func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	tok := strings.TrimPrefix(h, "Bearer ")
	if tok == "" {
		return false
	}
	if s.token == "" {
		return true
	}
	return tok == s.token
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every reply carries a request id (the protocol suite checks 401s too).
	w.Header().Set("X-GitHub-Request-Id", s.store.nextRequestID())
	if r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Unauthorized"})
		return
	}
	path := r.URL.Path
	switch {
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		s.handleREST(w, r)
	case path == "/api/graphql":
		s.handleGraphQL(w, r)
	case path == "/mcp":
		s.handleMCP(w, r)
	default:
		if !s.tryGitRoute(w, r) {
			writeErr(w, http.StatusNotFound, "Not Found")
		}
	}
}

// ---- git smart-HTTP routes ----

// tryGitRoute handles /{owner}/{repo}.git/{info/refs,git-upload-pack,git-receive-pack}.
// Returns false when the path is not a git route.
func (s *Server) tryGitRoute(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		return false
	}
	owner := parts[0]
	if !strings.HasSuffix(parts[1], ".git") {
		return false
	}
	repoName := strings.TrimSuffix(parts[1], ".git")
	service := strings.Join(parts[2:], "/")
	if owner != viewerLogin {
		writeErr(w, http.StatusNotFound, "Not Found")
		return true
	}
	switch service {
	case "info/refs":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "Not Found")
			return true
		}
		s.handleInfoRefs(w, r, owner, repoName)
	case "git-upload-pack":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusNotFound, "Not Found")
			return true
		}
		s.handleUploadPack(w, r, owner, repoName)
	case "git-receive-pack":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusNotFound, "Not Found")
			return true
		}
		s.handleReceivePack(w, r, owner, repoName)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
	return true
}

func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.mu.RLock()
	repo := s.store.getRepo(owner, repoName)
	if repo == nil {
		s.store.mu.RUnlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	// protocol v2 advertisement (upload-pack only, static capabilities)
	if service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2" {
		s.store.mu.RUnlock()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Write(advV2Bytes)
		return
	}
	// v0 advertisement, cached per repo generation
	var cached []byte
	var gen uint64
	if service == "git-upload-pack" {
		cached, gen = repo.advUpV0, repo.advUpGen
	} else {
		cached, gen = repo.advRecvV0, repo.advRecvGen
	}
	if cached != nil && gen == repo.gen {
		s.store.mu.RUnlock()
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		w.Write(cached)
		return
	}
	body := advertisementV0(repo, service)
	curGen := repo.gen
	s.store.mu.RUnlock()
	s.store.mu.Lock()
	if repo.gen == curGen {
		if service == "git-upload-pack" {
			repo.advUpV0, repo.advUpGen = body, curGen
		} else {
			repo.advRecvV0, repo.advRecvGen = body, curGen
		}
	}
	s.store.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Write(body)
}

func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read request body")
		return
	}
	// v2 if the header says so or the first line is a command=
	isV2 := r.Header.Get("Git-Protocol") == "version=2"
	if !isV2 && len(body) >= 4 {
		if pkts, _ := parsePktLines(body); len(pkts) > 0 && pkts[0].kind == "data" && strings.HasPrefix(pkts[0].text, "command=") {
			isV2 = true
		}
	}
	s.store.mu.RLock()
	repo := s.store.getRepo(owner, repoName)
	if repo == nil {
		s.store.mu.RUnlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	if isV2 {
		s.handleUploadPackV2(w, body, repo)
	} else {
		s.handleUploadPackV0(w, body, repo)
	}
}

// handleUploadPackV2 serves ls-refs / fetch. The caller holds the read lock;
// this function releases it before returning (it may upgrade to write it for
// cache storage).
func (s *Server) handleUploadPackV2(w http.ResponseWriter, body []byte, repo *Repo) {
	command, args, ok := parseV2Request(body)
	if !ok {
		s.store.mu.RUnlock()
		writeErr(w, http.StatusBadRequest, "malformed protocol v2 request")
		return
	}
	switch command {
	case "ls-refs":
		var a lsRefsArgs
		for _, arg := range args {
			switch {
			case arg == "peel":
				a.peel = true
			case arg == "symrefs":
				a.symrefs = true
			case arg == "unborn":
				a.unborn = true
			case strings.HasPrefix(arg, "ref-prefix "):
				a.prefixes = append(a.prefixes, strings.TrimPrefix(arg, "ref-prefix "))
			}
		}
		key := "ls:" + strings.Join(a.prefixes, ",") + fmt.Sprintf("|%v%v%v", a.peel, a.symrefs, a.unborn)
		if c, ok := repo.lsCache[key]; ok && c.gen == repo.gen {
			resp := c.body
			s.store.mu.RUnlock()
			w.Write(resp)
			return
		}
		resp := repo.lsRefsResponse(a)
		gen := repo.gen
		s.store.mu.RUnlock()
		s.store.mu.Lock()
		if repo.gen == gen {
			repo.lsCache[key] = cachedResp{gen: gen, body: resp}
		}
		s.store.mu.Unlock()
		w.Write(resp)
	case "fetch":
		req := parseFetchArgs(args)
		key := fetchCacheKey("v2", req)
		if c, ok := repo.fetchCache[key]; ok && c.gen == repo.gen {
			resp := c.body
			s.store.mu.RUnlock()
			w.Write(resp)
			return
		}
		resp := repo.fetchResponseV2(req)
		gen := repo.gen
		s.store.mu.RUnlock()
		s.store.mu.Lock()
		if repo.gen == gen {
			repo.fetchCache[key] = cachedResp{gen: gen, body: resp}
		}
		s.store.mu.Unlock()
		w.Write(resp)
	default:
		s.store.mu.RUnlock()
		writeErr(w, http.StatusBadRequest, "unsupported command: "+command)
	}
}

// handleUploadPackV0 serves the v0 fetch. The caller holds the read lock.
func (s *Server) handleUploadPackV0(w http.ResponseWriter, body []byte, repo *Repo) {
	req := fetchRequest{depth: -1}
	for _, p := range parseAllPktLines(body) {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(p.text, "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			oid := line[5:]
			if i := strings.Index(oid, " "); i >= 0 {
				oid = oid[:i] // capabilities follow on the first want line
			}
			req.wants = append(req.wants, oid)
		case strings.HasPrefix(line, "have "):
			req.haves = append(req.haves, line[5:])
		case strings.HasPrefix(line, "shallow "):
			req.shallow = append(req.shallow, line[8:])
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(line[7:]); err == nil {
				req.depth = n
			}
		case strings.HasPrefix(line, "deepen-since "):
			if n, err := strconv.ParseInt(line[13:], 10, 64); err == nil {
				req.deepenSince = n
			}
		case strings.HasPrefix(line, "deepen-not "):
			req.deepenNot = append(req.deepenNot, line[11:])
		}
	}
	key := fetchCacheKey("v0", req)
	if c, ok := repo.fetchCache[key]; ok && c.gen == repo.gen {
		resp := c.body
		s.store.mu.RUnlock()
		w.Write(resp)
		return
	}
	resp := repo.fetchResponseV0(req)
	gen := repo.gen
	s.store.mu.RUnlock()
	s.store.mu.Lock()
	if repo.gen == gen {
		repo.fetchCache[key] = cachedResp{gen: gen, body: resp}
	}
	s.store.mu.Unlock()
	w.Write(resp)
}

func parseFetchArgs(args []string) fetchRequest {
	req := fetchRequest{depth: -1}
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			req.wants = append(req.wants, arg[5:])
		case strings.HasPrefix(arg, "have "):
			req.haves = append(req.haves, arg[5:])
		case strings.HasPrefix(arg, "shallow "):
			req.shallow = append(req.shallow, arg[8:])
		case strings.HasPrefix(arg, "deepen "):
			if n, err := strconv.Atoi(arg[7:]); err == nil {
				req.depth = n
			}
		case strings.HasPrefix(arg, "deepen-since "):
			if n, err := strconv.ParseInt(arg[13:], 10, 64); err == nil {
				req.deepenSince = n
			}
		case strings.HasPrefix(arg, "deepen-not "):
			req.deepenNot = append(req.deepenNot, arg[11:])
		}
	}
	return req
}

func fetchCacheKey(prefix string, req fetchRequest) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("|w:")
	for _, w := range req.wants {
		b.WriteString(w)
		b.WriteString(",")
	}
	b.WriteString("|h:")
	for _, h := range req.haves {
		b.WriteString(h)
		b.WriteString(",")
	}
	b.WriteString("|s:")
	for _, sh := range req.shallow {
		b.WriteString(sh)
		b.WriteString(",")
	}
	b.WriteString("|d:")
	b.WriteString(strconv.Itoa(req.depth))
	b.WriteString("|ds:")
	b.WriteString(strconv.FormatInt(req.deepenSince, 10))
	b.WriteString("|dn:")
	for _, dn := range req.deepenNot {
		b.WriteString(dn)
		b.WriteString(",")
	}
	return b.String()
}

// parseAllPktLines parses every pkt-line in a stream (multiple flushes ok).
func parseAllPktLines(b []byte) []pkt {
	var out []pkt
	off := 0
	for off+4 <= len(b) {
		if string(b[off:off+4]) == "0000" {
			out = append(out, pkt{kind: "flush"})
			off += 4
			continue
		}
		if string(b[off:off+4]) == "0001" {
			out = append(out, pkt{kind: "delim"})
			off += 4
			continue
		}
		if string(b[off:off+4]) == "0002" {
			out = append(out, pkt{kind: "end"})
			off += 4
			continue
		}
		n, err := strconv.ParseInt(string(b[off:off+4]), 16, 32)
		if err != nil || n < 4 || off+int(n) > len(b) {
			break
		}
		out = append(out, pkt{kind: "data", text: string(b[off+4 : off+int(n)])})
		off += int(n)
	}
	return out
}

func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read request body")
		return
	}
	pkts, packOff := parsePktLines(body)
	var cmds []pushCommand
	useSideband := false
	for _, p := range pkts {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(p.text, "\n")
		caps := ""
		if i := strings.Index(line, "\x00"); i >= 0 {
			caps = line[i+1:]
			line = line[:i]
		}
		if strings.Contains(caps, "side-band-64k") {
			useSideband = true
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		cmds = append(cmds, pushCommand{oldOID: fields[0], newOID: fields[1], ref: fields[2]})
	}
	var pack []byte
	if packOff < len(body) {
		pack = body[packOff:]
	}
	s.store.mu.Lock()
	repo := s.store.getRepo(owner, repoName)
	if repo == nil {
		s.store.mu.Unlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	report, _, _ := repo.receivePack(cmds, pack, nil)
	s.store.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	var out bytes.Buffer
	if useSideband {
		// one sideband packet per report line (each packet is a whole line)
		for _, line := range report {
			payload := append([]byte{1}, []byte(line+"\n")...)
			out.WriteString(fmt.Sprintf("%04x", len(payload)+4))
			out.Write(payload)
		}
	} else {
		for _, line := range report {
			out.Write(pktLine(line + "\n"))
		}
	}
	out.Write(pktFlush())
	w.Write(out.Bytes())
}

// ---- lifecycle ----

func parseClockBase() int64 {
	v := os.Getenv("MAKEFUN_TRIAL_NOW")
	if v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.Unix()
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallbackEpochSec
}

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	store := NewStore(parseClockBase())
	srv := &Server{store: store, token: token}

	// Bind: PORT env, else an ephemeral port (never a fixed 8080).
	addr := ":0"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "meridian-forge: listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	httpSrv := &http.Server{Handler: srv}

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		// Standalone mode (local testing): serve until killed.
		fmt.Fprintf(os.Stderr, "meridian-forge: listening on port %d (standalone)\n", port)
		fmt.Printf("meridian-forge listening on http://127.0.0.1:%d\n", port)
		go func() {
			_ = httpSrv.Serve(ln)
		}()
		waitForSignal()
		_ = httpSrv.Close()
		return
	}

	// Trial mode: register the port, serve, poll /done.
	if err := registerPort(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "meridian-forge: register:", err)
		os.Exit(1)
	}
	go func() {
		_ = httpSrv.Serve(ln)
	}()
	fmt.Fprintf(os.Stderr, "meridian-forge: registered port %d, polling done\n", port)
	pollDone(envURL)
	_ = httpSrv.Close()
	os.Exit(0)
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
}

func registerPort(envURL string, port int) error {
	payload, _ := json.Marshal(map[string]int{"port": port})
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(envURL, "/")+"/register", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
				return nil
			}
			lastErr = fmt.Errorf("register status %d", resp.StatusCode)
		}
		time.Sleep(time.Duration(250+attempt*100) * time.Millisecond)
	}
	return lastErr
}

// pollDone polls GET $MAKEFUN_ENV_URL/done until {"done": true}.
// The endpoint answers at once; poll with backoff, never long-hold.
func pollDone(envURL string) {
	url := strings.TrimSuffix(envURL, "/") + "/done"
	delay := 250 * time.Millisecond
	for {
		if checkDoneOnce(url) {
			return
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func checkDoneOnce(url string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct {
		Done bool `json:"done"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	return body.Done
}
