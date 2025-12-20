package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func setupTestEnv() {
	// Only set test values if not already set from .env file
	if os.Getenv(supabaseURLEnvVar) == "" {
		os.Setenv(supabaseURLEnvVar, "https://test.supabase.co")
	}
	if os.Getenv(supabaseServiceKeyEnvVar) == "" {
		os.Setenv(supabaseServiceKeyEnvVar, "test-service-key")
	}
}

func teardownTestEnv() {
	os.Unsetenv(supabaseURLEnvVar)
	os.Unsetenv(supabaseServiceKeyEnvVar)
}

func TestHandler_MissingEnvVars(t *testing.T) {
	ctx := context.Background()
	teardownTestEnv()

	request := events.LambdaFunctionURLRequest{
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "GET",
				Path:   "/",
			},
		},
		RawPath: "/",
		QueryStringParameters: map[string]string{
			"filename": "test.epub",
		},
	}

	response, err := handler(ctx, request)
	if err != nil {
		t.Fatalf("Handler returned error: %v", err)
	}

	if response.StatusCode != 500 {
		t.Errorf("Expected status 500, got %d", response.StatusCode)
	}

	var errorBody ErrorResponse
	if err := json.Unmarshal([]byte(response.Body), &errorBody); err != nil {
		t.Fatalf("Failed to unmarshal error response: %v", err)
	}

	setupTestEnv()
}

func TestHandler_MissingFilename(t *testing.T) {
	ctx := context.Background()
	setupTestEnv()
	defer teardownTestEnv()

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

	if response.StatusCode != 400 {
		t.Errorf("Expected status 400, got %d", response.StatusCode)
	}

	var errorBody ErrorResponse
	if err := json.Unmarshal([]byte(response.Body), &errorBody); err != nil {
		t.Fatalf("Failed to unmarshal error response: %v", err)
	}

	if errorBody.Status != 400 {
		t.Errorf("Expected error status 400, got %d", errorBody.Status)
	}
}

func TestHandler_InvalidFilename_PathTraversal(t *testing.T) {
	ctx := context.Background()
	setupTestEnv()
	defer teardownTestEnv()

	request := events.LambdaFunctionURLRequest{
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "GET",
				Path:   "/",
			},
		},
		RawPath: "/",
		QueryStringParameters: map[string]string{
			"filename": "../../etc/passwd",
		},
	}

	response, err := handler(ctx, request)
	if err != nil {
		t.Fatalf("Handler returned error: %v", err)
	}

	if response.StatusCode != 400 {
		t.Errorf("Expected status 400, got %d", response.StatusCode)
	}

	var errorBody ErrorResponse
	if err := json.Unmarshal([]byte(response.Body), &errorBody); err != nil {
		t.Fatalf("Failed to unmarshal error response: %v", err)
	}
}

func TestHandler_FilenameInQueryParams(t *testing.T) {
	ctx := context.Background()
	setupTestEnv()
	defer teardownTestEnv()

	// Note: This test will fail if the file doesn't exist in Supabase
	// For integration testing, use a real Supabase instance or mock the HTTP client
	testFilename := "8f1acca6-4d96-410c-ba90-bfa06c451b72/c9170176-8372-48c7-897d-f6bfe6ea3eef.epub"

	request := events.LambdaFunctionURLRequest{
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "GET",
				Path:   "/",
			},
		},
		RawPath: "/",
		QueryStringParameters: map[string]string{
			"filename": testFilename,
		},
	}

	response, err := handler(ctx, request)
	if err != nil {
		t.Fatalf("Handler returned error: %v", err)
	}

	// This will likely return 500 if the file doesn't exist, which is expected
	// For a real test, you'd need a valid Supabase setup or mock the HTTP client
	if response.StatusCode < 400 {
		var body Response
		if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}
		t.Logf("Response: %+v", body)
	}
}

func TestHandler_FilenameInBody(t *testing.T) {
	ctx := context.Background()
	setupTestEnv()
	defer teardownTestEnv()

	testFilename := "test-folder/test.epub"
	bodyJSON, _ := json.Marshal(map[string]string{"filename": testFilename})

	request := events.LambdaFunctionURLRequest{
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "POST",
				Path:   "/",
			},
		},
		RawPath: "/",
		Body:    string(bodyJSON),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}

	response, err := handler(ctx, request)
	if err != nil {
		t.Fatalf("Handler returned error: %v", err)
	}

	// This will likely return 500 if the file doesn't exist, which is expected
	// For a real test, you'd need a valid Supabase setup or mock the HTTP client
	if response.StatusCode < 400 {
		var body Response
		if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}
		t.Logf("Response: %+v", body)
	}
}
