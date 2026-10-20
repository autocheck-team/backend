// Package openapi встраивает сгенерированную спецификацию REST API (grpc-gateway) в бинарник.
package openapi

import _ "embed"

// Spec — OpenAPI v2 (Swagger) всего API, генерируется buf generate.
//
//go:embed autocheck.swagger.yaml
var Spec []byte
