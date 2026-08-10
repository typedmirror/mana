// Package serve is the REPL's session over the wire (D-049).
//
// A session is one persistent evaluator: flat scripts run on it, so bindings
// persist between submissions — the session is the context window (axiom 7).
// A script with acts runs as a self-contained job inside the session, and its
// results come back in the report, which is the carry channel.
//
// The HTTP layer is not the error channel (D-050): a runtime failure returns
// 200 with `ok:false`, exactly as the CLI returns exit 1 with a report.
// Status codes are reserved for the transport itself.
package serve

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/typedmirror/mana/internal/act"
	"github.com/typedmirror/mana/internal/ast"
	"github.com/typedmirror/mana/internal/evaluator"
	"github.com/typedmirror/mana/internal/host"
	"github.com/typedmirror/mana/internal/object"
	"github.com/typedmirror/mana/internal/parser"
)

// MaxSessions bounds the session table (D-051). At the cap, creation fails
// with the cap named rather than evicting someone else's context.
const MaxSessions = 64

// MaxScriptBytes bounds one submitted script.
const MaxScriptBytes = 1 << 20

// Options tune the server.
type Options struct {
	// Token, when non-empty, must arrive as `Authorization: Bearer <token>`
	// on every request. Empty means loopback trust (D-051).
	Token string
	// Timeout bounds each shell command in a run. Zero uses the host default.
	Timeout time.Duration
	// Retries is extra attempts for a failed act in act jobs. Flat session
	// runs never retry: a fresh attempt would need a fresh evaluator, and the
	// session's leftovers are the point of having a session.
	Retries int
}

// Server holds the sessions.
type Server struct {
	newHost func() host.Host
	opts    Options

	mu       sync.Mutex
	sessions map[string]*session
}

// session is one context window: an evaluator that survives across runs, fed
// through a host whose output target is swapped per run. Runs serialize on
// the session mutex — two artifacts interleaving on one binding table would
// make the window mean nothing (D-051).
type session struct {
	mu   sync.Mutex
	eval *evaluator.Evaluator
	out  *swappable
	base host.Host
	runs int
}

// New builds a server. newHost constructs the base host for each session and
// one-shot run — the binary passes the same wiring `mana run` uses, tests
// pass fakes.
func New(newHost func() host.Host, opts Options) *Server {
	return &Server{
		newHost:  newHost,
		opts:     opts,
		sessions: map[string]*session{},
	}
}

// swappable routes the script's output to the current run's buffer. Runs are
// serialized per session, so the swap is guarded by the session mutex, not
// its own.
type swappable struct {
	host.Host
	buf *strings.Builder
}

func (s *swappable) Out() io.Writer { return s.buf }

// Handler is the surface (D-050).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", s.auth(s.oneShot))
	mux.HandleFunc("POST /sessions", s.auth(s.create))
	mux.HandleFunc("POST /sessions/{id}/run", s.auth(s.run))
	mux.HandleFunc("GET /sessions/{id}", s.auth(s.state))
	mux.HandleFunc("DELETE /sessions/{id}", s.auth(s.remove))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Token != "" {
			if r.Header.Get("Authorization") != "Bearer "+s.opts.Token {
				reply(w, http.StatusUnauthorized, map[string]any{"error": "missing or wrong bearer token"})
				return
			}
		}
		next(w, r)
	}
}

// oneShot runs a script with no session: an ephemeral context window.
func (s *Server) oneShot(w http.ResponseWriter, r *http.Request) {
	src, ok := s.readScript(w, r)
	if !ok {
		return
	}
	prog, perrs := parse(src)
	if perrs != nil {
		reply(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "parse_errors": perrs})
		return
	}
	var buf strings.Builder
	h := &swappable{Host: s.newHost(), buf: &buf}
	report := act.RunWith(prog, h, act.Options{Retries: s.opts.Retries, Timeout: s.opts.Timeout})
	s.sendReport(w, report, buf.String())
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= MaxSessions {
		reply(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("at capacity: %d sessions; delete one first", MaxSessions),
		})
		return
	}
	id := newID()
	base := s.newHost()
	out := &swappable{Host: base, buf: &strings.Builder{}}
	e := evaluator.New(out)
	e.SetTimeout(s.opts.Timeout)
	s.sessions[id] = &session{eval: e, out: out, base: base}
	reply(w, http.StatusCreated, map[string]any{"session": id})
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*session, bool) {
	s.mu.Lock()
	sess, ok := s.sessions[r.PathValue("id")]
	s.mu.Unlock()
	if !ok {
		reply(w, http.StatusNotFound, map[string]any{"error": "no such session"})
	}
	return sess, ok
}

// run executes one script in a session. Flat scripts run on the session
// evaluator; act scripts run as self-contained jobs (D-049).
func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.lookup(w, r)
	if !ok {
		return
	}
	src, ok := s.readScript(w, r)
	if !ok {
		return
	}
	prog, perrs := parse(src)
	if perrs != nil {
		reply(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "parse_errors": perrs})
		return
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.runs++
	var buf strings.Builder
	sess.out.buf = &buf

	acts, _ := act.Split(prog)
	var report *act.Report
	if len(acts) > 0 {
		report = act.RunWith(prog, sess.out, act.Options{Retries: s.opts.Retries, Timeout: s.opts.Timeout})
	} else {
		report = s.runFlatInSession(sess, prog)
	}
	s.sendReport(w, report, buf.String())
}

// runFlatInSession is the REPL's evalOne shaped as a job report: the session
// evaluator keeps its bindings, per-run recording resets, and a reported
// failure is marked handled so it is reported once.
func (s *Server) runFlatInSession(sess *session, prog *ast.Program) *act.Report {
	start := time.Now()
	e := sess.eval
	e.BeginRun()
	v := e.Run(prog)
	out := act.Outcome{
		Name:     "",
		Status:   act.Succeeded,
		Attempts: 1,
		Uses:     e.Uses(),
		Intents:  e.Intents(),
		Steps:    e.Steps(),
	}
	if err, bad := v.(*object.Err); bad {
		out.Status, out.Err = act.Failed, err
		// Reported in this response. Without this, the end-of-run sweep would
		// surface the same failure again after every later submission.
		err.Handle()
	} else if res, has := e.Result(); has {
		out.Result, out.HasResult = res, true
	}
	out.Duration = time.Since(start)
	return &act.Report{Outcomes: []act.Outcome{out}, Elapsed: out.Duration}
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.lookup(w, r)
	if !ok {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	reply(w, http.StatusOK, map[string]any{
		"session":  r.PathValue("id"),
		"bindings": sess.eval.Bindings(),
		"runs":     sess.runs,
	})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	_, ok := s.sessions[r.PathValue("id")]
	delete(s.sessions, r.PathValue("id"))
	s.mu.Unlock()
	if !ok {
		reply(w, http.StatusNotFound, map[string]any{"error": "no such session"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readScript(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxScriptBytes+1))
	if err != nil {
		reply(w, http.StatusBadRequest, map[string]any{"error": "could not read the request body"})
		return "", false
	}
	if len(body) > MaxScriptBytes {
		reply(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("script exceeds %d bytes", MaxScriptBytes)})
		return "", false
	}
	if strings.TrimSpace(string(body)) == "" {
		reply(w, http.StatusBadRequest, map[string]any{"error": "empty script"})
		return "", false
	}
	return string(body), true
}

func (s *Server) sendReport(w http.ResponseWriter, report *act.Report, output string) {
	blob, err := act.JSON(report, output)
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]any{"error": "could not encode the report: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(blob)
	w.Write([]byte("\n"))
}

func parse(src string) (*ast.Program, []string) {
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		return nil, errs
	}
	return prog, nil
}

func reply(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
