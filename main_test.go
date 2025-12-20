package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestHandler_GET(t *testing.T) {
	ctx := context.Background()

	request := events.LambdaFunctionURLRequest{
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "GET",
				Path:   "/",
			},
		},
		RawPath: "/",
	}

	response, err := handler(ctx, request)
	if err != nil {
		t.Fatalf("Handler returned error: %v", err)
	}

	if response.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", response.StatusCode)
	}

	var body Response
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if body.Message != "Hello from Lambda Function URL!" {
		t.Errorf("Expected message 'Hello from Lambda Function URL!', got '%s'", body.Message)
	}

	if body.Status != 200 {
		t.Errorf("Expected status 200, got %d", body.Status)
	}
}

func TestHandler_POST(t *testing.T) {
	ctx := context.Background()

	request := events.LambdaFunctionURLRequest{
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "POST",
				Path:   "/",
			},
		},
		RawPath: "/",
		Body:    `{"test":"data"}`,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}

	response, err := handler(ctx, request)
	if err != nil {
		t.Fatalf("Handler returned error: %v", err)
	}

	if response.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", response.StatusCode)
	}

	var body Response
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if body.Message != "Hello from Lambda Function URL!" {
		t.Errorf("Expected message 'Hello from Lambda Function URL!', got '%s'", body.Message)
	}
}

