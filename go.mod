module github.com/juntwang/rhoai-nightly-updater

go 1.27.0

// The toolchain CI and the Containerfile build with (golang 1.27.4): with
// GOTOOLCHAIN=auto (the default) an older local go switches to it. The go
// line stays 1.27.0: golangci-lint refuses a go line newer than the Go it
// was built with.
toolchain go1.27.4

// frontend/node_modules holds a Go package of an npm dependency (flatted);
// ./... must not vet, lint or test it.
ignore ./frontend/node_modules

require (
	cel.dev/cel-go v0.33.4
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cel.dev/expr v0.26.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/exp v0.0.0-20261119141654-b15d094f3610 // indirect
	golang.org/x/text v0.44.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261118221215-c8830f628b3f // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261118221215-c8830f628b3f // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
