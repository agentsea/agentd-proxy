
.PHONY: test
test:
	PROXY_TEST=1 go test -v .