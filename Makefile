NOW = $(shell date +%Y%m%d%H%M%S)
OS = $(shell uname -n -m)
AFTER_COMMIT = $(shell git log --format="%H" -n 1)

LDFLAGS = "-X 'main.BuildTime=$(NOW)' -X 'main.BuildOSUname=$(OS)' -X 'main.BuildCommit=$(AFTER_COMMIT)' -extldflags '-static -O2'"

GO = go
BIN = heif2png
SRC = *.go

.PHONY: all clean

$(BIN): $(SRC)
		$(GO) build -ldflags $(LDFLAGS) -o $(BIN)

all: $(BIN)

clean:
		rm -f $(BIN)
