.PHONY: build test fmt vet smoke integration up down logs shell

build:
	go build ./...

test:
	go test ./...

fmt:
	gofmt -l -w .

vet:
	go vet ./...

# The real gate: can the container still reach both marketplaces?
# Mercado Livre is the fragile one -- it only answers a headful browser.
smoke:
	docker compose build
	docker compose run --rm --no-deps bot ptb probe all "ps5"

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f bot

shell:
	docker compose run --rm --no-deps --entrypoint sh bot

# Live end-to-end test against both marketplaces, inside the container.
# The runtime image has no Go toolchain, so the test binary is cross-compiled
# here and mounted in. modernc.org/sqlite is pure Go, so CGO stays off.
integration: build
	CGO_ENABLED=0 GOOS=linux GOARCH=$(shell go env GOARCH) \
	  go test -c -tags=integration -o ./browser.test ./internal/browser/
	CGO_ENABLED=0 GOOS=linux GOARCH=$(shell go env GOARCH) \
	  go test -c -tags=integration -o ./tracker.test ./internal/tracker/
	docker compose run --rm --no-deps \
	  -v "$(PWD)/browser.test:/tmp/browser.test:ro" \
	  bot /tmp/browser.test -test.v -test.timeout=5m
	docker compose run --rm --no-deps \
	  -v "$(PWD)/tracker.test:/tmp/tracker.test:ro" \
	  bot /tmp/tracker.test -test.v -test.timeout=10m
	rm -f ./browser.test ./tracker.test
