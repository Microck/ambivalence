.PHONY: test build e2e apk
test:
	go test ./...
build:
	go build -buildvcs=false -o bin/ambivalence ./cmd/ambivalence
e2e: build
	./scripts/e2e-fake.sh
apk:
	./scripts/build-apk.sh
