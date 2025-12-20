.PHONY: build test deploy clean

# Build the Lambda function for ARM64 (Amazon Linux 2023)
build:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap main.go
	zip function.zip bootstrap

# Run integration tests
test:
	go test -v

# Clean build artifacts
clean:
	rm -f bootstrap function.zip

# Download dependencies
deps:
	go mod download
	go mod tidy

