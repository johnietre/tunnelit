.PHONY: tunnelit run-test clean-test

tunnelit:
	go build -o bin/tunnelit .

run-test:
	go run test/main.go

clean-test:
	rm test/*.log
