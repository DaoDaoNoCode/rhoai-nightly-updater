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
	github.com/google/cel-go v0.31.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cel.dev/expr v0.25.1 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/exp v0.0.0-20240823005443-9b4947da3948 // indirect
	golang.org/x/text v0.22.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)
