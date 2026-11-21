// The forge binary: an HTTP server carrying the whole surface (REST v3, GraphQL, MCP and git
// smart HTTP) over the one store in store.go. It binds an ephemeral port, prints that port on
// its first stdout line (the lifecycle shell in main.ts reports it to the harness), and serves
// until stopped. Every JSON reply carries an X-GitHub-Request-Id; every error body is a JSON
// object with a string `message`; the only credential is the trial's bearer token.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"
const maxBodyBytes = 64 * 1024 * 1024

var requestCounter atomic.Int64

func nextRequestID() string {
	return "req-" + strconv.FormatInt(requestCounter.Add(1), 16)
}

// Marshal a JSON value the way JSON.stringify would: no HTML escaping, no trailing newline.
func marshalJSON(value any) []byte {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return []byte("null")
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// Parse a JSON document; numbers keep their literal form (json.Number).
func parseJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func isRecord(value any) bool {
	_, ok := value.(map[string]any)
	return ok
}

func asRecord(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	return nil
}

func strField(body map[string]any, key string) *string {
	if value, ok := body[key].(string); ok {
		return &value
	}
	return nil
}

// Decode one path segment; a malformed escape is kept verbatim rather than thrown.
func decodeSegment(segment string) string {
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return segment
	}
	return decoded
}

type server struct {
	forge *Forge
	token string
	// Requests are handled one at a time: the store is plain maps, and serialising keeps every
	// run of the same request sequence byte-identical.
	mu sync.Mutex
}

func (s *server) authorized(r *http.Request) bool {
	return s.token == "" || r.Header.Get("Authorization") == "Bearer "+s.token
}

// Answer with a JSON body and the headers every reply carries.
func sendJson(w http.ResponseWriter, status int, body any, headers map[string]string) {
	sendJsonBytes(w, status, marshalJSON(body), headers)
}

// Answer with pre-marshalled JSON bytes and the headers every reply carries.
func sendJsonBytes(w http.ResponseWriter, status int, text []byte, headers map[string]string) {
	header := w.Header()
	header.Set("content-type", jsonType)
	header.Set("content-length", strconv.Itoa(len(text)))
	header.Set("x-github-request-id", nextRequestID())
	for key, value := range headers {
		header.Set(key, value)
	}
	w.WriteHeader(status)
	w.Write(text)
}

// Answer an error: GitHub's `{message}` shape.
func sendError(w http.ResponseWriter, status int, message string) {
	sendJson(w, status, JObj{"message": message}, nil)
}

// Answer 204 with no body at all.
func sendNoContent(w http.ResponseWriter) {
	w.Header().Set("x-github-request-id", nextRequestID())
	w.WriteHeader(204)
}

// Read a whole request body (binary-safe); nil when it exceeds the cap.
func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return []byte{}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return []byte{}
	}
	if len(raw) > maxBodyBytes {
		return nil
	}
	return raw
}

// A ResponseWriter that remembers whether a status was already written (a failure after that
// point can only close the response, never turn it into a 500).
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(status int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackingWriter) Write(data []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(data)
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	tracked := &trackingWriter{ResponseWriter: w}
	w = tracked
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(os.Stderr, "[forge] request failed: %v\n", recovered)
			if !tracked.wrote {
				sendError(w, 500, "Internal Server Error")
			}
		}
	}()
	path := r.URL.EscapedPath()
	method := r.Method

	if method == "GET" && path == "/healthz" {
		sendJson(w, 200, JObj{"ok": true}, nil)
		return
	}

	raw := readBody(r)
	if raw == nil {
		sendError(w, 413, "Request body too large")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if path == "/api/graphql" {
		if method != "POST" {
			sendError(w, 404, "Not Found")
			return
		}
		if !s.authorized(r) {
			sendError(w, 401, "Bad credentials")
			return
		}
		result := handleGraphQL(s.forge, string(raw))
		sendJson(w, result.status, result.body, nil)
		return
	}

	if path == "/mcp" {
		if method != "POST" {
			sendError(w, 404, "Not Found")
			return
		}
		if !s.authorized(r) {
			sendError(w, 401, "Unauthorized")
			return
		}
		result := handleMcp(s.forge, string(raw))
		header := w.Header()
		header.Set("content-type", jsonType)
		header.Set("content-length", strconv.Itoa(len(result.text)))
		header.Set("x-github-request-id", nextRequestID())
		w.WriteHeader(result.status)
		io.WriteString(w, result.text)
		return
	}

	if owner, repo, service, ok := matchGitRoute(path); ok {
		if !s.authorized(r) {
			sendError(w, 401, "Bad credentials")
			return
		}
		result := handleGitRequest(s.forge, GitRequest{
			Owner:       decodeSegment(owner),
			Repo:        decodeSegment(repo),
			Service:     service,
			Method:      method,
			Query:       r.URL.Query(),
			GitProtocol: r.Header.Get("Git-Protocol"),
			Raw:         raw,
		})
		header := w.Header()
		header.Set("content-type", result.ContentType)
		header.Set("content-length", strconv.Itoa(len(result.Body)))
		header.Set("x-github-request-id", nextRequestID())
		for key, value := range result.Headers {
			header.Set(key, value)
		}
		w.WriteHeader(result.Status)
		w.Write(result.Body)
		return
	}

	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		if !s.authorized(r) {
			sendError(w, 401, "Bad credentials")
			return
		}
		ctx := &restCtx{
			forge:    s.forge,
			w:        w,
			r:        r,
			query:    r.URL.Query(),
			rawQuery: r.URL.RawQuery,
		}
		segments := []string{}
		for _, segment := range strings.Split(path[len("/api/v3"):], "/") {
			if segment != "" {
				segments = append(segments, decodeSegment(segment))
			}
		}
		routeRest(ctx, segments, raw)
		return
	}

	sendError(w, 404, "Not Found")
}

// The git route shape: /<owner>/<repo>.git/(info/refs|git-upload-pack|git-receive-pack), matched
// against the escaped path with the segments still encoded.
func matchGitRoute(path string) (owner, repo, service string, ok bool) {
	if !strings.HasPrefix(path, "/") {
		return "", "", "", false
	}
	parts := strings.Split(path[1:], "/")
	if len(parts) < 3 || parts[0] == "" || len(parts[1]) <= len(".git") || !strings.HasSuffix(parts[1], ".git") {
		return "", "", "", false
	}
	tail := strings.Join(parts[2:], "/")
	if tail != "info/refs" && tail != "git-upload-pack" && tail != "git-receive-pack" {
		return "", "", "", false
	}
	return parts[0], parts[1][:len(parts[1])-len(".git")], tail, true
}

func main() {
	// The store only grows and every request allocates; a lazier GC trades a little
	// memory for measurably less collector work on the request path.
	debug.SetGCPercent(300)
	forge := NewForge()
	srv := &server{forge: forge, token: os.Getenv("MAKEFUN_SHOP_TOKEN")}
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: listen: %v\n", err)
		os.Exit(1)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintf(os.Stderr, "forge: no bound address after listen\n")
		os.Exit(1)
	}
	// The port line is the handshake with the lifecycle shell; nothing else goes to stdout.
	fmt.Printf("%d\n", address.Port)
	httpServer := &http.Server{Handler: http.HandlerFunc(srv.handle)}
	if err := httpServer.Serve(listener); err != nil {
		fmt.Fprintf(os.Stderr, "forge: serve: %v\n", err)
		os.Exit(1)
	}
}
