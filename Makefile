.PHONY: all generate build test vet lint e2e clean

all: generate vet lint test build

# Builds the monthly Lambda zip that bedrock-admin embeds.
generate:
	go generate ./...

build: generate
	mkdir -p bin
	go build -o bin/bedrock-admin ./cmd/bedrock-admin
	go build -o bin/bedrock ./cmd/bedrock

test: generate
	go test ./...

vet: generate
	go vet ./...

lint: generate
	golangci-lint run ./...

# make e2e SUITE=cli (see tests/e2e/README.md; every suite but cli uses a real AWS account)
e2e:
	tests/e2e/run.sh $(SUITE)

clean:
	rm -rf bin dist internal/lambdacode/assets/bootstrap.zip
