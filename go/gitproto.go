// Meridian Forge — git smart HTTP: protocol v2 + v0, upload-pack and
// receive-pack. pkt-line framing, advertisements, ls-refs, fetch (with
// shallow/deepen), and push with fast-forward checks.
//
// All responses are built from the in-memory store; the fetch response,
// ls-refs response and advertisements are cached per repository generation
// (the clone benchmark issues 50 identical concurrent clones).
package main

import (
	"bytes"
	"sort"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

// ---- pkt-line framing ----

const hexLower = "0123456789abcdef"

// pktLenPrefix writes a 4-hex-digit pkt-line length prefix for n into b.
func pktLenPrefix(b []byte, n int) []byte {
	return append(b, hexLower[(n>>12)&0xf], hexLower[(n>>8)&0xf], hexLower[(n>>4)&0xf], hexLower[n&0xf])
}

func pktLine(s string) []byte {
	n := len(s) + 4
	out := make([]byte, n)
	out[0] = hexLower[(n>>12)&0xf]
	out[1] = hexLower[(n>>8)&0xf]
	out[2] = hexLower[(n>>4)&0xf]
	out[3] = hexLower[n&0xf]
	copy(out[4:], s)
	return out
}

func pktFlush() []byte { return []byte("0000") }

// parseHex4 parses 4 lowercase/uppercase hex digits (a pkt-line length).
func parseHex4(b []byte) (int, bool) {
	n := 0
	for i := 0; i < 4; i++ {
		c := b[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return 0, false
		}
		n = n*16 + v
	}
	return n, true
}

// pkt is one parsed pkt-line: kind is "data", "flush", "delim" or "end".
type pkt struct {
	kind string
	text string
}

// parsePktLines parses a pkt-line stream. Returns the packets and the offset
// just past the first flush packet (or len(b) when there is none) — the
// receive-pack pack bytes start there.
func parsePktLines(b []byte) ([]pkt, int) {
	var out []pkt
	off := 0
	for off+4 <= len(b) {
		if b[off] == '0' && b[off+1] == '0' && b[off+2] == '0' {
			switch b[off+3] {
			case '0':
				out = append(out, pkt{kind: "flush"})
				off += 4
				return out, off
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
			return out, len(b) // malformed: stop, caller decides
		}
		out = append(out, pkt{kind: "data", text: string(b[off+4 : off+n])})
		off += n
	}
	return out, off
}

// sidebandPackets wraps pack bytes into band-1 sideband pkt-lines.
func sidebandPackets(pack []byte) []byte {
	var out bytes.Buffer
	const maxData = 65515 // 65516 payload - 1 band byte
	var hdr [4]byte
	for len(pack) > 0 {
		n := len(pack)
		if n > maxData {
			n = maxData
		}
		chunk := pack[:n]
		pack = pack[n:]
		// length prefix covers the 4 length bytes + band byte + chunk
		total := len(chunk) + 5
		hdr[0] = hexLower[(total>>12)&0xf]
		hdr[1] = hexLower[(total>>8)&0xf]
		hdr[2] = hexLower[(total>>4)&0xf]
		hdr[3] = hexLower[total&0xf]
		out.Write(hdr[:])
		out.WriteByte(1)
		out.Write(chunk)
	}
	return out.Bytes()
}

// ---- advertisements ----

const agentCap = "agent=meridian-forge/1.0"
const symrefCap = "symref=HEAD:refs/heads/main"

var advV2Bytes = func() []byte {
	var b bytes.Buffer
	b.Write(pktLine("version 2\n"))
	b.Write(pktLine(agentCap + "\n"))
	b.Write(pktLine("ls-refs=unborn\n"))
	b.Write(pktLine("fetch=shallow\n"))
	b.Write(pktLine("object-format=sha1\n"))
	b.Write(pktFlush())
	return b.Bytes()
}()

// advertisementV0 builds the v0 advertisement for upload-pack or receive-pack.
func advertisementV0(r *Repo, service string) []byte {
	var caps string
	if service == "git-receive-pack" {
		caps = "report-status delete-refs side-band-64k ofs-delta " + agentCap + " " + symrefCap
	} else {
		caps = "multi_ack thin-pack side-band side-band-64k ofs-delta shallow " + symrefCap + " " + agentCap
	}
	var b bytes.Buffer
	b.Write(pktLine("# service=" + service + "\n"))
	b.Write(pktFlush())
	names := r.sortedRefNames()
	if len(names) == 0 {
		b.Write(pktLine(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		head := r.headOID()
		if head == "" {
			head = zeroOID
		}
		b.Write(pktLine(head + " HEAD\x00" + caps + "\n"))
		for _, name := range names {
			oid := r.refs[name]
			b.Write(pktLine(oid + " " + name + "\n"))
			// peeled line for annotated tags
			if strings.HasPrefix(name, "refs/tags/") {
				if obj := r.objects[oid]; obj != nil && obj.Type == objTag {
					if peeled := r.peelToCommit(oid); peeled != "" {
						b.Write(pktLine(peeled + " " + name + "^{}\n"))
					}
				}
			}
		}
	}
	b.Write(pktFlush())
	return b.Bytes()
}

// ---- ls-refs (v2) ----

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func (r *Repo) lsRefsResponse(a lsRefsArgs) []byte {
	var b bytes.Buffer
	matches := func(name string) bool {
		if len(a.prefixes) == 0 {
			return true
		}
		for _, p := range a.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	// HEAD
	if matches("HEAD") {
		head := r.headOID()
		if head != "" {
			line := head + " HEAD"
			if a.symrefs {
				line += " symref-target:refs/heads/" + r.DefaultBranch
			}
			b.Write(pktLine(line + "\n"))
		} else if a.unborn {
			line := "unborn HEAD"
			if a.symrefs {
				line += " symref-target:refs/heads/" + r.DefaultBranch
			}
			b.Write(pktLine(line + "\n"))
		}
	}
	for _, name := range r.sortedRefNames() {
		if !matches(name) {
			continue
		}
		oid := r.refs[name]
		line := oid + " " + name
		if a.peel {
			if obj := r.objects[oid]; obj != nil && obj.Type == objTag {
				if peeled := r.peelToCommit(oid); peeled != "" {
					line += " peeled:" + peeled
				}
			}
		}
		b.Write(pktLine(line + "\n"))
	}
	b.Write(pktFlush())
	return b.Bytes()
}

// ---- fetch object selection ----

type fetchRequest struct {
	wants       []string
	haves       []string
	shallow     []string
	depth       int  // -1 when unset
	deepenSince int64
	deepenNot   []string
}

// fetchSelection computes the objects to send, plus shallow/unshallow lines.
// depth N means the tip plus N-1 generations of ancestors. The have-closure
// stops at the client's shallow boundaries.
func (r *Repo) fetchSelection(req fetchRequest) (send map[string]*Object, shallowLines, unshallowLines []string) {
	clientShallow := map[string]bool{}
	for _, s := range req.shallow {
		clientShallow[s] = true
	}
	// have-closure: reachable from haves, never traversing past a shallow
	// boundary commit's parents.
	haveSet := map[string]bool{}
	{
		stack := append([]string{}, req.haves...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if haveSet[oid] {
				continue
			}
			obj := r.objects[oid]
			if obj == nil {
				continue
			}
			haveSet[oid] = true
			switch obj.Type {
			case objCommit:
				tree, parents, _ := parseCommit(obj.Data)
				if tree != "" {
					stack = append(stack, tree)
				}
				if !clientShallow[oid] {
					stack = append(stack, parents...)
				}
			case objTree:
				for _, e := range parseTree(obj.Data) {
					stack = append(stack, e.OID)
				}
			case objTag:
				target, _ := parseTagTarget(obj.Data)
				if target != "" {
					stack = append(stack, target)
				}
			}
		}
	}
	// want-closure: commits within depth of the wants (all when depth unset),
	// excluding anything the client already holds. The BFS traverses THROUGH
	// held commits — a shallow boundary's parents are exactly what a deepen
	// must reach — but only non-held commits join the traversal set. BFS
	// (FIFO) visits each commit at its minimum depth first, so the depth
	// limit is never applied from a longer path.
	traversal := map[string]bool{} // commits to include
	{
		type item struct {
			oid   string
			depth int
		}
		queue := []item{}
		for _, w := range req.wants {
			queue = append(queue, item{w, 0})
		}
		visited := map[string]bool{}
		for head := 0; head < len(queue); head++ {
			it := queue[head]
			if visited[it.oid] {
				continue
			}
			visited[it.oid] = true
			obj := r.objects[it.oid]
			if obj == nil {
				continue
			}
			if obj.Type == objTag {
				// a want pointing at a tag: include the tag and traverse its target
				if !haveSet[it.oid] {
					traversal[it.oid] = true
				}
				target, _ := parseTagTarget(obj.Data)
				if target != "" {
					queue = append(queue, item{target, it.depth})
				}
				continue
			}
			if obj.Type != objCommit {
				if !haveSet[it.oid] {
					traversal[it.oid] = true
				}
				continue
			}
			if !haveSet[it.oid] {
				traversal[it.oid] = true
			}
			if req.depth >= 0 && it.depth+1 >= req.depth {
				continue // depth limit reached
			}
			_, parents, _ := parseCommit(obj.Data)
			for _, p := range parents {
				queue = append(queue, item{p, it.depth + 1})
			}
		}
	}
	// Expand the traversal commits to their full closure (trees + blobs),
	// minus what the client holds.
	send = map[string]*Object{}
	{
		stack := []string{}
		for oid := range traversal {
			stack = append(stack, oid)
		}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if haveSet[oid] {
				continue
			}
			obj := r.objects[oid]
			if obj == nil {
				continue
			}
			if _, ok := send[oid]; ok {
				continue
			}
			send[oid] = obj
			switch obj.Type {
			case objCommit:
				tree, parents, _ := parseCommit(obj.Data)
				if tree != "" {
					stack = append(stack, tree)
				}
				// parents are included only when they are in the traversal set
				for _, p := range parents {
					if traversal[p] {
						stack = append(stack, p)
					}
				}
			case objTree:
				for _, e := range parseTree(obj.Data) {
					stack = append(stack, e.OID)
				}
			case objTag:
				target, _ := parseTagTarget(obj.Data)
				if target != "" {
					stack = append(stack, target)
				}
			}
		}
	}
	// shallow lines: only when depth-limited. A sent commit is a boundary
	// when it has a parent outside the traversal set.
	if req.depth >= 0 {
		for oid := range traversal {
			obj := r.objects[oid]
			if obj == nil || obj.Type != objCommit {
				continue
			}
			_, parents, _ := parseCommit(obj.Data)
			if len(parents) == 0 {
				continue // root commits are complete, not shallow
			}
			boundary := false
			for _, p := range parents {
				if !traversal[p] {
					boundary = true
					break
				}
			}
			if boundary {
				shallowLines = append(shallowLines, oid)
			}
		}
		sort.Strings(shallowLines)
	}
	// unshallow lines: old client boundaries whose parent is now sent.
	for _, b := range req.shallow {
		obj := r.objects[b]
		if obj == nil || obj.Type != objCommit {
			continue
		}
		_, parents, _ := parseCommit(obj.Data)
		for _, p := range parents {
			if traversal[p] {
				unshallowLines = append(unshallowLines, b)
				break
			}
		}
	}
	sort.Strings(unshallowLines)
	return send, shallowLines, unshallowLines
}

// fetchResponseV2 builds the full v2 fetch response body.
func (r *Repo) fetchResponseV2(req fetchRequest) []byte {
	send, shallowLines, unshallowLines := r.fetchSelection(req)
	var b bytes.Buffer
	// acknowledgments
	b.Write(pktLine("acknowledgments\n"))
	if len(req.haves) == 0 {
		b.Write(pktLine("NAK\n"))
	} else {
		// ACK the haves that are ancestors of (or equal to) a want.
		acked := false
		wantClosure := r.reachableObjects(req.wants)
		for _, h := range req.haves {
			if wantClosure[h] {
				b.Write(pktLine("ACK " + h + "\n"))
				acked = true
			}
		}
		if acked {
			b.Write(pktLine("ready\n"))
		} else {
			b.Write(pktLine("NAK\n"))
		}
	}
	b.Write(pktFlush())
	// shallow-info (only when there is something to say)
	if len(shallowLines) > 0 || len(unshallowLines) > 0 {
		b.Write(pktLine("shallow-info\n"))
		for _, s := range shallowLines {
			b.Write(pktLine("shallow " + s + "\n"))
		}
		for _, s := range unshallowLines {
			b.Write(pktLine("unshallow " + s + "\n"))
		}
		b.Write(pktFlush())
	}
	// packfile
	b.Write(pktLine("packfile\n"))
	pack := writePack(send)
	b.Write(sidebandPackets(pack))
	b.Write(pktFlush())
	return b.Bytes()
}

// fetchResponseV0 builds the v0 fetch response body.
func (r *Repo) fetchResponseV0(req fetchRequest) []byte {
	send, shallowLines, unshallowLines := r.fetchSelection(req)
	var b bytes.Buffer
	for _, s := range shallowLines {
		b.Write(pktLine("shallow " + s + "\n"))
	}
	for _, s := range unshallowLines {
		b.Write(pktLine("unshallow " + s + "\n"))
	}
	if len(req.haves) == 0 {
		b.Write(pktLine("NAK\n"))
	} else {
		acked := false
		wantClosure := r.reachableObjects(req.wants)
		for _, h := range req.haves {
			if wantClosure[h] {
				b.Write(pktLine("ACK " + h + "\n"))
				acked = true
			}
		}
		if !acked {
			b.Write(pktLine("NAK\n"))
		}
	}
	pack := writePack(send)
	b.Write(sidebandPackets(pack))
	b.Write(pktFlush())
	return b.Bytes()
}

// ---- receive-pack ----

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// receivePack applies a push: stores the pack's objects, then evaluates each
// ref update. Returns the report lines (without pkt framing) and whether the
// unpack succeeded.
func (r *Repo) receivePack(cmds []pushCommand, pack []byte, resolveMissing func(string) *Object) (report []string, unpackOK bool, unpackErr string) {
	if len(pack) > 0 {
		objects, err := readPack(pack, func(oid string) *Object { return r.objects[oid] })
		if err != nil {
			report = append(report, "unpack "+err.Error())
			for _, c := range cmds {
				report = append(report, "ng "+c.ref+" unpack error")
			}
			return report, false, err.Error()
		}
		// readPack already hashed every object; store by its oid directly.
		for oid, obj := range objects {
			r.putObjectWithOID(oid, obj.Type, obj.Data)
		}
	}
	report = append(report, "unpack ok")
	changed := false
	for _, c := range cmds {
		ok, reason := r.applyRefUpdate(c)
		if ok {
			report = append(report, "ok "+c.ref)
			changed = true
		} else {
			report = append(report, "ng "+c.ref+" "+reason)
		}
	}
	if changed {
		r.gen++
	}
	return report, true, ""
}

// applyRefUpdate evaluates one ref update against the fast-forward rule.
func (r *Repo) applyRefUpdate(c pushCommand) (bool, string) {
	current, exists := r.refs[c.ref]
	isTag := strings.HasPrefix(c.ref, "refs/tags/")
	if c.newOID == zeroOID {
		// delete
		if !exists {
			return false, "remote ref does not exist"
		}
		if c.oldOID != zeroOID && c.oldOID != current {
			return false, "non-fast-forward"
		}
		delete(r.refs, c.ref)
		return true, ""
	}
	// create or update: the new object must exist
	if r.objects[c.newOID] == nil {
		return false, "missing object"
	}
	if !exists {
		if c.oldOID != zeroOID {
			return false, "non-fast-forward"
		}
		r.refs[c.ref] = c.newOID
		return true, ""
	}
	// ref exists
	if c.oldOID != zeroOID && c.oldOID != current {
		return false, "non-fast-forward"
	}
	if isTag {
		// tags may not be moved
		return false, "non-fast-forward"
	}
	if c.newOID == current {
		return true, ""
	}
	if !r.isAncestor(current, c.newOID) {
		return false, "non-fast-forward"
	}
	r.refs[c.ref] = c.newOID
	return true, ""
}

// ---- request parsing helpers ----

// parseV2Request splits a v2 POST body into the command and argument lines
// (everything after the delim packet, or all data lines when no delim).
func parseV2Request(body []byte) (command string, args []string, ok bool) {
	pkts, _ := parsePktLines(body)
	afterDelim := false
	for _, p := range pkts {
		switch p.kind {
		case "delim":
			afterDelim = true
		case "data":
			line := strings.TrimSuffix(p.text, "\n")
			if strings.HasPrefix(line, "command=") {
				command = strings.TrimPrefix(line, "command=")
			} else if afterDelim {
				args = append(args, line)
			}
		}
	}
	return command, args, command != ""
}
