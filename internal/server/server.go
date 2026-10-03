// Package server wires the generated API (internal/api) to the stores: routing under /api/v1, request validation against
// the spec, the error contract, and the operation implementations (one file per area, added as they are built).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3filter"
	middleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
	"github.com/azdharsyahputra/ecopurnity-api/internal/db"
)

const BasePath = "/api/v1"

// Server implements api.StrictServerInterface. Every operation answers 501 through the embedded Unimplemented until a
// method with the same name is defined on *Server.
type Server struct {
	api.Unimplemented

	DB        *db.Cluster
	Analytics *analytics.Client
	Log       *slog.Logger
}

var _ api.StrictServerInterface = (*Server)(nil)

// Handler returns the full HTTP handler: health probes plus the API with request validation.
func (s *Server) Handler() (http.Handler, error) {
	spec, err := api.GetSwagger()
	if err != nil {
		return nil, err
	}
	// Spec paths are relative to BasePath; the validator strips it (Prefix) and ignores hosts (the servers entry is for docs).
	spec.Servers = nil

	validate := middleware.OapiRequestValidatorWithOptions(spec, &middleware.Options{
		DoNotValidateServers: true,
		Prefix:               BasePath,
		Options: openapi3filter.Options{
			MultiError: true,
			// The session cookie is checked by the auth middleware and the handlers (401/403 with the error contract),
			// not by the validator.
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, _ *http.Request, opts middleware.ErrorHandlerOpts) {
			if opts.MatchedRoute == nil {
				writeError(w, &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Endpoint tidak ditemukan"})
				return
			}
			writeError(w, validationError(err))
		},
	})

	strict := api.NewStrictHandlerWithOptions(s, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: err.Error()})
		},
		ResponseErrorHandlerFunc: s.handlerError,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", s.readyz)
	apiHandler := api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		BaseURL: BasePath,
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			// Path/query parameters that fail to bind (e.g. a non-integer page).
			writeError(w, &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: err.Error()})
		},
	})
	mux.Handle(BasePath+"/", validate(apiHandler))
	return mux, nil
}

// handlerError maps an error returned by an operation to the error contract.
func (s *Server) handlerError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	switch {
	case errors.As(err, &apiErr):
		writeError(w, apiErr)
	case errors.Is(err, api.ErrNotImplemented):
		writeError(w, &Error{Status: http.StatusNotImplemented, Code: "not_implemented", Message: "Endpoint ini belum diimplementasikan"})
	default:
		if s.Log != nil {
			s.Log.Error("handler", "method", r.Method, "path", r.URL.Path, "err", err)
		}
		writeError(w, &Error{Status: http.StatusInternalServerError, Code: "internal", Message: "Terjadi kesalahan di server"})
	}
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	code := http.StatusOK
	if s.DB != nil {
		st, err := s.DB.Status(r.Context())
		body["postgres"] = st
		if err != nil {
			code = http.StatusServiceUnavailable
		}
	}
	if s.Analytics != nil {
		body["clickhouse"] = "ok"
		if err := s.Analytics.Ping(r.Context()); err != nil {
			// Analytics down degrades dashboards but must not take the API out of rotation.
			body["clickhouse"] = err.Error()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
