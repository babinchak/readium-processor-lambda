package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/joho/godotenv"
)

type Response struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
	Data    any    `json:"data,omitempty"`
}

type ErrorResponse struct {
	Error  string `json:"error"`
	Status int    `json:"status"`
}

const (
	supabaseURLEnvVar        = "SUPABASE_URL"
	supabaseServiceKeyEnvVar = "SUPABASE_SERVICE_ROLE_KEY"
	epubBucket               = "epub-files"
)

func handler(ctx context.Context, request events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	log.Printf("Received request: Method=%s, Path=%s", request.RequestContext.HTTP.Method, request.RawPath)

	// Only allow POST requests since this operation mutates server state
	if request.RequestContext.HTTP.Method != "POST" {
		return createErrorResponse(405, "Method not allowed. This endpoint only accepts POST requests."), nil
	}

	// Get Supabase configuration from environment variables
	supabaseURL := os.Getenv(supabaseURLEnvVar)
	supabaseServiceKey := os.Getenv(supabaseServiceKeyEnvVar)

	if supabaseURL == "" {
		return createErrorResponse(500, "SUPABASE_URL environment variable is not set"), nil
	}

	if supabaseServiceKey == "" {
		return createErrorResponse(500, "SUPABASE_SERVICE_ROLE_KEY environment variable is not set"), nil
	}

	// Extract EPUB filename from request body
	var epubFilename string

	if request.Body != "" {
		var bodyData map[string]string
		if err := json.Unmarshal([]byte(request.Body), &bodyData); err == nil {
			if filename := bodyData["filename"]; filename != "" {
				epubFilename = filename
			}
		}
	}

	// Validate filename
	if epubFilename == "" {
		return createErrorResponse(400, "Missing 'filename' parameter. Provide EPUB filename in request body: {\"filename\":\"...\"}"), nil
	}

	// Sanitize filename (remove leading slashes, prevent path traversal)
	epubFilename = strings.TrimPrefix(epubFilename, "/")
	if strings.Contains(epubFilename, "..") {
		return createErrorResponse(400, "Invalid filename: path traversal not allowed"), nil
	}

	log.Printf("Processing EPUB file: %s", epubFilename)

	// Construct Supabase storage URL
	// Format: {SUPABASE_URL}/storage/v1/object/{bucket}/{filename}
	// Using authenticated endpoint with service role key (not public endpoint)
	storageURL := fmt.Sprintf("%s/storage/v1/object/%s/%s", strings.TrimSuffix(supabaseURL, "/"), epubBucket, epubFilename)

	log.Printf("Downloading EPUB from Supabase: %s", storageURL)

	// Download the EPUB file
	epubData, err := downloadEPUBFromSupabase(storageURL, supabaseServiceKey)
	if err != nil {
		log.Printf("Error downloading EPUB: %v", err)
		return createErrorResponse(500, fmt.Sprintf("Failed to download EPUB: %v", err)), nil
	}
	log.Printf("Successfully downloaded EPUB file (%d bytes)", len(epubData))

	// For now, just return success with file size
	// Later we'll process it with Readium toolkit
	responseBody := Response{
		Message: "EPUB file downloaded successfully",
		Status:  200,
		Data: map[string]interface{}{
			"file_size": len(epubData),
			"filename":  epubFilename,
		},
	}

	body, err := json.Marshal(responseBody)
	if err != nil {
		log.Printf("Error marshaling response: %v", err)
		return createErrorResponse(500, "Internal server error"), nil
	}

	return events.LambdaFunctionURLResponse{
		StatusCode: 200,
		Body:       string(body),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, nil
}

func downloadEPUBFromSupabase(storageURL, serviceKey string) ([]byte, error) {
	// Create HTTP client
	client := &http.Client{}

	// Create request
	req, err := http.NewRequest("GET", storageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set Supabase authentication headers
	req.Header.Set("apikey", serviceKey)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", serviceKey))
	req.Header.Set("User-Agent", "Readium-Processor-Lambda/1.0")

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code: %d, response: %s", resp.StatusCode, string(bodyBytes))
	}

	// Read response body
	epubData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Validate it's actually an EPUB (check for ZIP signature)
	if len(epubData) < 4 {
		return nil, fmt.Errorf("file too small to be a valid EPUB")
	}

	// EPUB files are ZIP archives, check for ZIP signature (PK\x03\x04)
	if epubData[0] != 'P' || epubData[1] != 'K' {
		return nil, fmt.Errorf("file does not appear to be a valid EPUB (missing ZIP signature)")
	}

	return epubData, nil
}

func createErrorResponse(statusCode int, message string) events.LambdaFunctionURLResponse {
	errorBody := ErrorResponse{
		Error:  message,
		Status: statusCode,
	}

	body, err := json.Marshal(errorBody)
	if err != nil {
		body = []byte(`{"error":"Internal server error","status":500}`)
	}

	return events.LambdaFunctionURLResponse{
		StatusCode: statusCode,
		Body:       string(body),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}
}

func init() {
	// Load .env file for local development and testing (ignores error if file doesn't exist)
	// In Lambda, environment variables are set directly, so this won't affect production
	// init() runs before main() and before tests, so .env will be loaded for both
	_ = godotenv.Load()
}

func main() {
	lambda.Start(handler)
}
