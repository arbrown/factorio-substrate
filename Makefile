.PHONY: test build deploy verify verify-with-idle prove watch watch-transitions check-update clean

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

prove:
	./hack/prove-e2e.sh

watch:
	./hack/watch-actors.sh

watch-transitions:
	./hack/watch-actors.sh --transitions

check-update:
	@kubectl run curl-check-update --rm -i --restart=Never -n factorio --image=curlimages/curl:latest -- \
		curl -sS -X POST http://factorio-gateway-http.factorio.svc.cluster.local:8080/check-update | jq .

verify-with-idle:
	IDLE_TIMEOUT=30s ./hack/deploy.sh
	VERIFY_IDLE_SUSPEND=true IDLE_WAIT_SECS=40 ./hack/test-e2e.sh
	IDLE_TIMEOUT=10m ./hack/deploy.sh

clean:
	rm -rf bin/
