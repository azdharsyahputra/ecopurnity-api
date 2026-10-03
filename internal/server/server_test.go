package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func code(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestRoutingValidationAndErrors(t *testing.T) {
	h, err := (&Server{}).Handler()
	if err != nil {
		t.Fatal(err)
	}

	if status, _ := do(t, h, "GET", "/healthz", ""); status != http.StatusNoContent {
		t.Fatalf("healthz: %d", status)
	}

	// A routed, valid request to an operation nobody implemented yet: 501 in the error contract.
	status, body := do(t, h, "GET", "/api/v1/markets?page=1", "")
	if status != http.StatusNotImplemented || code(body) != "not_implemented" {
		t.Fatalf("markets: %d %v", status, body)
	}

	// Request validation against the spec: wrong type in the body is a 422 with fields.
	status, body = do(t, h, "POST", "/api/v1/auth/login", `{"email": 5, "password": "x"}`)
	if status != http.StatusUnprocessableEntity || code(body) != "validation" {
		t.Fatalf("login validation: %d %v", status, body)
	}
	if fields, _ := body["error"].(map[string]any)["fields"].(map[string]any); len(fields) == 0 {
		t.Fatalf("login validation: no fields in %v", body)
	}

	// Query parameter outside its schema (pageSize max 50).
	status, body = do(t, h, "GET", "/api/v1/markets?pageSize=500", "")
	if fields, _ := body["error"].(map[string]any)["fields"].(map[string]any); status != http.StatusUnprocessableEntity || fields["pageSize"] == nil {
		t.Fatalf("pageSize: %d %v", status, body)
	}

	// Unknown endpoint under the API prefix.
	if status, body = do(t, h, "GET", "/api/v1/nope", ""); status != http.StatusNotFound || code(body) != "not_found" {
		t.Fatalf("unknown: %d %v", status, body)
	}
}

// Every operation in the spec is routed: with placeholder path params, no body and no session it must reach request
// validation (422), the handler (501 until implemented, or the handler's own 2xx/401/403/...), never 404/405/5xx.
func TestEveryOperationIsRouted(t *testing.T) {
	h, err := (&Server{}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	spec, err := apiSpec()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for path, item := range spec.Paths.Map() {
		for method := range item.Operations() {
			p := path
			for strings.Contains(p, "{") {
				i, j := strings.Index(p, "{"), strings.Index(p, "}")
				p = p[:i] + "00000000-0000-0000-0000-000000000000" + p[j+1:]
			}
			status, body := do(t, h, method, BasePath+p, "")
			if status == http.StatusNotFound || status == http.StatusMethodNotAllowed || (status >= 500 && status != http.StatusNotImplemented) {
				t.Errorf("%s %s: %d %v", method, path, status, body)
			}
			n++
		}
	}
	if n != 164 {
		t.Errorf("operations in spec: %d, want 164", n)
	}
}
