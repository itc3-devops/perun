TEST?=./...

.PHONY: config-install get-deps code-analysis test all

all: get-deps code-analysis test

config-install:
	mkdir -p "$(HOME)/.config/cftool"
	cp defaults/main.yaml "$(HOME)/.config/cftool/main.yaml"

get-deps:
	go get -t -v .
	go install github.com/Appliscale/cftool
	go install github.com/stretchr/testify

code-analysis: get-deps
	go tool vet -v .

test: get-deps
	go test -v -cover $(TEST)