.PHONY: test lint chart-check build
test:
	go test ./...
lint:
	go vet ./...
chart-check:
	helm lint charts/reprise
	helm template reprise charts/reprise --namespace reprise >/dev/null
build:
	go build ./cmd/reprise
