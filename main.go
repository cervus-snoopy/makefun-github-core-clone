// Forge entrypoint: a standalone GitHub-compatible git forge in one Go process.
//
// Lifecycle (the harness's, not the contest's): bind an ephemeral port, POST it to
// $MAKEFUN_ENV_URL/register, serve until GET $MAKEFUN_ENV_URL/done answers done:true,
// then exit 0. The server is raw TCP HTTP/1.1 (no framework): one goroutine per
// keep-alive connection, pre-serialized hot responses, and a single in-memory store.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var reqCounter uint64

func nextReqID() string {
	n := atomic.AddUint64(&reqCounter, 1)
	var b [24]byte
	b[0] = 'r'
	b[1] = 'e'
	b[2] = 'q'
	b[3] = '-'
	s := strconv.FormatUint(n, 16)
	copy(b[4:], s)
	return string(b[:4+len(s)])
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[forge] MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	store = newStore(os.Getenv("MAKEFUN_SHOP_TOKEN"))

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[forge] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "[forge] listening on port %d\n", port)

	if err := register(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "[forge] register:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "[forge] registered; waiting for done")

	// Serve in the background while the main goroutine polls /done.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(c)
		}
	}()

	waitForDone(envURL)
	ln.Close()
	fmt.Fprintln(os.Stderr, "[forge] done")
	os.Exit(0)
}

// ---------------------------------------------------------------- lifecycle HTTP
// Minimal HTTP client over raw TCP (no net/http): only what register/done need.

func lifecycleReq(envURL, method, path, body string) (int, []byte, error) {
	rest := strings.TrimPrefix(envURL, "http://")
	host := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host = rest[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 30*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	var sb strings.Builder
	sb.WriteString(method)
	sb.WriteString(" ")
	sb.WriteString(path)
	sb.WriteString(" HTTP/1.1\r\nHost: ")
	sb.WriteString(host)
	sb.WriteString("\r\nConnection: close\r\n")
	if body != "" {
		sb.WriteString("Content-Type: application/json\r\nContent-Length: ")
		sb.WriteString(strconv.Itoa(len(body)))
		sb.WriteString("\r\n")
	} else {
		sb.WriteString("Content-Length: 0\r\n")
	}
	sb.WriteString("\r\n")
	sb.WriteString(body)
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return 0, nil, err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	parts := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("bad status line %q", line)
	}
	status, _ := strconv.Atoi(parts[1])
	clen := 0
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		if i := strings.IndexByte(h, ':'); i >= 0 {
			if strings.EqualFold(strings.TrimSpace(h[:i]), "Content-Length") {
				clen, _ = strconv.Atoi(strings.TrimSpace(h[i+1:]))
			}
		}
	}
	var out []byte
	if clen > 0 {
		out = make([]byte, clen)
		if _, err := readFull(br, out); err != nil {
			return 0, nil, err
		}
	}
	return status, out, nil
}

func readFull(r *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := r.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func sleepBackoff(attempt int) {
	ms := 250 << uint(min(attempt, 3))
	if ms > 2000 {
		ms = 2000
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

func register(envURL string, port int) error {
	body := `{"port":` + strconv.Itoa(port) + `}`
	for attempt := 0; ; attempt++ {
		status, _, err := lifecycleReq(envURL, "POST", "/register", body)
		if err == nil && (status == 200 || status == 409) {
			return nil
		}
		if err == nil && (status == 400 || status == 405) {
			return fmt.Errorf("register answered HTTP %d", status)
		}
		sleepBackoff(attempt)
	}
}

func waitForDone(envURL string) {
	for attempt := 0; ; attempt++ {
		status, body, err := lifecycleReq(envURL, "GET", "/done", "")
		if err == nil && status == 200 && bytes.Contains(body, []byte(`"done":true`)) {
			return
		}
		sleepBackoff(attempt)
	}
}

// ---------------------------------------------------------------- raw HTTP server

type httpRequest struct {
	method  string
	path    string // decoded, no query
	rawPath string
	query   string
	headers map[string]string
	body    []byte
	host    string
}

type httpResponse struct {
	status int
	ctype  string
	extra  []string // extra header lines, already formatted "Name: value"
	body   []byte
	noBody bool
}

func handleConn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 32768)
	bw := bufio.NewWriterSize(c, 32768)
	for {
		req, ok := readRequest(br)
		if !ok {
			return
		}
		resp := dispatch(req)
		if !writeResponse(bw, req, resp) {
			return
		}
		if req.headers["connection"] == "close" {
			return
		}
	}
}

func readRequest(br *bufio.Reader) (*httpRequest, bool) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, false
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, false
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 3 {
		return nil, false
	}
	req := &httpRequest{method: parts[0], headers: make(map[string]string, 16)}
	rawTarget := parts[1]
	if i := strings.IndexByte(rawTarget, '?'); i >= 0 {
		req.rawPath = rawTarget[:i]
		req.query = rawTarget[i+1:]
	} else {
		req.rawPath = rawTarget
	}
	req.path = urlDecodePath(req.rawPath)
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, false
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		if i := strings.IndexByte(h, ':'); i >= 0 {
			req.headers[strings.ToLower(strings.TrimSpace(h[:i]))] = strings.TrimSpace(h[i+1:])
		}
	}
	req.host = req.headers["host"]
	clen := 0
	if v, ok := req.headers["content-length"]; ok {
		clen, _ = strconv.Atoi(v)
	}
	if strings.EqualFold(req.headers["transfer-encoding"], "chunked") {
		body, ok := readChunked(br)
		if !ok {
			return nil, false
		}
		req.body = body
	} else if clen > 0 {
		if clen > 64*1024*1024 {
			return nil, false
		}
		body := make([]byte, clen)
		if _, err := readFull(br, body); err != nil {
			return nil, false
		}
		req.body = body
	}
	return req, true
}

func readChunked(br *bufio.Reader) ([]byte, bool) {
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, false
		}
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(line, 16, 64)
		if err != nil || n < 0 {
			return nil, false
		}
		if n == 0 {
			// trailing headers until blank line
			for {
				h, err := br.ReadString('\n')
				if err != nil || strings.TrimRight(h, "\r\n") == "" {
					break
				}
			}
			return out, true
		}
		chunk := make([]byte, n)
		if _, err := readFull(br, chunk); err != nil {
			return nil, false
		}
		out = append(out, chunk...)
		if _, err := br.ReadString('\n'); err != nil {
			return nil, false
		}
	}
}

func writeResponse(bw *bufio.Writer, req *httpRequest, resp *httpResponse) bool {
	var sb strings.Builder
	sb.WriteString("HTTP/1.1 ")
	sb.WriteString(strconv.Itoa(resp.status))
	sb.WriteString(" ")
	sb.WriteString(statusText(resp.status))
	sb.WriteString("\r\nContent-Type: ")
	sb.WriteString(resp.ctype)
	sb.WriteString("\r\nContent-Length: ")
	if resp.noBody {
		sb.WriteString("0")
	} else {
		sb.WriteString(strconv.Itoa(len(resp.body)))
	}
	sb.WriteString("\r\nX-GitHub-Request-Id: ")
	sb.WriteString(nextReqID())
	sb.WriteString("\r\n")
	for _, h := range resp.extra {
		sb.WriteString(h)
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := bw.WriteString(sb.String()); err != nil {
		return false
	}
	if !resp.noBody && len(resp.body) > 0 {
		if _, err := bw.Write(resp.body); err != nil {
			return false
		}
	}
	if err := bw.Flush(); err != nil {
		return false
	}
	return true
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	default:
		return "Status"
	}
}

func urlDecodePath(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	var sb strings.Builder
	sb.Grow(len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '%' && i+2 < len(p) {
			if v, ok := hexVal(p[i+1], p[i+2]); ok {
				sb.WriteByte(v)
				i += 2
				continue
			}
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

func hexVal(a, b byte) (byte, bool) {
	h := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	hi, ok1 := h(a)
	lo, ok2 := h(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return hi<<4 | lo, true
}

// queryValue returns the first value of key in a URL query string.
func queryValue(q, key string) string {
	for q != "" {
		var pair string
		if i := strings.IndexByte(q, '&'); i >= 0 {
			pair, q = q[:i], q[i+1:]
		} else {
			pair, q = q, ""
		}
		if i := strings.IndexByte(pair, '='); i >= 0 {
			if urlDecodeQuery(pair[:i]) == key {
				return urlDecodeQuery(pair[i+1:])
			}
		} else if urlDecodeQuery(pair) == key {
			return ""
		}
	}
	return ""
}

func urlDecodeQuery(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	s = strings.ReplaceAll(s, "+", " ")
	return urlDecodePath(s)
}
