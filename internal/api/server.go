// Package api serves campaign management over HTTP.
//
//	GET  /healthz                    liveness, no token needed
//	GET  /me                         the caller, as their token describes them
//	POST /campaigns                  create a campaign (admins)
//	POST /campaigns/{slug}/members   add or invite a person (admins, the campaign's officers)
//
// Every route but /healthz needs an authentik token as a bearer token.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pixil98/campaign-manager/internal/campaign"
)

// Authenticator reads the caller from a bearer token.
type Authenticator interface {
	Caller(ctx context.Context, token string) (campaign.Caller, error)
}

// Campaigns is what the API does on a caller's behalf.
type Campaigns interface {
	Create(ctx context.Context, caller campaign.Caller, req campaign.CreateRequest) (*campaign.Created, error)
	Invite(ctx context.Context, caller campaign.Caller, slug string, req campaign.InviteRequest) (*campaign.Invited, error)
}

// maxBody bounds a request body; every request is a few short fields.
const maxBody = 64 << 10

// Server is the HTTP API, run as a service worker.
type Server struct {
	addr    string
	handler http.Handler
}

// NewServer returns the API on addr, checking callers with auth.
func NewServer(addr string, auth Authenticator, campaigns Campaigns) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	h := &handlers{campaigns: campaigns}
	mux.Handle("GET /me", authenticated(auth, h.me))
	mux.Handle("POST /campaigns", authenticated(auth, h.create))
	mux.Handle("POST /campaigns/{slug}/members", authenticated(auth, h.invite))
	return &Server{addr: addr, handler: logged(mux)}
}

// Handler is the API's handler, for tests.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Start serves until ctx is done, then lets requests in flight finish.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopped <- srv.Shutdown(shutdown)
	}()

	slog.InfoContext(ctx, "serving api", "addr", s.addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving api: %w", err)
	}
	return <-stopped
}

type handlers struct {
	campaigns Campaigns
}

func (h *handlers) me(w http.ResponseWriter, _ *http.Request, caller campaign.Caller) {
	writeJSON(w, http.StatusOK, caller)
}

func (h *handlers) create(w http.ResponseWriter, r *http.Request, caller campaign.Caller) {
	var req campaign.CreateRequest
	if !readJSON(w, r, &req) {
		return
	}
	created, err := h.campaigns.Create(r.Context(), caller, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created.New {
		status = http.StatusCreated
	}
	writeJSON(w, status, created)
}

func (h *handlers) invite(w http.ResponseWriter, r *http.Request, caller campaign.Caller) {
	var req campaign.InviteRequest
	if !readJSON(w, r, &req) {
		return
	}
	invited, err := h.campaigns.Invite(r.Context(), caller, r.PathValue("slug"), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, invited)
}

// authenticated runs next for the caller its bearer token names, and answers
// 401 without one, or with a token that does not verify.
func authenticated(auth Authenticator, next func(http.ResponseWriter, *http.Request, campaign.Caller)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorBody{"a bearer token is required"})
			return
		}
		caller, err := auth.Caller(r.Context(), token)
		if err != nil {
			slog.InfoContext(r.Context(), "rejected token", "error", err)
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{"the token is not valid"})
			return
		}
		if rec, ok := w.(*recorder); ok {
			rec.caller = caller.Username
		}
		next(w, r, caller)
	})
}

type errorBody struct {
	Error string `json:"error"`
}

// writeError answers with err's status. Only the campaign package's errors
// are shown to the caller; anything else is logged and answered with a
// plain 500, since it can carry details of authentik or the wiki.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	for _, known := range []struct {
		err    error
		status int
	}{
		{campaign.ErrInvalid, http.StatusBadRequest},
		{campaign.ErrForbidden, http.StatusForbidden},
		{campaign.ErrNotFound, http.StatusNotFound},
		{campaign.ErrConflict, http.StatusConflict},
	} {
		if errors.Is(err, known.err) {
			writeJSON(w, known.status, errorBody{err.Error()})
			return
		}
	}
	slog.ErrorContext(r.Context(), "handling request", "method", r.Method, "path", r.URL.Path, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorBody{"something went wrong; see the service's logs"})
}

// readJSON decodes the request body into v, answering 400 itself if it
// cannot. Unknown fields are refused, so a misspelled one is not silently
// ignored.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"reading request body: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writing response", "error", err)
	}
}

// recorder notes a response's status, and who it was for, for logged.
type recorder struct {
	http.ResponseWriter
	status int
	caller string
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// logged logs every request but health checks once it is answered.
func logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		slog.InfoContext(r.Context(), "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"caller", rec.caller, "duration", time.Since(start))
	})
}
