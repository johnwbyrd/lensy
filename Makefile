.PHONY: build test demo stop clean

build:
	cd eventcore && go build -o ../bin/eventcore .
	cd console && (test -x node_modules/.bin/tsc && node_modules/.bin/tsc -p . || tsc -p .) && cp index.html styles.css dist/

test:
	cd eventcore && go test ./...
	cd perception && python3 -m unittest -v

demo:
	scripts/demo.sh

stop:
	scripts/demo.sh stop

clean: stop
	rm -rf bin console/dist data
