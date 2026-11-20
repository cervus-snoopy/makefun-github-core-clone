package main

// Entry point: harness lifecycle (register / done polling), a raw
// TCP HTTP/1.1 server, and the top-level dispatch pipeline.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// request is one parsed HTTP request.
type request struct {
	method   string
	path     string // percent-decoded path
	rawPath  string // path exactly as received (for Link headers)
	queryStr string // raw query string
	headers  map[string]string
	host     string
	body     []byte
}

// header returns a request header by lower-cased name.
func (r *request) header(name string) string {
	return r.headers[strings.ToLower(name)]
}

// query returns the value of the first query pair whose decoded
// key matches; a bare key yields "".
func (r *request) query(key string) string {
	for _, pair := range strings.Split(r.queryStr, "&") {
		if pair == "" {
			continue
		}
		k, v := pair, ""
		if i := strings.IndexByte(pair, '='); i >= 0 {
			k, v = pair[:i], pair[i+1:]
		}
		if decodeQueryComponent(k) == key {
			return decodeQueryComponent(v)
		}
	}
	return ""
}

// response is one HTTP response to write.
type response struct {
	status int
	ctype  string
	body   []byte
	extra  map[string]string
}

var reasonPhrases = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
}

var reqCounter int64

// writeResponse serialises one response onto the connection.
func writeResponse(w *bufio.Writer, resp response) {
	n := atomic.AddInt64(&reqCounter, 1)
	reason, ok := reasonPhrases[resp.status]
	if !ok {
		reason = "Status"
	}
	ctype := resp.ctype
	if ctype == "" {
		ctype = "application/json; charset=utf-8"
	}
	body := resp.body
	if resp.status == 204 {
		body = nil
	}
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n", resp.status, reason)
	fmt.Fprintf(w, "Content-Type: %s\r\n", ctype)
	fmt.Fprintf(w, "Content-Length: %d\r\n", len(body))
	fmt.Fprintf(w, "X-GitHub-Request-Id: req-%s\r\n", strconv.FormatInt(n, 16))
	if len(resp.extra) > 0 {
		keys := make([]string, 0, len(resp.extra))
		for k := range resp.extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s: %s\r\n", k, resp.extra[k])
		}
	}
	w.WriteString("\r\n")
	w.Write(body)
	w.Flush()
}

// ---- URL / query decoding ----

func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// percentDecode decodes %XX sequences; an invalid % sequence is
// copied through literally.
func percentDecode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, ok1 := hexVal(s[i+1]); ok1 {
				if lo, ok2 := hexVal(s[i+2]); ok2 {
					b.WriteByte(byte(hi<<4 | lo))
					i += 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// decodeQueryComponent decodes a query key/value: '+' becomes a
// space, then percent-decoding.
func decodeQueryComponent(s string) string {
	return percentDecode(strings.ReplaceAll(s, "+", " "))
}

// ---- request reading ----

const maxBody = 64 << 20

// readRequest reads one request from the connection. A malformed
// request or read error returns an error and the connection is
// dropped silently.
func readRequest(br *bufio.Reader) (*request, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("bad request line")
	}
	req := &request{
		method:  parts[0],
		headers: map[string]string{},
	}
	target := parts[1]
	if i := strings.IndexByte(target, '?'); i >= 0 {
		req.rawPath = target[:i]
		req.queryStr = target[i+1:]
	} else {
		req.rawPath = target
	}
	req.path = percentDecode(req.rawPath)
	for {
		hline, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		if i := strings.IndexByte(hline, ':'); i >= 0 {
			name := strings.ToLower(strings.TrimSpace(hline[:i]))
			req.headers[name] = strings.TrimSpace(hline[i+1:])
		}
	}
	req.host = req.headers["host"]
	// Body.
	if cl := req.headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("bad content-length")
		}
		if n > maxBody {
			return nil, fmt.Errorf("body too large")
		}
		if n > 0 {
			req.body = make([]byte, n)
			if _, err := io.ReadFull(br, req.body); err != nil {
				return nil, err
			}
		}
	} else if strings.Contains(strings.ToLower(req.headers["transfer-encoding"]), "chunked") {
		var body []byte
		for {
			sizeLine, err := br.ReadString('\n')
			if err != nil {
				return nil, err
			}
			sizeLine = strings.TrimSpace(sizeLine)
			if i := strings.IndexByte(sizeLine, ';'); i >= 0 {
				sizeLine = sizeLine[:i]
			}
			sz, err := strconv.ParseInt(sizeLine, 16, 64)
			if err != nil || sz < 0 {
				return nil, fmt.Errorf("bad chunk size")
			}
			if sz == 0 {
				// Trailing headers until a blank line.
				for {
					tl, err := br.ReadString('\n')
					if err != nil {
						return nil, err
					}
					if strings.TrimRight(tl, "\r\n") == "" {
						break
					}
				}
				break
			}
			if int64(len(body))+sz > maxBody {
				return nil, fmt.Errorf("body too large")
			}
			chunk := make([]byte, sz)
			if _, err := io.ReadFull(br, chunk); err != nil {
				return nil, err
			}
			body = append(body, chunk...)
			// Trailing CRLF after the chunk.
			if _, err := br.ReadString('\n'); err != nil {
				return nil, err
			}
		}
		req.body = body
	}
	return req, nil
}

// ---- dispatch ----

var reqGuard sync.Mutex

// dispatch runs one request through the pipeline: healthz, auth,
// git, GraphQL, MCP, REST, 404.
func dispatch(req *request, store *Store, token string) response {
	if req.method == "GET" && req.path == "/healthz" {
		return jsonOK(`{"status":"ok"}`)
	}
	if token != "" {
		if req.header("authorization") != "Bearer "+token {
			return jsonErr(401, "Requires authentication")
		}
	}
	if handled, resp := routeGit(req, store); handled {
		return resp
	}
	if req.path == "/api/graphql" {
		if req.method != "POST" {
			return jsonErr(405, "Method Not Allowed")
		}
		return handleGraphQL(req, store)
	}
	if req.path == "/mcp" {
		return handleMCP(req, store)
	}
	if strings.HasPrefix(req.path, "/api/v3") {
		if handled, resp := routeREST(req, store); handled {
			return resp
		}
	}
	return jsonErr(404, "Not Found")
}

func serveConn(conn net.Conn, store *Store, token string) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 32<<10)
	bw := bufio.NewWriterSize(conn, 32<<10)
	for {
		req, err := readRequest(br)
		if err != nil {
			return
		}
		reqGuard.Lock()
		resp := dispatch(req, store, token)
		reqGuard.Unlock()
		writeResponse(bw, resp)
		if req.header("connection") == "close" {
			return
		}
	}
}

// ---- minimal harness HTTP client ----

// httpCall performs one raw HTTP/1.1 request against the harness
// base URL (path must start with "/") and returns status + body.
func httpCall(base, method, path string, body []byte) (int, []byte, error) {
	rest := strings.TrimPrefix(base, "http://")
	host := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host = rest[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var sb strings.Builder
	sb.WriteString(method + " " + path + " HTTP/1.1\r\n")
	sb.WriteString("Host: " + host + "\r\n")
	sb.WriteString("Connection: close\r\n")
	if method == "POST" {
		sb.WriteString("Content-Type: application/json\r\n")
		sb.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	} else {
		sb.WriteString("Content-Length: 0\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return 0, nil, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			return 0, nil, err
		}
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	fields := strings.SplitN(strings.TrimRight(statusLine, "\r\n"), " ", 3)
	if len(fields) < 2 {
		return 0, nil, fmt.Errorf("bad status line")
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, nil, err
	}
	contentLength := 0
	chunked := false
	for {
		hline, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		if i := strings.IndexByte(hline, ':'); i >= 0 {
			name := strings.ToLower(strings.TrimSpace(hline[:i]))
			val := strings.TrimSpace(hline[i+1:])
			if name == "content-length" {
				contentLength, _ = strconv.Atoi(val)
			}
			if name == "transfer-encoding" && strings.Contains(strings.ToLower(val), "chunked") {
				chunked = true
			}
		}
	}
	var respBody []byte
	if chunked {
		for {
			sizeLine, err := br.ReadString('\n')
			if err != nil {
				return 0, nil, err
			}
			sizeLine = strings.TrimSpace(sizeLine)
			if i := strings.IndexByte(sizeLine, ';'); i >= 0 {
				sizeLine = sizeLine[:i]
			}
			sz, err := strconv.ParseInt(sizeLine, 16, 64)
			if err != nil {
				return 0, nil, err
			}
			if sz == 0 {
				break
			}
			chunk := make([]byte, sz)
			if _, err := io.ReadFull(br, chunk); err != nil {
				return 0, nil, err
			}
			respBody = append(respBody, chunk...)
			br.ReadString('\n')
		}
	} else if contentLength > 0 {
		respBody = make([]byte, contentLength)
		if _, err := io.ReadFull(br, respBody); err != nil {
			return 0, nil, err
		}
	}
	return status, respBody, nil
}

// backoff returns the retry delay for an attempt: 250ms <<
// min(attempt, 3), capped at 2000ms.
func backoff(attempt int) time.Duration {
	shift := attempt
	if shift > 3 {
		shift = 3
	}
	d := 250 * time.Millisecond << uint(shift)
	if d > 2000*time.Millisecond {
		d = 2000 * time.Millisecond
	}
	return d
}

func main() {
	envURL := strings.TrimSuffix(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[forge] MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	store := newStore()

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[forge] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "[forge] listening on port %d\n", port)

	// Register, retrying forever except on a fatal 400/405.
	attempt := 0
	for {
		status, _, err := httpCall(envURL, "POST", "/register", []byte(`{"port":`+strconv.Itoa(port)+`}`))
		if err == nil && (status == 200 || status == 409) {
			break
		}
		if err == nil && (status == 400 || status == 405) {
			fmt.Fprintf(os.Stderr, "[forge] register fatal status %d\n", status)
			os.Exit(1)
		}
		time.Sleep(backoff(attempt))
		attempt++
	}
	fmt.Fprintln(os.Stderr, "[forge] registered")

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, store, token)
		}
	}()

	// Poll /done until the harness signals completion. The check
	// accepts the compact substring "done":true and, for
	// robustness, a parsed JSON body with done=true (some
	// encoders emit {"done": true} with a space).
	attempt = 0
	for {
		status, body, err := httpCall(envURL, "GET", "/done", nil)
		done := false
		if err == nil && status == 200 {
			if v, ok := parseJSON(string(body)); ok && v.BoolField("done", false) {
				done = true
			} else if strings.Contains(string(body), `"done":true`) {
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(backoff(attempt))
		attempt++
	}
	fmt.Fprintln(os.Stderr, "[forge] done, shutting down")
	ln.Close()
}
