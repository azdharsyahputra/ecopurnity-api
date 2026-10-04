package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
)

type Error struct {
	Status  int               `json:"-"`
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func writeError(w http.ResponseWriter, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(map[string]*Error{"error": e})
}

func validationError(err error) *Error {
	fields := map[string]string{}
	var collect func(error)
	collect = func(err error) {

		if multi, ok := err.(openapi3.MultiError); ok {
			for _, e := range multi {
				collect(e)
			}
			return
		}
		var reqErr *openapi3filter.RequestError
		if errors.As(err, &reqErr) {
			if reqErr.Parameter != nil {
				fields[reqErr.Parameter.Name] = reason(reqErr.Err, reqErr.Reason)
				return
			}
			if reqErr.Err != nil {
				collect(reqErr.Err)
				return
			}
		}
		var schemaErr *openapi3.SchemaError
		if errors.As(err, &schemaErr) {
			key := strings.Join(schemaErr.JSONPointer(), ".")
			if key == "" {
				key = "body"
			}
			fields[key] = schemaErr.Reason
			return
		}
		fields["body"] = err.Error()
	}
	collect(err)
	return &Error{Status: http.StatusUnprocessableEntity, Code: "validation", Message: "Periksa kembali isian", Fields: fields}
}

func reason(err error, fallback string) string {
	if multi, ok := err.(openapi3.MultiError); ok && len(multi) > 0 {
		return reason(multi[0], fallback)
	}
	var schemaErr *openapi3.SchemaError
	if errors.As(err, &schemaErr) {
		return schemaErr.Reason
	}
	if fallback != "" {
		return fallback
	}
	if err != nil {
		return err.Error()
	}
	return "invalid"
}
