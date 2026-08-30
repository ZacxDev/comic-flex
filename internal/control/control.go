package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultAddr is the bind address required by the design: the cluster reaches
// the Pi by LAN IP, so loopback is not an option. The compensating control is
// the bearer token plus a host firewall rule restricting :8790 to the LAN.
const DefaultAddr = "0.0.0.0:8790"

// maxBodyBytes caps request bodies. Every body this API accepts is a handful of
// bytes of JSON.
const maxBodyBytes = 4 << 10

// intervalMin and intervalMax bound POST /api/interval, per proposal §4.2.
const (
	intervalMin = 1
	intervalMax = 3600
)

// Config configures a control Server.
type Config struct {
	// Addr to bind. Empty means DefaultAddr.
	Addr string
	// Token is the bearer credential. Fail-closed: New returns an error and no
	// listener is created if it is empty or shorter than MinTokenBytes.
	Token string
	// Viewer is the port onto the slideshow. Required.
	Viewer Viewer
	// Version is reported by GET /healthz.
	Version string
}

// Server is the Pi-side control API.
type Server struct {
	token   string
	viewer  Viewer
	version string
	http    *http.Server
}

// New builds a Server. It returns an error — and therefore no listener — when
// the token precondition fails.
func New(cfg Config) (*Server, error) {
	if cfg.Viewer == nil {
		return nil, errors.New("control: Viewer is required")
	}
	if err := validateToken(cfg.Token); err != nil {
		return nil, err
	}
	addr := cfg.Addr
	if addr == "" {
		addr = DefaultAddr
	}
	s := &Server{
		token:   cfg.Token,
		viewer:  cfg.Viewer,
		version: cfg.Version,
	}
	s.http = &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s, nil
}

// Addr reports the configured bind address.
func (s *Server) Addr() string { return s.http.Addr }

// Handler exposes the routed handler, for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// ListenAndServe blocks until the server stops. It returns http.ErrServerClosed
// after Shutdown.
func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }

// Shutdown stops the server gracefully. main calls it after gtk.Main() returns.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) routes() http.Handler {
	// api carries every authenticated route. It is mounted behind requireToken
	// as a whole, so a new endpoint added to it is authenticated by
	// construction rather than by remembering to wrap it.
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)
	api.HandleFunc("POST /api/next", s.handleNext)
	api.HandleFunc("POST /api/prev", s.handlePrev)
	api.HandleFunc("POST /api/pause", s.handlePause)
	api.HandleFunc("POST /api/resume", s.handleResume)
	api.HandleFunc("POST /api/viewmode", s.handleViewMode)
	api.HandleFunc("POST /api/goto", s.handleGoto)
	api.HandleFunc("POST /api/interval", s.handleInterval)
	api.HandleFunc("POST /api/rescan", s.handleRescan)

	mux := http.NewServeMux()
	// /healthz touches no viewer state, so it may be unauthenticated.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/api/", s.requireToken(api))
	return mux
}

// ---------------------------------------------------------------------------
// responses
// ---------------------------------------------------------------------------

type errorBody struct {
	Error string `json:"error"`
}

type acceptedBody struct {
	Accepted bool `json:"accepted"`
}

type healthBody struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// accepted is the R1 reply: the mutation is on the GTK loop's queue and the
// handler is done. It is deliberately not 200 — nothing has happened yet.
func accepted(w http.ResponseWriter) {
	writeJSON(w, http.StatusAccepted, acceptedBody{Accepted: true})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, errorBody{Error: msg})
}

func notFound(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusNotFound, errorBody{Error: msg})
}

// decodeBody reads a small JSON body into dst. An absent body is treated as an
// empty object so that endpoints with optional fields answer 400 from their own
// validation rather than from the decoder.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		badRequest(w, "malformed JSON body")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthBody{OK: true, Version: s.version})
}

// handleState is the one R2 endpoint: a read, synchronous, 200, no IdleAdd.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.viewer.Snapshot())
}

func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	s.viewer.Enqueue(s.viewer.Next)
	accepted(w)
}

func (s *Server) handlePrev(w http.ResponseWriter, r *http.Request) {
	s.viewer.Enqueue(s.viewer.Prev)
	accepted(w)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.viewer.Enqueue(func() { s.viewer.SetPaused(true) })
	accepted(w)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.viewer.Enqueue(func() { s.viewer.SetPaused(false) })
	accepted(w)
}

func (s *Server) handleViewMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	// Validation is cheap and touches no viewer state, so it runs here on the
	// handler goroutine and answers 400 directly.
	mode, ok := ParseViewMode(body.Mode)
	if !ok {
		badRequest(w, "unknown view mode "+strconv.Quote(body.Mode)+
			"; want landscape_single, portrait_single or landscape_two")
		return
	}
	s.viewer.Enqueue(func() { s.viewer.SetViewMode(mode) })
	accepted(w)
}

func (s *Server) handleGoto(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   *string `json:"key"`
		Index *int    `json:"index"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	switch {
	case body.Key != nil && *body.Key != "":
		key := *body.Key
		// This lookup exists ONLY to answer 404. It is an R2-class read: it
		// acquires and releases the read lock and returns a plain value, so no
		// lock is held across the Enqueue below (R3).
		//
		// 🔴 The authoritative resolution is GotoKey's, inside the closure,
		// under the lock — the gallery can be replaced by a rescan between the
		// two, and the closure must act on what is there when it runs, not on
		// an index captured here.
		if _, ok := s.viewer.Resolve(key); !ok {
			notFound(w, "key not in the gallery")
			return
		}
		s.viewer.Enqueue(func() { s.viewer.GotoKey(key) })
		accepted(w)

	case body.Index != nil:
		idx := *body.Index
		if idx < 0 || idx >= s.viewer.Snapshot().Total {
			notFound(w, "index out of range")
			return
		}
		s.viewer.Enqueue(func() { s.viewer.GotoIndex(idx) })
		accepted(w)

	default:
		badRequest(w, `goto requires "key" or "index"`)
	}
}

func (s *Server) handleInterval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seconds *int `json:"seconds"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Seconds == nil {
		badRequest(w, `interval requires "seconds"`)
		return
	}
	n := *body.Seconds
	if n < intervalMin || n > intervalMax {
		badRequest(w, "seconds out of range; want "+strconv.Itoa(intervalMin)+"-"+strconv.Itoa(intervalMax))
		return
	}
	s.viewer.Enqueue(func() { s.viewer.SetInterval(n) })
	accepted(w)
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	s.viewer.Enqueue(s.viewer.Rescan)
	accepted(w)
}
