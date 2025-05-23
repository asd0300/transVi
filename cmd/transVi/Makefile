# Makefile

BINARY = transVi
INPUT ?= ./sample.mp4

.PHONY: all build run clean

all: run

build:
	go build -o $(BINARY)

run: build
	./$(BINARY) -input $(INPUT)

clean:
	rm -f $(BINARY)
