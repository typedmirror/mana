BINARY := mana
PKG    := ./...
COVDIR := .coverdata

.PHONY: build run test cover examples lint fmt tidy clean

build:
	go build -o bin/$(BINARY) ./cmd/$(BINARY)

run:
	go run ./cmd/$(BINARY)

test:
	go test -race -cover $(PKG)

# Coverage across the whole tree, not per package: object, host and ast are
# exercised by other packages' tests, so their per-package number reads 0%,
# which is not what it means.
#
# Uses the binary coverage format rather than -coverprofile. A text profile
# written by several test binaries does not union: it reported ast at 0% for 65
# functions the parser tests demonstrably execute, and a total of 32.5% against
# a real figure near 80%. An undercount is still a wrong number.
#
# go test prints its own per-package figures too, including a 0.0% line for
# every package that has no test file of its own. Those are dropped here so the
# only numbers on screen are the merged ones.
cover:
	@rm -rf $(COVDIR) && mkdir -p $(COVDIR)
	@go test -coverpkg=$(PKG) $(PKG) -args -test.gocoverdir=$(PWD)/$(COVDIR) \
		| grep -Ev "coverage:|no test files" || true
	@go tool covdata percent -i=$(COVDIR) | sort

# End-to-end through the built binary, which the unit tests cannot check: the
# exit code is the only thing a caller upstream of Mana can see.
#
# examples/spec_example.mana is deliberately not run here — it is spec §14
# verbatim and points at a host that does not exist. It is covered against a
# scripted host in internal/evaluator/evaluator_test.go.
examples: build
	./bin/$(BINARY) examples/hello.mana
	./bin/$(BINARY) examples/acts.mana
	@! ./bin/$(BINARY) examples/failing.mana >/dev/null 2>&1 && echo "failing.mana exited non-zero, as intended"
	@for s in tests/fallback_chain tests/pipe_transform tests/match_dispatch tests/act_graph; do \
		./bin/$(BINARY) $$s.mana | diff -q - $$s.expected >/dev/null \
			&& echo "$$s.mana matches its golden output" || exit 1; \
	done
	@for s in tests/error_model tests/intent_stack tests/act_failure; do \
		./bin/$(BINARY) $$s.mana >/dev/null 2>&1; \
		test $$? -eq 1 && echo "$$s.mana exited 1, as intended" || exit 1; \
	done

lint:
	go vet $(PKG)

fmt:
	go fmt $(PKG)

tidy:
	go mod tidy

clean:
	rm -rf bin $(COVDIR) coverage.out
