ts := $(shell /bin/date "+%s")
hash := $(shell git log -1 --pretty=format:"%H")

all: go

go:
	go mod download
	go build -ldflags "-X main.BuildTimestamp=$(ts) -X main.BuildGitHash=$(hash)" .

clean:
	rm -rf poll-bot

docker:
	docker build -t jheidel/poll-bot .
	docker push jheidel/poll-bot

.PHONY: go clean

