BINARY     := cinevote
IMAGE      := ghcr.io/o5ten/cinevote
DEMO_IMAGE := ghcr.io/o5ten/cinevote-demo

.PHONY: run demo build test fmt vet lint docker docker-run docker-demo compose compose-demo compose-build clean

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

## docker: build both images — production and demo
docker:
	docker build --target production -t $(IMAGE):dev .
	docker build --target demo -t $(DEMO_IMAGE):dev .

## docker-run: run the production image with a local volume for the database
docker-run: docker
	docker run --rm -p 8080:8080 --env-file .env -v cinevote-data:/data $(IMAGE):dev

## docker-demo: run the demo image — no configuration, no volume
docker-demo: docker
	docker run --rm -p 8080:8080 $(DEMO_IMAGE):dev

## compose: run the published production image
compose:
	docker compose up -d

## compose-demo: run the published demo image
compose-demo:
	docker compose --profile demo up

## compose-build: build both images from this checkout and run production
compose-build:
	docker compose -f docker-compose.build.yml up --build -d

clean:
	rm -rf dist data
