module github.com/useteploy/teploy-observe

go 1.25.0

require (
	github.com/aws/aws-sdk-go-v2 v1.41.6
	github.com/aws/aws-sdk-go-v2/credentials v1.19.15
	github.com/aws/aws-sdk-go-v2/service/s3 v1.99.1
	github.com/jackc/pgx/v5 v5.7.2
	github.com/neutron-build/neutron/go v0.0.0
	golang.org/x/crypto v0.54.0
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.22 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.22 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.14 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.22 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.22 // indirect
	github.com/aws/smithy-go v1.25.0 // indirect
	github.com/gabriel-vasile/mimetype v1.4.12 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.1 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260720211330-0afa2a65878a // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260720211330-0afa2a65878a // indirect
	google.golang.org/grpc v1.82.1 // indirect
)

// Standing replace: the live Neutron workspace. With vendor/ present, builds
// use vendor/ and ignore this — it only matters for FUTURE regeneration.
// Regeneration procedure (vendor must always be generated from the PIN, not
// the workspace):
//   1. git -C ../../Neutron worktree add /tmp/neutron-pin-worktree <pin-sha> (detached)
//   2. change this replace to => /tmp/neutron-pin-worktree/go
//   3. go mod vendor
//   3. go mod vendor
//   4. restore this replace to ../../Neutron/go
//   5. update the two "# ... => " annotation lines for this module in
//      vendor/modules.txt to say ../../Neutron/go (go records the replace
//      target used at generation time; leaving the worktree path there
//      breaks -mod=vendor consistency)
//   6. git -C Neutron checkout <pin-sha> so the submodule pin matches
replace github.com/neutron-build/neutron/go => ../../Neutron/go

replace github.com/useteploy/teploy-observe/sdk/go => ./sdk/go

require (
	github.com/coreos/go-oidc/v3 v3.11.0
	github.com/useteploy/teploy-observe/sdk/go v0.0.0
	go.opentelemetry.io/proto/otlp v1.11.0
	golang.org/x/oauth2 v0.36.0
	google.golang.org/protobuf v1.36.11
)
