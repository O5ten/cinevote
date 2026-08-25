BINARY := cinevote
IMAGE  := ghcr.io/mikaelo/cinevote

.PHONY: run demo build test fmt vet lint docker docker-run docker-demo compose compose-demo clean

## run: start the server locally on :8080 (reads .env if present)
run:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/$(BINARY)

## demo: start a fully seeded demo on :8080 — needs no configuration at all
demo:
	go run ./cmd/$(BINARY) -demo

## build: compile a static binary into dist/
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/$(BINARY) ./cmd/$(BINARY)

## test: run the full test suite with the race detector
test:
	go test -race ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

## lint: what CI checks before it runs the tests
lint: vet
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi

## docker: build the container image
docker:
	docker build -t $(IMAGE):dev .

## docker-run: run the image with a local volume for the database
docker-run: docker
	docker run --rm -p 8080:8080 --env-file .env -v cinevote-data:/data $(IMAGE):dev

## docker-demo: run the demo in a container, no env file, no volume
docker-demo: docker
	docker run --rm -p 8080:8080 -e CINEVOTE_DB= $(IMAGE):dev -demo

## compose: bring the stack up in the background
compose:
	docker compose up --build -d

## compose-demo: the same demo via compose
compose-demo:
	docker compose --profile demo up --build

clean:
	rm -rf dist data
