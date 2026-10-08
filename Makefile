.PHONY: build test vet fmt integration
build:
	go build -trimpath -o bin/envctl ./cmd/envctl
test:
	go test -race -count=1 ./...
vet:
	go vet ./...
fmt:
	gofmt -w cmd internal integration
integration:
	ENVCTL_INTEGRATION=1 go test -tags=integration -count=1 -timeout=9m -v ./integration
