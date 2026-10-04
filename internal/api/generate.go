package api

//go:generate sh -c "cd ../.. && npm run --silent openapi:bundle && oapi-codegen -config internal/api/cfg.yaml api/openapi.codegen.yaml && go run ./internal/api/stubgen"
