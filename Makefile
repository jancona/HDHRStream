BINARY := hdhrstream

.PHONY: build run vet clean install uninstall

build:
	CGO_ENABLED=0 go build -trimpath -o $(BINARY) .

run: build
	./$(BINARY)

vet:
	go vet ./...

clean:
	rm -f $(BINARY)

install:
	./install.sh

uninstall:
	./uninstall.sh
