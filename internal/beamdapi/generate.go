// Package beamdapi contains Go types generated from the beamd hosted
// control-plane OpenAPI spec (openapi.json). It is the shared contract between
// this repo (CLI + edge) and the hosted web app (beamd-web): the web app owns
// the spec, exports it here, and these types are generated from it.
//
// Do NOT edit types.gen.go by hand. Regenerate with `make api-gen` (or
// `go generate ./internal/beamdapi/...`) and commit. CI runs ./check-drift.sh
// to fail the build if the committed types fall out of sync with the spec.
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 --config oapi-codegen.yaml openapi.json
package beamdapi
