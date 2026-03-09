.PHONY: build run test clean docker-up docker-down tidy chat e2e

build:
	go build -o bin/lokilens ./cmd/lokilens
	go build -o bin/loggen ./cmd/loggen
	go build -o bin/chat ./cmd/chat
	go build -o bin/e2e ./cmd/e2e

run:
	go run ./cmd/lokilens

test:
	go test ./... -v

chat:
	go run ./cmd/chat

e2e:
	go run ./cmd/e2e

tidy:
	go mod tidy

clean:
	rm -rf bin/

docker-up:
	docker compose -f docker/docker-compose.yml up --build -d

docker-down:
	docker compose -f docker/docker-compose.yml down -v

loggen:
	go run ./cmd/loggen
