package server

import (
	"github.com/getkin/kin-openapi/openapi3"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

func apiSpec() (*openapi3.T, error) { return api.GetSwagger() }
