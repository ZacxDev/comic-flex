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
// the Pi by LAN IP, so loopback is not an option.
//
// ✅ THE FIREWALL RULE THIS COMMENT WAS OWED HAS LANDED, and this paragraph is
// the "say so here and name where it is defined" that the previous version asked
// for. Measured on the Pi (192.168.50.131) on 2026-09-01:
//
//	table inet filter { chain input {                 # policy accept
//	  tcp dport 8790 ip saddr 192.168.50.{94,75,186,191} accept   # the 4 cluster nodes
//	  tcp dport 8790 drop                                          # "not a cluster node"
//	}}
//
// Applied from /etc/nftables.conf ON THE PI by nftables.service (enabled;
// ExecStart is `nft -f /etc/nftables.conf`). The ruleset is ALSO written out in
// homelab-talos, at claudedocs/runbook-comic-flex-pwa-deploy.md §1a, with the
// apply procedure and a rollback — that is where to rebuild it from.
//
// 🔴 But NOTHING REASSERTS IT. The runbook is a human procedure, not a
// reconciler; the rule is not in this repo, and no CI or GitOps applies it. It
// survives exactly as long as that host file does, and a reimaged Pi comes back
// without it — with no alert, and with the API still listening. (Checked: no
// config-management agent on the Pi, no /etc/.git, no /etc/nftables.d, no cron
// or timer referencing nftables.)
//
// Verified from both sides rather than by reading the ruleset alone. From a LAN
// host outside the allow list, tcp/8790 HANGS INDEFINITELY — every observed
// "timeout" was the prober's own ceiling, at 3 s and again at 20 s, not the
// connection giving up — while tcp/22 connects in ~2 ms and a port with no
// listener answers RST in ~2 ms. Three outcomes, so "filtered" is distinguished
// from "closed" and from "host down". The attribution is tighter than that: the
// DROP RULE'S OWN COUNTER advances by exactly the blocked attempts while the
// four accept counters stay still, so it is THIS rule and not the router.
//
// The accept counters also settle the masquerade question /etc/nftables.conf
// raises about itself. A bare counter names no originator, so this was checked
// properly: the PWA pod is an ordinary pod-network pod (hostNetwork unset,
// podIP 10.244.0.x) on talos-jkj-deb, and that node's rule carries an order of
// magnitude more packets than the other three. Traffic leaving 10.244.0.x and
// arriving with saddr 192.168.50.94 can only be SNAT.
//
// What that means for a reader: there are now two layers, not one. Do NOT relax
// the token rules on the strength of it — the drop protects against other LAN
// devices reaching a host with passwordless root, and nothing more. Anything
// that reaches this port from a cluster node and holds the token still gets a
// root-adjacent host, so keep COMIC_FLEX_CONTROL_TOKEN off anything leaving the
// LAN. If the rule is ever removed or the Pi reimaged, the token is once again
// the only control — re-measure rather than trusting this paragraph.
const DefaultAddr = "0.0.0.0:8790"

// maxBodyBytes caps request bodies. Every body this API accepts is a handful of
// bytes of JSON, so 4 KiB is already two orders of magnitude of headroom.
//
// The cap is not decoration: without it an unauthenticated-length body is read
// into memory by the JSON decoder before any validation runs, on a Pi. It is
// pinned by TestBodiesLargerThanTheCapAreRefused.
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
	api.HandleFunc("POST /api/toggle", s.handleToggle)
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

// enqueue is the ONE place an R1 mutation endpoint replies, so the 202/503
// decision cannot drift between the handlers that make it.
//
// 🔴 Enqueue can REFUSE. The GTK main loop drains at up to 30 s per image
// (updateSingleImage sits on an S3 GET) while a client can POST as fast as the
// Pi will accept connections, and every accepted closure costs a permanent
// gotk3 callback-registry entry. An unbounded queue therefore grows without
// limit under a caller that is merely impatient. A refusal is a 503 with
// Retry-After, not a 202 — telling the caller the page turn is queued when it
// is not would be the same lie the trailing-JSON case in decodeBody avoids.
func (s *Server) enqueue(w http.ResponseWriter, fn func()) {
	if !s.viewer.Enqueue(fn) {
		refuse(w, "the display queue is full; retry shortly")
		return
	}
	accepted(w)
}

// refuse is the ONE place a 503 is written, so the status, the Retry-After and
// the JSON shape cannot drift between the two admission points (the GTK queue
// cap and the concurrent-scan bound).
//
// 🔴 503 IS A CONTRACT CHANGE AND CONSUMERS MUST BE UPDATED IN LOCKSTEP.
// Proposal §4.2 (clawgate #442, in the homelab-infra repo) says a mutation
// always answers 202. It no longer does. A caller that branches only on
// `202 vs 4xx` reads this as a dead Pi and will escalate, page, or mark the
// display down — for what is ordinary backpressure that clears in a second.
//
// Before this runs on the Pi, BOTH of these must land in homelab-infra:
//
//   - §4.2 amended to document 503 + Retry-After on every mutation endpoint,
//   - the cluster-side caller taught that 503 means RETRY (honouring
//     Retry-After), not FAILED.
//
// Deliberately not fixed by widening 202: telling a caller its page turn is
// queued when nothing was queued is the same lie as accepting a body with a
// discarded second JSON object, which decodeBody refuses two functions down.
func refuse(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: msg})
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
//
// Two refusals that a single Decode does NOT give you, and that both endpoints
// with a body depend on:
//
//   - MaxBytesReader caps what the decoder will read at all. Without it a
//     multi-megabyte body is buffered on a Raspberry Pi before any of our
//     validation runs.
//   - The stream must hold EXACTLY ONE value. json.Decoder stops at the end of
//     the first one and reports success, so `{"seconds":5}{"seconds":9999}`
//     would be accepted with the second object silently discarded — the caller
//     would be told 202 for a value it never sent.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{
				Error: "body larger than " + strconv.Itoa(maxBodyBytes) + " bytes",
			})
			return false
		}
		badRequest(w, "malformed JSON body")
		return false
	}
	// Exactly one value: anything after the first one is a second request the
	// caller believes was honoured.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		badRequest(w, "body must contain exactly one JSON object")
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
	s.enqueue(w, s.viewer.Next)
}

func (s *Server) handlePrev(w http.ResponseWriter, r *http.Request) {
	s.enqueue(w, s.viewer.Prev)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.enqueue(w, func() { s.viewer.SetPaused(true) })
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.enqueue(w, func() { s.viewer.SetPaused(false) })
}

// handleToggle flips the slideshow between paused and playing.
//
// 🔴 THE FLIP IS MADE IN THE CLOSURE, NOT HERE, AND THIS HANDLER READS NOTHING.
// That is the entire reason the endpoint exists. The obvious implementation —
//
//	if s.viewer.Snapshot().Paused {   // read, on the handler goroutine
//	    s.enqueue(w, func() { s.viewer.SetPaused(false) })   // write, later
//	} else {
//	    s.enqueue(w, func() { s.viewer.SetPaused(true) })
//	}
//
// — is the bug this endpoint replaces, moved one hop closer to the state. The
// read and the write are two lock acquisitions with an unbounded gap between
// them (the closure sits behind up to maxQueuedMutations page turns, each of
// which can hold the GTK loop for 30 s on an S3 GET), and in that gap the `p`
// keypress on the Pi, a queued POST /api/pause, or a second toggle can move the
// flag. The absolute value written then derives from a state that no longer
// exists: the user's tap does nothing, or silently undoes someone else's.
//
// So the port method takes no argument (Viewer.TogglePaused), the whole
// read-and-flip happens under ONE lock acquisition inside the implementation,
// and there is no value for this handler to capture even if a maintainer wanted
// to. TestToggleReadsAndFlipsInsideTheClosure is the behavioural guard: it moves
// the paused flag AFTER the 202 and asserts the flip landed on the late value.
//
// 🔴 WHAT THE CALLER IS TOLD, and why it is not the resulting state. This
// answers 202 {"accepted":true}, exactly like the other seven enqueued
// mutations. The state the flip lands on is not knowable here without waiting
// for the closure to run, and waiting is forbidden — the loop drains at up to
// 30 s per image. Putting a predicted `"paused": !observed` in the body would be
// the same lie as a 202 for work that was never queued: it is a guess made from
// a read taken before the flip, and the guard above exists precisely because
// that read can be stale. A caller that needs the landed state polls
// GET /api/state, which is an R2 read and is deliberately NOT subject to the
// queue cap (TestReadsAreNotSubjectToTheQueueCap), so it answers even while the
// toggle is still queued.
//
// 🔴 SCANNING — the deliberate tri-state decision. GET /api/state reports three
// user-visible conditions (playing, paused, scanning), and a scan can be
// outstanding for the whole drain latency of the display queue, including the
// startup scan at boot. The decision: A TOGGLE DURING A SCAN PROCEEDS. It is not
// refused, not deferred, and it does not touch Scanning.
//
// The reasons, since "refuse with 409 while scanning" is the plausible
// alternative:
//
//   - paused and scanning are ORTHOGONAL axes, not three values of one field.
//     The slideshow is paused-or-playing whether or not a listing is running,
//     and the flag it flips is read by the slide timer on its next tick.
//     Collapsing them would be the flattening this API refuses elsewhere (see
//     Snapshot.Scanning: `Scanning && Total == 0` is "indexing…", Scanning alone
//     is not).
//   - a refusal would have to be decided from a read taken here, which is the
//     stale read this whole endpoint exists to eliminate. The scan can start or
//     end between the check and the closure, so the refusal would be wrong in
//     both directions.
//   - Scanning is true at boot and after every rescan, so refusing would make
//     the play/pause button dead exactly when an operator is most likely to
//     press it.
//
// The tri-state is preserved for the UI by scanning staying reported, and
// untouched, in GET /api/state. TestToggleWhileScanningFlipsAndLeavesScanning
// pins both halves.
func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	s.enqueue(w, func() { s.viewer.TogglePaused() })
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
	s.enqueue(w, func() { s.viewer.SetViewMode(mode) })
}

// handleGoto selects a page by key or by index.
//
// 🔴 The two arms answer 202 identically but behave DIFFERENTLY when the
// gallery changes between the handler's bounds check and the closure running.
// This is deliberate, and it is documented here because both arms look
// interchangeable from the outside:
//
//	key   — a key that has left the gallery is a NO-OP. GotoKey re-resolves under
//	        the lock and finds nothing, so the display stays where it was. The
//	        202 means "queued", and what got queued turned out to be nothing.
//	index — an index that is now out of range is CLAMPED TO THE LAST IMAGE.
//	        gotoIndex must not panic on images[idx], and refusing inside the
//	        closure has nobody left to tell, so it lands on the nearest page
//	        instead. `{"index":500}` against a gallery that shrank to 40 lands on
//	        image 39, NOT on nothing.
//
// The asymmetry is the right one — a key names a specific comic, an index names
// a position, and a position in a shortened list is nearest-neighbour — but a
// caller that needs "this exact page or nothing" must use the key arm. A caller
// that polls GET /api/state after a 202 sees where it actually landed.
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
		s.enqueue(w, func() { s.viewer.GotoKey(key) })

	case body.Index != nil:
		idx := *body.Index
		if idx < 0 || idx >= s.viewer.Snapshot().Total {
			notFound(w, "index out of range")
			return
		}
		s.enqueue(w, func() { s.viewer.GotoIndex(idx) })

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
	s.enqueue(w, func() { s.viewer.SetInterval(n) })
}

// handleRescan starts a bucket listing. It is the ONE mutation endpoint that
// does not go through Server.enqueue, and that is the fix for a hole rather than
// an inconsistency.
//
// 🔴 Round-1 this read `s.enqueue(w, s.viewer.Rescan)`, which looked like the
// most bounded endpoint of the nine and was in fact the only unbounded one.
// Rescan spawns the listing onto its own goroutine and returns in microseconds,
// so the queue slot it reserved was released before the next request arrived and
// the cap never engaged. Measured against the round-1 code, driving the real
// enqueueBounded + adapter 500 times:
//
//	attempts=500 refused(503)=0 queueDepth=0 scansInFlight=500
//
// — 500 concurrent MinIO ListObjects, each able to hold for the 2 minute
// listTimeout, on a Raspberry Pi. Backpressure was on the eight cheap endpoints
// and absent from the expensive one.
//
// So the bound lives where the work does (the viewer's concurrent-listing cap)
// and the admission answer comes back SYNCHRONOUSLY, while the caller is still
// there to be told 503. Nothing in Rescan touches a widget, so there is no GTK
// thread requirement to satisfy by enqueueing it.
func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if !s.viewer.Rescan() {
		// Plural: the bound is maxConcurrentScans (4 today), not 1. The singular
		// wording told an operator a concurrent-scan refusal meant ONE listing
		// was running.
		//
		// 🔴 "scan", NOT "bucket listing" — round 4. A scan now spans the listing
		// AND the completion closure the implementation hands to the display, so
		// this refusal can be held entirely by queued display callbacks with ZERO
		// listings running. "the bucket-listing budget is exhausted" sent the
		// operator to look for network activity that is not there.
		refuse(w, "the scan budget is exhausted (a scan is held until its results have been "+
			"displayed, so this can be queued display work rather than a running listing); "+
			"retry shortly")
		return
	}
	accepted(w)
}
