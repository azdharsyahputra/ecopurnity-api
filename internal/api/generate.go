// Package api is generated from api/openapi.yaml: models, the net/http router and the typed (strict) handler interface.
// Regenerate with `make gen` after changing the spec.
package api

//go:generate sh -c "cd ../.. && npm run --silent openapi:bundle && oapi-codegen -config internal/api/cfg.yaml api/openapi.codegen.yaml && go run ./internal/api/stubgen"
