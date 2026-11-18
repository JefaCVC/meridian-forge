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
	"runtime"
	"runtime/debug"
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
		if !s.tryIssueFastPath(w, r) {
			s.handleREST(w, r)
		}
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

// tryIssueFastPath serves the benchmark read/write hot paths — GET
// /api/v3/repos/{owner}/{repo}/issues/{number} and POST
// /api/v3/repos/{owner}/{repo}/issues — with index-scan parsing instead of
// the generic router's Split chain. Anything that does not match the pattern
// exactly (odd numbers, extra segments, other methods) returns false and the
// generic router handles it, so behaviour is identical.
func (s *Server) tryIssueFastPath(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/v3/repos/") {
		return false
	}
	rest := p[len("/api/v3/repos/"):]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return false
	}
	owner := rest[:i]
	rest = rest[i+1:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return false
	}
	name := rest[:j]
	rest = rest[j+1:]
	if owner != viewerLogin {
		return false // generic router answers 404
	}
	if rest == "issues" {
		if r.Method != http.MethodPost {
			return false // GET list goes through the generic router
		}
		s.createIssue(w, r, owner, name)
		return true
	}
	if !strings.HasPrefix(rest, "issues/") {
		return false
	}
	numStr := rest[len("issues/"):]
	if numStr == "" || r.Method != http.MethodGet {
		return false
	}
	num := 0
	for k := 0; k < len(numStr); k++ {
		c := numStr[k]
		if c < '0' || c > '9' {
			return false // non-numeric: generic router answers 404
		}
		num = num*10 + int(c-'0')
		if num > 1<<30 {
			return false // overflow guard: generic router answers 404
		}
	}
	s.getIssue(w, r, owner, name, num)
	return true
}

// ---- git smart-HTTP routes ----

// tryGitRoute handles /{owner}/{repo}.git/{info/refs,git-upload-pack,git-receive-pack}.
// Returns false when the path is not a git route. Parsed with index scans
// (no Split/Join allocations) — every push and clone hits this twice.
func (s *Server) tryGitRoute(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if len(p) < 2 || p[0] != '/' {
		return false
	}
	rest := p[1:]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return false
	}
	owner := rest[:i]
	rest = rest[i+1:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return false
	}
	repoPart := rest[:j]
	service := rest[j+1:]
	if !strings.HasSuffix(repoPart, ".git") {
		return false
	}
	repoName := repoPart[:len(repoPart)-4]
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
	// Fast path: the only query the suite sends is a bare service= param.
	// Anything else falls back to full query parsing.
	var service string
	switch r.URL.RawQuery {
	case "service=git-upload-pack":
		service = "git-upload-pack"
	case "service=git-receive-pack":
		service = "git-receive-pack"
	default:
		service = r.URL.Query().Get("service")
	}
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
	// v2 if the header says so or the first line is a command= (peek at the
	// first pkt-line directly instead of parsing the whole body).
	isV2 := r.Header.Get("Git-Protocol") == "version=2"
	if !isV2 && len(body) >= 12 {
		if n, ok := parseHex4(body[:4]); ok && n >= 12 && len(body) >= n && bytes.HasPrefix(body[4:n], commandPrefix) {
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
		key := "ls:" + strings.Join(a.prefixes, ",") + "|" + boolByte(a.peel) + boolByte(a.symrefs) + boolByte(a.unborn)
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

var commandPrefix = []byte("command=")

func boolByte(b bool) string {
	if b {
		return "1"
	}
	return "0"
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
		if b[off] == '0' && b[off+1] == '0' && b[off+2] == '0' {
			switch b[off+3] {
			case '0':
				out = append(out, pkt{kind: "flush"})
				off += 4
				continue
			case '1':
				out = append(out, pkt{kind: "delim"})
				off += 4
				continue
			case '2':
				out = append(out, pkt{kind: "end"})
				off += 4
				continue
			}
		}
		n, ok := parseHex4(b[off : off+4])
		if !ok || n < 4 || off+n > len(b) {
			break
		}
		out = append(out, pkt{kind: "data", text: string(b[off+4 : off+n])})
		off += n
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
		var hdr [4]byte
		for _, line := range report {
			total := len(line) + 6 // 4 length + 1 band + line + \n
			hdr[0] = hexLower[(total>>12)&0xf]
			hdr[1] = hexLower[(total>>8)&0xf]
			hdr[2] = hexLower[(total>>4)&0xf]
			hdr[3] = hexLower[total&0xf]
			out.Write(hdr[:])
			out.WriteByte(1)
			out.WriteString(line)
			out.WriteByte('\n')
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
	// The live set is a few MiB of in-memory state; let the heap grow 3x
	// before a GC cycle instead of 2x — fewer collections on the hot paths
	// for a few MiB of headroom (the trial allows 1024 MiB).
	debug.SetGCPercent(200)
	// The trial grants the entry 2 CPUs, but Go 1.24's default GOMAXPROCS
	// is the host's CPU count (gVisor reports the whole node). Capping the
	// scheduler at the cgroup limit avoids thread thrash and GC-worker spam
	// on a throttled 2-CPU container.
	runtime.GOMAXPROCS(2)
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

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		// Standalone mode (local testing): serve until killed.
		fmt.Fprintf(os.Stderr, "meridian-forge: listening on port %d (standalone)\n", port)
		fmt.Printf("meridian-forge listening on http://127.0.0.1:%d\n", port)
		go rawServe(ln, srv)
		waitForSignal()
		_ = ln.Close()
		return
	}

	// Trial mode: register the port, serve, poll /done.
	if err := registerPort(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "meridian-forge: register:", err)
		os.Exit(1)
	}
	go rawServe(ln, srv)
	fmt.Fprintf(os.Stderr, "meridian-forge: registered port %d, polling done\n", port)
	pollDone(envURL)
	_ = ln.Close()
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
