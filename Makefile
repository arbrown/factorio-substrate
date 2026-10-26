.PHONY: test build deploy verify clean

test:
	go test -v -race ./...

build:
	mkdir -p bin
	go build -o bin/factorio-sidecar ./cmd/factorio-sidecar
	go build -o bin/factorio-gateway ./cmd/factorio-gateway
	go build -o bin/factorio-probe ./cmd/factorio-probe

deploy:
	./hack/deploy.sh

verify:
	./hack/test-e2e.sh

verify-with-idle:
	IDLE_TIMEOUT=30s ./hack/deploy.sh
	VERIFY_IDLE_SUSPEND=true IDLE_WAIT_SECS=40 ./hack/test-e2e.sh
	IDLE_TIMEOUT=10m ./hack/deploy.sh

clean:
	rm -rf bin/
