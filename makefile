.PHONY: bin tunnelit run-test clean-test

bin:
	mkdir -p bin

tunnelit: bin
	go build -o bin/tunnelit .

run-test:
	go run test/main.go

clean-test:
	rm test/*.log
