.DEFAULT_GOAL := help

CONFIG ?= config.yaml

.PHONY: help init build run cli test vet check fmt

help:
	@printf '%s\n' \
	  'make init    Copy example configuration without overwriting local files' \
	  'make build   Build bin/kagari' \
	  'make run     Build and start the Telegram service' \
	  'make cli ARGS="..."  Build and execute a CLI command' \
	  'make test    Run all Go tests' \
	  'make vet     Run go vet' \
	  'make check   Run tests and go vet' \
	  'make fmt     Format Go code' \
	  'CONFIG defaults to config.yaml; override with CONFIG=path/to/config.yaml'

init:
	@test -e .env || cp -n .env.example .env
	@test -e config.yaml || cp -n config.example.yaml config.yaml
	@test -e profile.md || cp -n profile.example.md profile.md

build:
	mkdir -p bin
	go build -o bin/kagari ./cmd/kagari

build-linux:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/kagari-linux-amd64 ./cmd/kagari

run: build
	./bin/kagari -config "$(CONFIG)" run

cli: build
	./bin/kagari -config "$(CONFIG)" $(ARGS)

test:
	go test ./...

vet:
	go vet ./...

check: test vet

fmt:
	go fmt ./...
