.PHONY: build run test vet fmt docker

build:
	go build -o bin/framewright ./cmd/framewright

run: build
	DATA_DIR=./data ./bin/framewright

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# Multi-arch, matching docs/DECISIONS.md. Requires `docker buildx create --use`
# once per machine if you haven't already.
docker:
	docker buildx build --platform linux/amd64,linux/arm64 -t framewright:local --load .
