.PHONY: demo chaos-report test test-unit test-integration build run token compose-up compose-down observe load benchmark resilience

demo:
	sh ./scripts/smoke.sh

chaos-report:
	powershell -ExecutionPolicy Bypass -File scripts/chaos-report.ps1

test:
	go test ./... -count=1

test-unit:
	go test ./internal/... -count=1

test-integration:
	go test ./integration -count=1

build:
	mkdir -p bin
	go build -o bin/gatego ./cmd/gatego
	go build -o bin/demo-backend ./cmd/demo-backend
	go build -o bin/token ./cmd/token
	go build -o bin/loadgen ./cmd/loadgen

run:
	go run ./cmd/gatego -config configs/gatego.json

token:
	go run ./cmd/token

compose-up:
	docker compose up --build -d

compose-down:
	docker compose --profile observability down

observe:
	docker compose --profile observability up --build -d

load:
	go run ./cmd/loadgen -url http://localhost:8080/public/echo -c 50 -d 10s

benchmark:
	powershell -ExecutionPolicy Bypass -File scripts/benchmark.ps1

resilience:
	powershell -ExecutionPolicy Bypass -File scripts/resilience.ps1
