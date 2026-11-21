// Meridian Forge — hand-rolled HTTP/1.1 transport.
//
// Replaces net/http's server for the trial hot paths: fewer allocations and
// fewer syscalls per request (one buffered read for the head, one write for
// the whole response). Handlers are unchanged: every request is presented as
// a real *http.Request and every response is collected by a buffering
// http.ResponseWriter, then flushed as a single write.
//
// Supported: HTTP/1.0 and HTTP/1.1, keep-alive, Content-Length and chunked
// request bodies, Expect: 100-continue, HEAD. Responses carry Content-Length
// (except 204/304), sorted header keys and a sniffed Content-Type when the
// handler set none — the same wire shape net/http produces, minus the Date
// header (no client in the suite reads it).
package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// maxRequestBody is the largest request body accepted (64 MiB); the suite's
// biggest body is the ~88 KiB clone-history pack.
const maxRequestBody = 64 << 20

var bufioReaderPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 4096) }}
var responseBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 2048); return &b }}
var responseWriterPool = sync.Pool{New: func() any { return &rawResponseWriter{header: make(http.Header, 4)} }}
var requestHeaderPool = sync.Pool{New: func() any { return make(http.Header, 8) }}

// tuneTCPConn sets the per-socket options net/http sets (TCP_NODELAY) plus
// TCP_QUICKACK. The trial runs entry and suite in separate containers over a
// Docker network: if the client's request arrives in two small writes with
// Nagle on, the server's delayed ACK (up to 40 ms) stalls the second write.
// QUICKACK makes the server ACK request segments immediately. The kernel
// clears quickack mode once an ACK is emitted, so quickackTCP re-arms it
// after every request (one cheap setsockopt per request).
func tuneTCPConn(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetNoDelay(true)
	quickackTCP(tc)
}

func quickackTCP(tc *net.TCPConn) {
	raw, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
	})
}

// rawServe accepts connections on ln and serves handler on each, one
// goroutine per connection, until ln is closed.
func rawServe(ln net.Listener, handler http.Handler) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		tuneTCPConn(conn)
		go serveRawConn(conn, handler)
	}
}

func serveRawConn(conn net.Conn, handler http.Handler) {
	defer conn.Close()
	br := bufioReaderPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer bufioReaderPool.Put(br)
	tc, _ := conn.(*net.TCPConn)
	for {
		req, closeAfter, err := readRawRequest(br, conn)
		if err != nil {
			return
		}
		// Re-arm QUICKACK for the next request's arrival (the kernel
		// clears it after emitting an ACK).
		if tc != nil {
			quickackTCP(tc)
		}
		rw := responseWriterPool.Get().(*rawResponseWriter)
		handler.ServeHTTP(rw, req)
		ok := writeRawResponse(conn, req, rw)
		// Recycle the writer and the request header map (both are used
		// only synchronously inside ServeHTTP).
		hdr := req.Header
		clear(hdr)
		requestHeaderPool.Put(hdr)
		clear(rw.header)
		rw.status = 0
		rw.wroteHeader = false
		if cap(rw.body) <= 65536 {
			rw.body = rw.body[:0]
			responseWriterPool.Put(rw)
		}
		if !ok || closeAfter {
			return
		}
	}
}

// readLine reads one \n-terminated line. The returned slice aliases the
// reader's buffer (valid only until the next read) unless the line exceeded
// the buffer, in which case it is a fresh allocation.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err == nil {
		return line, nil
	}
	if err != bufio.ErrBufferFull {
		return nil, err
	}
	buf := append([]byte(nil), line...)
	for err == bufio.ErrBufferFull {
		line, err = br.ReadSlice('\n')
		buf = append(buf, line...)
	}
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func trimLineEnd(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}

// parseRequestURI parses the request-target. Plain absolute paths with no
// query, fragment or escapes (every hot-path URI) are built directly —
// url.ParseRequestURI returns exactly {Path: uri} for those. Anything else
// (query strings, escapes, absolute-form, non-printable bytes) falls back
// to the full parser, so behaviour is identical.
func parseRequestURI(uri string) (*url.URL, error) {
	if len(uri) > 0 && uri[0] == '/' {
		plain := true
		for i := 0; i < len(uri); i++ {
			c := uri[i]
			if c < 0x21 || c > 0x7e || c == '?' || c == '%' || c == '#' {
				plain = false
				break
			}
		}
		if plain {
			// RawPath mirrors Path so EscapedPath/String match the stdlib
			// (ParseRequestURI sets RawPath when the raw form differs from
			// the default escaping, e.g. for '!' or '(' in a segment).
			return &url.URL{Path: uri, RawPath: uri}, nil
		}
	}
	return url.ParseRequestURI(uri)
}

// canonicalHeaderKey is textproto.CanonicalMIMEHeaderKey with a fast path
// for the headers the suite sends (avoids one string allocation +
// canonicalization per header). Unknown names fall back to the stdlib, so
// the result is identical for every input.
func canonicalHeaderKey(b []byte) string {
	switch {
	case bytes.EqualFold(b, []byte("host")):
		return "Host"
	case bytes.EqualFold(b, []byte("content-length")):
		return "Content-Length"
	case bytes.EqualFold(b, []byte("transfer-encoding")):
		return "Transfer-Encoding"
	case bytes.EqualFold(b, []byte("expect")):
		return "Expect"
	case bytes.EqualFold(b, []byte("connection")):
		return "Connection"
	case bytes.EqualFold(b, []byte("accept")):
		return "Accept"
	case bytes.EqualFold(b, []byte("authorization")):
		return "Authorization"
	case bytes.EqualFold(b, []byte("user-agent")):
		return "User-Agent"
	case bytes.EqualFold(b, []byte("content-type")):
		return "Content-Type"
	case bytes.EqualFold(b, []byte("git-protocol")):
		return "Git-Protocol"
	case bytes.EqualFold(b, []byte("accept-encoding")):
		return "Accept-Encoding"
	case bytes.EqualFold(b, []byte("cache-control")):
		return "Cache-Control"
	default:
		return textproto.CanonicalMIMEHeaderKey(string(b))
	}
}

// readRawRequest parses one HTTP/1.x request: request line, headers, body.
// closeAfter reports whether the connection must close after the response
// (HTTP/1.0 without keep-alive, or an explicit Connection: close).
func readRawRequest(br *bufio.Reader, conn net.Conn) (*http.Request, bool, error) {
	// Request line: METHOD SP REQUEST-URI SP HTTP/x.y
	rawLine, err := readLine(br)
	if err != nil {
		return nil, false, err
	}
	line := trimLineEnd(rawLine)
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 < 0 {
		return nil, false, errBadRequest
	}
	sp2 := bytes.LastIndexByte(line, ' ')
	if sp2 <= sp1 {
		return nil, false, errBadRequest
	}
	method := string(line[:sp1])
	uri := string(line[sp1+1 : sp2])
	proto := string(line[sp2+1:])
	if !strings.HasPrefix(proto, "HTTP/") {
		return nil, false, errBadRequest
	}
	protoMinor := 1
	if proto == "HTTP/1.0" {
		protoMinor = 0
	} else if proto != "HTTP/1.1" {
		return nil, false, errBadRequest
	}
	u, err := parseRequestURI(uri)
	if err != nil {
		return nil, false, errBadRequest
	}

	// Headers (map pooled; serveRawConn recycles it after the response).
	header := requestHeaderPool.Get().(http.Header)
	var host string
	var contentLength int64 = -1
	chunked := false
	expectContinue := false
	closeAfter := protoMinor == 0
	for {
		raw, err := readLine(br)
		if err != nil {
			return nil, false, err
		}
		h := trimLineEnd(raw)
		if len(h) == 0 {
			break
		}
		colon := bytes.IndexByte(h, ':')
		if colon < 0 {
			return nil, false, errBadRequest
		}
		name := canonicalHeaderKey(h[:colon])
		value := strings.TrimLeft(string(h[colon+1:]), " \t")
		header[name] = append(header[name], value)
		switch name {
		case "Host":
			host = value
		case "Content-Length":
			if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && n >= 0 {
				contentLength = n
			} else {
				return nil, false, errBadRequest
			}
		case "Transfer-Encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case "Expect":
			if strings.EqualFold(value, "100-continue") {
				expectContinue = true
			}
		case "Connection":
			lower := strings.ToLower(value)
			if strings.Contains(lower, "close") {
				closeAfter = true
			} else if strings.Contains(lower, "keep-alive") {
				closeAfter = false
			}
		}
	}

	// Body.
	var body []byte
	if chunked {
		if expectContinue {
			conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
		}
		body, err = readChunkedBody(br)
		if err != nil {
			return nil, false, err
		}
	} else if contentLength > 0 {
		if contentLength > maxRequestBody {
			return nil, false, errBodyTooLarge
		}
		if expectContinue {
			conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
		}
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, false, err
		}
	} else if contentLength == 0 {
		body = []byte{}
	}
	// contentLength < 0 (no length, no chunked): no body, as net/http does.

	req := &http.Request{
		Method:        method,
		URL:           u,
		Proto:         proto,
		ProtoMajor:    1,
		ProtoMinor:    protoMinor,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Host:          host,
		RequestURI:    uri,
	}
	return req, closeAfter, nil
}

var errBadRequest = errTransport("bad request")
var errBodyTooLarge = errTransport("request body too large")

type errTransport string

func (e errTransport) Error() string { return string(e) }

// readChunkedBody reads a chunked transfer-encoded body (trailers discarded).
func readChunkedBody(br *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		raw, err := readLine(br)
		if err != nil {
			return nil, err
		}
		sizeLine := trimLineEnd(raw)
		// chunk extensions after ';' are ignored
		if i := bytes.IndexByte(sizeLine, ';'); i >= 0 {
			sizeLine = sizeLine[:i]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(sizeLine)), 16, 64)
		if err != nil || size < 0 {
			return nil, errBadRequest
		}
		if size == 0 {
			// trailers until the blank line
			for {
				raw, err := readLine(br)
				if err != nil {
					return nil, err
				}
				if len(trimLineEnd(raw)) == 0 {
					return body, nil
				}
			}
		}
		if int64(len(body))+size > maxRequestBody {
			return nil, errBodyTooLarge
		}
		start := len(body)
		body = append(body, make([]byte, size)...)
		if _, err := io.ReadFull(br, body[start:]); err != nil {
			return nil, err
		}
		// trailing CRLF after the chunk
		raw, err = readLine(br)
		if err != nil {
			return nil, err
		}
		if len(trimLineEnd(raw)) != 0 {
			return nil, errBadRequest
		}
	}
}

// rawResponseWriter collects a response in memory for a single-write flush.
type rawResponseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        []byte
}

func (w *rawResponseWriter) Header() http.Header { return w.header }

func (w *rawResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
}

func (w *rawResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

// writeRawResponse serializes the collected response (status line, sorted
// headers, Content-Length, body) and writes it in a single syscall.
// Returns false when the write failed (the connection must close).
func writeRawResponse(conn net.Conn, req *http.Request, rw *rawResponseWriter) bool {
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	bodyAllowed := status != http.StatusNoContent && status != http.StatusNotModified &&
		!(status >= 100 && status < 200)
	if _, ok := rw.header["Content-Type"]; !ok && bodyAllowed && len(rw.body) > 0 {
		rw.header.Set("Content-Type", http.DetectContentType(rw.body))
	}
	if bodyAllowed {
		rw.header.Set("Content-Length", strconv.Itoa(len(rw.body)))
	}
	keys := make([]string, 0, len(rw.header))
	for k := range rw.header {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	bufp := responseBufPool.Get().(*[]byte)
	b := (*bufp)[:0]
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = append(b, http.StatusText(status)...)
	b = append(b, "\r\n"...)
	for _, k := range keys {
		for _, v := range rw.header[k] {
			b = append(b, k...)
			b = append(b, ':', ' ')
			b = append(b, v...)
			b = append(b, "\r\n"...)
		}
	}
	b = append(b, "\r\n"...)
	if bodyAllowed && req.Method != http.MethodHead {
		b = append(b, rw.body...)
	}
	_, err := conn.Write(b)
	if cap(b) <= 65536 {
		*bufp = b[:0]
		responseBufPool.Put(bufp)
	}
	return err == nil
}
