.PHONY: build test check install

build:
	go build -trimpath -o bin/aliyundrive ./cmd/aliyundrive

test:
	go test ./...

check:
	go vet ./...
	go test -race ./...

install:
	go install ./cmd/aliyundrive
