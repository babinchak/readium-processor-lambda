package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/joho/godotenv"
	"github.com/readium/go-toolkit/pkg/archive"
	"github.com/readium/go-toolkit/pkg/asset"
	"github.com/readium/go-toolkit/pkg/fetcher"
	"github.com/readium/go-toolkit/pkg/manifest"
	"github.com/readium/go-toolkit/pkg/mediatype"
	"github.com/readium/go-toolkit/pkg/parser/epub"
	"github.com/readium/go-toolkit/pkg/pub"
	"github.com/readium/go-toolkit/pkg/util/url"
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
	manifestBucket           = "readium-manifests"
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

	// Process EPUB with Readium toolkit
	manifestURL, err := processEPUB(epubData, epubFilename, supabaseURL, supabaseServiceKey)
	if err != nil {
		log.Printf("Error processing EPUB: %v", err)
		return createErrorResponse(500, fmt.Sprintf("Failed to process EPUB: %v", err)), nil
	}

	responseBody := Response{
		Message: "EPUB processed successfully",
		Status:  200,
		Data: map[string]interface{}{
			"manifest_url": manifestURL,
			"filename":     epubFilename,
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

// processEPUB processes an EPUB file using the Readium toolkit, extracts resources,
// uploads them to Supabase, and generates a manifest with Supabase URLs
func processEPUB(epubData []byte, epubFilename, supabaseURL, serviceKey string) (string, error) {
	ctx := context.Background()

	// Create a zip.Reader from the EPUB bytes
	zipReader, err := zip.NewReader(bytes.NewReader(epubData), int64(len(epubData)))
	if err != nil {
		return "", fmt.Errorf("failed to create zip reader: %w", err)
	}
	if zipReader == nil {
		return "", fmt.Errorf("zip.NewReader returned nil")
	}

	// Create an archive from the zip reader
	epubArchive := archive.NewGoZIPArchive(zipReader, func() error { return nil }, false)
	if epubArchive == nil {
		return "", fmt.Errorf("NewGoZIPArchive returned nil")
	}

	// Create a fetcher from the archive
	assetFetcher := fetcher.NewArchiveFetcher(epubArchive)
	if assetFetcher == nil {
		return "", fmt.Errorf("NewArchiveFetcher returned nil")
	}

	// Create a custom asset that uses our archive fetcher
	// The parser needs an asset, but we'll make it use our fetcher
	epubAsset := &bytesAsset{
		name:      epubFilename,
		mediaType: "application/epub+zip",
		fetcher:   assetFetcher,
	}

	// Create EPUB parser
	parser := epub.NewParser(nil)

	// Parse the EPUB - pass the fetcher directly
	// The parser may use the fetcher parameter if provided, otherwise it calls CreateFetcher on the asset
	builder, err := parser.Parse(ctx, epubAsset, assetFetcher)
	if err != nil {
		return "", fmt.Errorf("failed to parse EPUB: %w", err)
	}
	if builder == nil {
		return "", fmt.Errorf("parser returned nil builder")
	}

	// Build the publication
	publication := builder.Build()
	if publication == nil {
		return "", fmt.Errorf("builder.Build() returned nil publication")
	}

	// Get the manifest (it's a field, not a method)
	manifest := publication.Manifest

	// Extract base path from EPUB filename (without extension)
	basePath := strings.TrimSuffix(epubFilename, filepath.Ext(epubFilename))
	// Replace any path separators with underscores for the storage path
	basePath = strings.ReplaceAll(basePath, "/", "_")
	basePath = strings.ReplaceAll(basePath, "\\", "_")

	// Extract and upload all resources
	resourceMap, err := extractAndUploadResources(publication, basePath, supabaseURL, serviceKey)
	if err != nil {
		return "", fmt.Errorf("failed to extract and upload resources: %w", err)
	}

	// Generate manifest with Supabase URLs
	manifestJSON, err := generateManifestWithSupabaseURLs(&manifest, resourceMap, basePath, supabaseURL)
	if err != nil {
		return "", fmt.Errorf("failed to generate manifest: %w", err)
	}

	// Upload manifest to Supabase
	manifestPath := fmt.Sprintf("%s/manifest.json", basePath)
	manifestURL, err := uploadToSupabase(manifestPath, manifestJSON, manifestBucket, supabaseURL, serviceKey)
	if err != nil {
		return "", fmt.Errorf("failed to upload manifest: %w", err)
	}

	return manifestURL, nil
}

// extractAndUploadResources extracts all resources from the publication and uploads them to Supabase
func extractAndUploadResources(pub *pub.Publication, basePath, supabaseURL, serviceKey string) (map[string]string, error) {
	resourceMap := make(map[string]string)
	manifest := pub.Manifest

	// Process reading order items
	for _, link := range manifest.ReadingOrder {
		hrefStr := link.Href.String()
		if err := processResource(hrefStr, pub, basePath, supabaseURL, serviceKey, resourceMap); err != nil {
			return nil, fmt.Errorf("failed to process reading order resource %s: %w", hrefStr, err)
		}
	}

	// Process table of contents items (need to extract base hrefs without fragments)
	if len(manifest.TableOfContents) > 0 {
		for _, link := range manifest.TableOfContents {
			hrefStr := link.Href.String()
			// Extract base href without fragment
			baseHref := hrefStr
			if idx := strings.Index(hrefStr, "#"); idx >= 0 {
				baseHref = hrefStr[:idx]
			}
			if baseHref != "" {
				if err := processResource(baseHref, pub, basePath, supabaseURL, serviceKey, resourceMap); err != nil {
					return nil, fmt.Errorf("failed to process TOC resource %s: %w", baseHref, err)
				}
			}
		}
	}

	// Process links (which may include landmarks or other navigation links)
	// Extract base hrefs without fragments for any links that point to resources
	for _, link := range manifest.Links {
		hrefStr := link.Href.String()
		// Only process links that look like they point to resources (not external URLs)
		if !strings.HasPrefix(hrefStr, "http://") && !strings.HasPrefix(hrefStr, "https://") && !strings.HasPrefix(hrefStr, "~") {
			// Extract base href without fragment
			baseHref := hrefStr
			if idx := strings.Index(hrefStr, "#"); idx >= 0 {
				baseHref = hrefStr[:idx]
			}
			if baseHref != "" {
				if err := processResource(baseHref, pub, basePath, supabaseURL, serviceKey, resourceMap); err != nil {
					// Log but don't fail - some links might not be resources
					log.Printf("Warning: failed to process link resource %s: %v", baseHref, err)
				}
			}
		}
	}

	// Process resources
	for _, link := range manifest.Resources {
		hrefStr := link.Href.String()
		if err := processResource(hrefStr, pub, basePath, supabaseURL, serviceKey, resourceMap); err != nil {
			return nil, fmt.Errorf("failed to process resource %s: %w", hrefStr, err)
		}
	}

	return resourceMap, nil
}

// processResource processes a single resource: reads it from publication and uploads to Supabase
func processResource(href string, pub *pub.Publication, basePath, supabaseURL, serviceKey string, resourceMap map[string]string) error {
	// Skip if already processed
	if _, exists := resourceMap[href]; exists {
		return nil
	}

	// Create context for the operation
	ctx := context.Background()

	// Create HREF from string
	hrefURL, err := url.URLFromString(href)
	if err != nil {
		return fmt.Errorf("failed to create HREF from %s: %w", href, err)
	}

	// Read resource from publication using the fetcher
	link := manifest.Link{Href: manifest.NewHREF(hrefURL)}
	resource := pub.Get(ctx, link)
	defer resource.Close()

	// Read all data from the resource using the Read method
	// Read(ctx, start, end) - when both are 0, the whole content is returned
	resourceData, resErr := resource.Read(ctx, 0, 0)
	if resErr != nil {
		return fmt.Errorf("failed to read resource: %v", resErr)
	}

	// Create storage path: basePath/resourcePath
	// Normalize the href to handle relative paths
	storagePath := fmt.Sprintf("%s/%s", basePath, strings.TrimPrefix(href, "/"))

	// Upload to Supabase
	resourceURL, err := uploadToSupabase(storagePath, resourceData, manifestBucket, supabaseURL, serviceKey)
	if err != nil {
		return fmt.Errorf("failed to upload resource: %w", err)
	}

	// Store mapping from original href to Supabase URL
	resourceMap[href] = resourceURL

	return nil
}

// convertLinkToSupabaseURL converts a link href to a Supabase URL, handling fragments
func convertLinkToSupabaseURL(hrefStr string, resourceMap map[string]string, basePath, supabaseURL string) string {
	// Split href into base path and fragment
	baseHref := hrefStr
	fragment := ""
	if idx := strings.Index(hrefStr, "#"); idx >= 0 {
		baseHref = hrefStr[:idx]
		fragment = hrefStr[idx:]
	}

	// Get the base URL from resource map
	supabaseResourceURL := resourceMap[baseHref]
	if supabaseResourceURL == "" {
		// Fallback: construct URL if not in map
		storagePath := fmt.Sprintf("%s/%s", basePath, strings.TrimPrefix(baseHref, "/"))
		supabaseResourceURL = fmt.Sprintf("%s/storage/v1/object/public/%s/%s", strings.TrimSuffix(supabaseURL, "/"), manifestBucket, storagePath)
	}

	// Append fragment if present
	return supabaseResourceURL + fragment
}

// convertTOCLink converts a TOC link (which may have children) to a map with Supabase URLs
func convertTOCLink(link manifest.Link, resourceMap map[string]string, basePath, supabaseURL string) map[string]interface{} {
	hrefStr := link.Href.String()
	supabaseURLWithFragment := convertLinkToSupabaseURL(hrefStr, resourceMap, basePath, supabaseURL)

	item := map[string]interface{}{
		"href": supabaseURLWithFragment,
	}
	if link.Title != "" {
		item["title"] = link.Title
	}

	// Handle nested TOC entries (children) - recursively convert them
	if len(link.Children) > 0 {
		children := make([]map[string]interface{}, 0, len(link.Children))
		for _, child := range link.Children {
			childItem := convertTOCLink(child, resourceMap, basePath, supabaseURL)
			children = append(children, childItem)
		}
		if len(children) > 0 {
			item["children"] = children
		}
	}

	return item
}

// generateManifestWithSupabaseURLs creates a new manifest with all URLs pointing to Supabase
func generateManifestWithSupabaseURLs(manifest *manifest.Manifest, resourceMap map[string]string, basePath, supabaseURL string) ([]byte, error) {
	// Create a new manifest structure with updated URLs
	updatedManifest := map[string]interface{}{
		"@context": "https://readium.org/webpub-manifest/context.jsonld",
		"metadata": manifest.Metadata,
	}

	// Update reading order with Supabase URLs
	readingOrder := make([]map[string]interface{}, 0, len(manifest.ReadingOrder))
	for _, link := range manifest.ReadingOrder {
		hrefStr := link.Href.String()
		supabaseResourceURL := resourceMap[hrefStr]
		if supabaseResourceURL == "" {
			// Fallback: construct URL if not in map
			storagePath := fmt.Sprintf("%s/%s", basePath, strings.TrimPrefix(hrefStr, "/"))
			supabaseResourceURL = fmt.Sprintf("%s/storage/v1/object/public/%s/%s", strings.TrimSuffix(supabaseURL, "/"), manifestBucket, storagePath)
		}

		item := map[string]interface{}{
			"href": supabaseResourceURL,
		}
		if link.MediaType != nil {
			item["type"] = link.MediaType.String()
		}
		if link.Title != "" {
			item["title"] = link.Title
		}
		readingOrder = append(readingOrder, item)
	}
	updatedManifest["readingOrder"] = readingOrder

	// Update table of contents with Supabase URLs
	if len(manifest.TableOfContents) > 0 {
		toc := make([]map[string]interface{}, 0, len(manifest.TableOfContents))
		for _, link := range manifest.TableOfContents {
			tocItem := convertTOCLink(link, resourceMap, basePath, supabaseURL)
			toc = append(toc, tocItem)
		}
		if len(toc) > 0 {
			updatedManifest["toc"] = toc
		}
	}

	// Extract landmarks from Links (links with specific rel values that indicate landmarks)
	// Common landmark rels: "contents", "start", "copyright", etc.
	landmarkRels := map[string]bool{
		"contents":  true,
		"start":     true,
		"copyright": true,
	}
	landmarks := make([]map[string]interface{}, 0)
	for _, link := range manifest.Links {
		// Check if this link has a rel that indicates it's a landmark
		isLandmark := false
		for _, rel := range link.Rels {
			if landmarkRels[rel] {
				isLandmark = true
				break
			}
		}
		// Also check if it's a landmark by title pattern (some EPUBs don't use rels)
		if !isLandmark && (link.Title == "Table of Contents" || link.Title == "Begin Reading" || link.Title == "Copyright Page") {
			isLandmark = true
		}

		if isLandmark {
			hrefStr := link.Href.String()
			supabaseURLWithFragment := convertLinkToSupabaseURL(hrefStr, resourceMap, basePath, supabaseURL)

			item := map[string]interface{}{
				"href": supabaseURLWithFragment,
			}
			if link.Title != "" {
				item["title"] = link.Title
			}
			landmarks = append(landmarks, item)
		}
	}
	if len(landmarks) > 0 {
		updatedManifest["landmarks"] = landmarks
	}

	// Update links with Supabase URLs (for non-landmark links)
	links := make([]map[string]interface{}, 0, len(manifest.Links))
	for _, link := range manifest.Links {
		// Skip links that are landmarks (already added above)
		isLandmark := false
		for _, rel := range link.Rels {
			if landmarkRels[rel] {
				isLandmark = true
				break
			}
		}
		if isLandmark {
			continue
		}

		hrefStr := link.Href.String()
		// Only convert internal links to Supabase URLs
		if !strings.HasPrefix(hrefStr, "http://") && !strings.HasPrefix(hrefStr, "https://") && !strings.HasPrefix(hrefStr, "~") {
			supabaseURLWithFragment := convertLinkToSupabaseURL(hrefStr, resourceMap, basePath, supabaseURL)
			hrefStr = supabaseURLWithFragment
		}

		item := map[string]interface{}{
			"href": hrefStr,
		}
		if link.MediaType != nil {
			item["type"] = link.MediaType.String()
		}
		if len(link.Rels) > 0 {
			if len(link.Rels) == 1 {
				item["rel"] = link.Rels[0]
			} else {
				item["rel"] = link.Rels
			}
		}
		links = append(links, item)
	}
	if len(links) > 0 {
		updatedManifest["links"] = links
	}

	// Update resources with Supabase URLs
	resources := make([]map[string]interface{}, 0, len(manifest.Resources))
	for _, link := range manifest.Resources {
		hrefStr := link.Href.String()
		supabaseResourceURL := resourceMap[hrefStr]
		if supabaseResourceURL == "" {
			// Fallback: construct URL if not in map
			storagePath := fmt.Sprintf("%s/%s", basePath, strings.TrimPrefix(hrefStr, "/"))
			supabaseResourceURL = fmt.Sprintf("%s/storage/v1/object/public/%s/%s", strings.TrimSuffix(supabaseURL, "/"), manifestBucket, storagePath)
		}

		item := map[string]interface{}{
			"href": supabaseResourceURL,
		}
		if link.MediaType != nil {
			item["type"] = link.MediaType.String()
		}

		// Add rel="contents" for TOC resources
		if strings.Contains(hrefStr, "toc.xhtml") || strings.Contains(hrefStr, "toc.ncx") {
			item["rel"] = "contents"
		}

		// Include any existing rel values from the link
		if len(link.Rels) > 0 {
			rels := make([]string, 0, len(link.Rels))
			for _, rel := range link.Rels {
				rels = append(rels, rel)
			}
			if len(rels) == 1 {
				item["rel"] = rels[0]
			} else if len(rels) > 1 {
				item["rel"] = rels
			}
		}

		resources = append(resources, item)
	}
	if len(resources) > 0 {
		updatedManifest["resources"] = resources
	}

	// Marshal to JSON
	manifestJSON, err := json.MarshalIndent(updatedManifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal manifest: %w", err)
	}

	return manifestJSON, nil
}

// uploadToSupabase uploads data to Supabase storage
func uploadToSupabase(path string, data []byte, bucket, supabaseURL, serviceKey string) (string, error) {
	// Construct upload URL
	uploadURL := fmt.Sprintf("%s/storage/v1/object/%s/%s", strings.TrimSuffix(supabaseURL, "/"), bucket, path)

	// Create HTTP client
	client := &http.Client{}

	// Create request
	req, err := http.NewRequest("POST", uploadURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set Supabase authentication headers
	req.Header.Set("apikey", serviceKey)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", serviceKey))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("x-upsert", "true") // Upsert to allow overwriting
	req.Header.Set("User-Agent", "Readium-Processor-Lambda/1.0")

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	// Check status code (Supabase returns 200 for successful uploads)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected status code: %d, response: %s", resp.StatusCode, string(bodyBytes))
	}

	// Construct public URL
	publicURL := fmt.Sprintf("%s/storage/v1/object/public/%s/%s", strings.TrimSuffix(supabaseURL, "/"), bucket, path)
	return publicURL, nil
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

// bytesAsset implements asset.PublicationAsset for in-memory EPUB data
type bytesAsset struct {
	name      string
	mediaType string
	fetcher   fetcher.Fetcher
}

func (a *bytesAsset) Name() string {
	return a.name
}

func (a *bytesAsset) MediaType(ctx context.Context) mediatype.MediaType {
	mt, _ := mediatype.New(a.mediaType, "", "")
	return mt
}

func (a *bytesAsset) CreateFetcher(ctx context.Context, dependencies asset.Dependencies, credentials string) (fetcher.Fetcher, error) {
	return a.fetcher, nil
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
