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
	"regexp"
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
	epubBucket               = "epubs"
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

	// Generate and upload content.json and positions.json
	// Files are stored at {basePath}/readium/ (without ~ since Supabase doesn't allow it)
	// We use relative paths in manifest: readium/content.json (resolved relative to manifest)
	_, _, err = generateAndUploadReadiumFiles(publication, &manifest, resourceMap, basePath, supabaseURL, serviceKey)
	if err != nil {
		return "", fmt.Errorf("failed to generate and upload Readium files: %w", err)
	}

	// Generate manifest with Supabase URLs
	// Note: We use relative paths for content.json and positions.json since we store them
	// at readium/ (without ~) due to Supabase storage key restrictions
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
		if err := processResource(hrefStr, &link, pub, basePath, supabaseURL, serviceKey, resourceMap); err != nil {
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
				// Find the link in manifest for the base href
				baseLink := findLinkInManifest(baseHref, &manifest)
				if err := processResource(baseHref, baseLink, pub, basePath, supabaseURL, serviceKey, resourceMap); err != nil {
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
				// Find the link in manifest for the base href
				baseLink := findLinkInManifest(baseHref, &manifest)
				if err := processResource(baseHref, baseLink, pub, basePath, supabaseURL, serviceKey, resourceMap); err != nil {
					// Log but don't fail - some links might not be resources
					log.Printf("Warning: failed to process link resource %s: %v", baseHref, err)
				}
			}
		}
	}

	// Process resources
	for _, link := range manifest.Resources {
		hrefStr := link.Href.String()
		if err := processResource(hrefStr, &link, pub, basePath, supabaseURL, serviceKey, resourceMap); err != nil {
			return nil, fmt.Errorf("failed to process resource %s: %w", hrefStr, err)
		}
	}

	return resourceMap, nil
}

// findLinkInManifest finds a link in the manifest by href
func findLinkInManifest(href string, m *manifest.Manifest) *manifest.Link {
	// Check reading order
	for _, link := range m.ReadingOrder {
		if link.Href.String() == href {
			return &link
		}
	}
	// Check resources
	for _, link := range m.Resources {
		if link.Href.String() == href {
			return &link
		}
	}
	// Check table of contents
	for _, link := range m.TableOfContents {
		linkHref := link.Href.String()
		// Remove fragment for comparison
		if idx := strings.Index(linkHref, "#"); idx >= 0 {
			linkHref = linkHref[:idx]
		}
		if linkHref == href {
			return &link
		}
	}
	return nil
}

// processResource processes a single resource: reads it from publication and uploads to Supabase
func processResource(href string, manifestLink *manifest.Link, pub *pub.Publication, basePath, supabaseURL, serviceKey string, resourceMap map[string]string) error {
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
	if manifestLink != nil {
		link.MediaType = manifestLink.MediaType
	}
	resource := pub.Get(ctx, link)
	defer resource.Close()

	// Read all data from the resource using the Read method
	// Read(ctx, start, end) - when both are 0, the whole content is returned
	resourceData, resErr := resource.Read(ctx, 0, 0)
	if resErr != nil {
		return fmt.Errorf("failed to read resource: %v", resErr)
	}

	// Check if this is an XHTML/HTML file that needs link rewriting
	mediaType := link.MediaType
	isXHTML := false
	if mediaType != nil {
		mtStr := mediaType.String()
		if mtStr == "application/xhtml+xml" || mtStr == "text/html" {
			isXHTML = true
		}
	}
	// Fallback to file extension check
	if !isXHTML && (strings.HasSuffix(href, ".xhtml") || strings.HasSuffix(href, ".html")) {
		isXHTML = true
	}

	if isXHTML {
		// Rewrite internal links in XHTML/HTML files
		resourceData = rewriteLinksInXHTML(resourceData, href, resourceMap, basePath, supabaseURL)
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

// rewriteLinksInXHTML keeps relative hrefs relative - Thorium Reader resolves them against manifest base
func rewriteLinksInXHTML(content []byte, currentHref string, resourceMap map[string]string, basePath, supabaseURL string) []byte {
	// Convert content to string for regex processing
	contentStr := string(content)

	// Pattern to match href attributes in <a> tags
	// Matches: href="relative/path.xhtml#fragment" or href='relative/path.xhtml#fragment'
	hrefPattern := regexp.MustCompile(`(?i)(<a[^>]*\s+href=["'])([^"']+)(["'][^>]*>)`)

	// Replace function - we'll normalize relative paths but keep them relative
	modifiedContent := hrefPattern.ReplaceAllStringFunc(contentStr, func(match string) string {
		parts := hrefPattern.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match // Return original if pattern doesn't match
		}

		prefix := parts[1]    // <a ... href="
		hrefValue := parts[2] // the href value
		suffix := parts[3]    // " ...>

		// Skip external URLs, data URIs, mailto, same-page anchors
		if strings.HasPrefix(hrefValue, "http://") ||
			strings.HasPrefix(hrefValue, "https://") ||
			strings.HasPrefix(hrefValue, "mailto:") ||
			strings.HasPrefix(hrefValue, "data:") ||
			strings.HasPrefix(hrefValue, "#") {
			return match // Keep external/absolute links as-is
		}

		// For internal relative links, keep them relative
		// Thorium Reader will resolve them against the manifest base URL
		// Just normalize the path (remove ./ and handle .. if needed)
		normalized := normalizeRelativeLink(hrefValue)

		return prefix + normalized + suffix
	})

	return []byte(modifiedContent)
}

// normalizeRelativeLink normalizes a relative link path while keeping it relative
func normalizeRelativeLink(link string) string {
	// Remove leading ./
	link = strings.TrimPrefix(link, "./")
	// Remove leading / if present (make it truly relative)
	link = strings.TrimPrefix(link, "/")
	// For now, assume links are already correct relative paths
	// More complex normalization (handling ..) could be added if needed
	return link
}

// getDirectoryFromHref extracts the directory path from an href
func getDirectoryFromHref(href string) string {
	// Remove leading slash
	href = strings.TrimPrefix(href, "/")

	// Find last slash
	lastSlash := strings.LastIndex(href, "/")
	if lastSlash == -1 {
		return "" // No directory, just filename
	}

	return href[:lastSlash+1] // Include trailing slash
}

// resolveRelativePath resolves a relative path against a base directory
func resolveRelativePath(relativePath, baseDir string) string {
	// If relativePath is already absolute (starts with /), return as-is
	if strings.HasPrefix(relativePath, "/") {
		return strings.TrimPrefix(relativePath, "/")
	}

	// Combine baseDir and relativePath
	combined := baseDir + relativePath

	// Normalize the path (remove .. and .)
	parts := strings.Split(combined, "/")
	result := make([]string, 0, len(parts))

	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(result) > 0 {
				result = result[:len(result)-1]
			}
			continue
		}
		result = append(result, part)
	}

	return strings.Join(result, "/")
}

// convertTOCLink converts a TOC link (which may have children) to a map with relative paths
func convertTOCLink(link manifest.Link, resourceMap map[string]string, basePath, supabaseURL string) map[string]interface{} {
	hrefStr := link.Href.String()
	// Use relative path (relative to manifest.json location)
	// Remove leading slash if present to ensure it's a relative path
	relativeHref := strings.TrimPrefix(hrefStr, "/")

	item := map[string]interface{}{
		"href": relativeHref,
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

// generateAndUploadReadiumFiles generates content.json and positions.json and uploads them to Supabase
// Returns the Supabase URLs for these files so they can be referenced in the manifest
func generateAndUploadReadiumFiles(publication *pub.Publication, manifest *manifest.Manifest, resourceMap map[string]string, basePath, supabaseURL, serviceKey string) (contentURL, positionsURL string, err error) {
	// Generate positions.json
	positionsJSON, err := generatePositionsJSON(publication, manifest, resourceMap, basePath, supabaseURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate positions.json: %w", err)
	}

	// Upload positions.json to readium/ directory (without ~ since Supabase doesn't allow it in keys)
	// We'll use full URLs in manifest instead of ~readium/ paths
	positionsPath := fmt.Sprintf("%s/readium/positions.json", basePath)
	positionsURL, err = uploadToSupabase(positionsPath, positionsJSON, manifestBucket, supabaseURL, serviceKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to upload positions.json: %w", err)
	}

	// Generate content.json
	contentJSON, err := generateContentJSON(manifest, resourceMap, basePath, supabaseURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate content.json: %w", err)
	}

	// Upload content.json to readium/ directory
	contentPath := fmt.Sprintf("%s/readium/content.json", basePath)
	contentURL, err = uploadToSupabase(contentPath, contentJSON, manifestBucket, supabaseURL, serviceKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to upload content.json: %w", err)
	}

	return contentURL, positionsURL, nil
}

// generatePositionsJSON generates the positions.json file based on reading order and content length
// It calculates positions based on content length (approximately 1024 characters per position)
func generatePositionsJSON(publication *pub.Publication, manifest *manifest.Manifest, resourceMap map[string]string, basePath, supabaseURL string) ([]byte, error) {
	ctx := context.Background()
	positions := make([]map[string]interface{}, 0)
	
	const charsPerPosition = 1024 // Standard: one position per 1024 characters
	
	// First pass: calculate total character count and positions per resource
	type resourceInfo struct {
		href        string
		mediaType   string
		charCount   int
		numPositions int
	}
	
	resourceInfos := make([]resourceInfo, 0, len(manifest.ReadingOrder))
	totalChars := 0
	
	for i := range manifest.ReadingOrder {
		link := &manifest.ReadingOrder[i]
		hrefStr := link.Href.String()
		
		// Read the resource to get its content length
		// Use the link directly from reading order
		resource := publication.Get(ctx, *link)
		if resource == nil {
			log.Printf("Warning: failed to get resource %s", hrefStr)
			continue
		}
		
		// Read the resource content
		resourceData, err := resource.Read(ctx, 0, 0)
		resource.Close()
		
		if err != nil {
			log.Printf("Warning: failed to read resource %s: %v", hrefStr, err)
			continue
		}
		
		// Count characters (for text-based resources)
		charCount := len(string(resourceData))
		numPositions := 1 // At least one position
		if charCount > 0 {
			// Calculate number of positions: at least 1, or based on content length
			numPositions = (charCount + charsPerPosition - 1) / charsPerPosition // Ceiling division
			if numPositions == 0 {
				numPositions = 1
			}
		}
		
		mediaTypeStr := ""
		if link.MediaType != nil {
			mediaTypeStr = link.MediaType.String()
		}
		
		resourceInfos = append(resourceInfos, resourceInfo{
			href:        hrefStr,
			mediaType:   mediaTypeStr,
			charCount:   charCount,
			numPositions: numPositions,
		})
		
		totalChars += charCount
	}
	
	// Second pass: generate positions with proper progression values
	positionCounter := 1
	cumulativeChars := 0
	
	for _, info := range resourceInfos {
		relativeHref := strings.TrimPrefix(info.href, "/")
		
		// Generate positions for this resource
		for i := 0; i < info.numPositions; i++ {
			// Calculate progression within this document (0.0 to 1.0)
			var progression float64
			if info.numPositions > 1 {
				progression = float64(i) / float64(info.numPositions-1)
			} else {
				progression = 0.0
			}
			
			// Calculate totalProgression across entire publication
			var totalProgression float64
			if totalChars > 0 {
				// Estimate position in total content based on cumulative chars
				// Add proportional contribution for this position within the resource
				charsAtPosition := cumulativeChars + (i * info.charCount / info.numPositions)
				totalProgression = float64(charsAtPosition) / float64(totalChars)
				if totalProgression > 1.0 {
					totalProgression = 1.0
				}
			}
			
			position := map[string]interface{}{
				"href": relativeHref,
				"locations": map[string]interface{}{
					"position":         positionCounter,
					"progression":      progression,
					"totalProgression": totalProgression,
				},
			}
			
			// Add type if available
			if info.mediaType != "" {
				position["type"] = info.mediaType
			}
			
			positions = append(positions, position)
			positionCounter++
		}
		
		cumulativeChars += info.charCount
	}
	
	positionsData := map[string]interface{}{
		"total":    len(positions),
		"positions": positions,
	}
	
	return json.MarshalIndent(positionsData, "", "  ")
}

// buildContentItemFromLink converts a TOC link to a content item structure
func buildContentItemFromLink(link manifest.Link, resourceMap map[string]string, basePath, supabaseURL string) map[string]interface{} {
	hrefStr := link.Href.String()
	// Use relative path (relative to manifest.json location)
	// Remove leading slash if present to ensure it's a relative path
	relativeHref := strings.TrimPrefix(hrefStr, "/")

	item := map[string]interface{}{
		"href": relativeHref,
	}

	if link.Title != "" {
		item["title"] = link.Title
	}

	if link.MediaType != nil {
		item["type"] = link.MediaType.String()
	}

	// Recursively add children
	if len(link.Children) > 0 {
		children := make([]map[string]interface{}, 0, len(link.Children))
		for _, child := range link.Children {
			childItem := buildContentItemFromLink(child, resourceMap, basePath, supabaseURL)
			children = append(children, childItem)
		}
		if len(children) > 0 {
			item["children"] = children
		}
	}

	return item
}

// generateContentJSON generates the content.json file based on table of contents
func generateContentJSON(manifest *manifest.Manifest, resourceMap map[string]string, basePath, supabaseURL string) ([]byte, error) {
	content := make([]map[string]interface{}, 0)
	if len(manifest.TableOfContents) > 0 {
		for _, link := range manifest.TableOfContents {
			item := buildContentItemFromLink(link, resourceMap, basePath, supabaseURL)
			content = append(content, item)
		}
	} else {
		// If no TOC, use reading order as fallback
		for _, link := range manifest.ReadingOrder {
			hrefStr := link.Href.String()
			// Use relative path (relative to manifest.json location)
			// Remove leading slash if present to ensure it's a relative path
			relativeHref := strings.TrimPrefix(hrefStr, "/")

			item := map[string]interface{}{
				"href": relativeHref,
			}
			if link.Title != "" {
				item["title"] = link.Title
			}
			if link.MediaType != nil {
				item["type"] = link.MediaType.String()
			}
			content = append(content, item)
		}
	}

	contentData := map[string]interface{}{
		"metadata": map[string]interface{}{
			"numberOfItems": len(content),
		},
		"structure": content,
	}

	return json.MarshalIndent(contentData, "", "  ")
}

// generateManifestWithSupabaseURLs creates a new manifest with all URLs pointing to Supabase
func generateManifestWithSupabaseURLs(manifest *manifest.Manifest, resourceMap map[string]string, basePath, supabaseURL string) ([]byte, error) {
	// Construct manifest URL for self reference
	manifestPath := fmt.Sprintf("%s/manifest.json", basePath)
	manifestURL := fmt.Sprintf("%s/storage/v1/object/public/%s/%s", strings.TrimSuffix(supabaseURL, "/"), manifestBucket, manifestPath)

	// Create a new manifest structure with updated URLs
	updatedManifest := map[string]interface{}{
		"@context": "https://readium.org/webpub-manifest/context.jsonld",
		"metadata": manifest.Metadata,
	}

	// Update reading order with relative paths
	readingOrder := make([]map[string]interface{}, 0, len(manifest.ReadingOrder))
	for _, link := range manifest.ReadingOrder {
		hrefStr := link.Href.String()
		// Use relative path (relative to manifest.json location)
		// Remove leading slash if present to ensure it's a relative path
		relativeHref := strings.TrimPrefix(hrefStr, "/")

		item := map[string]interface{}{
			"href": relativeHref,
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

	// Extract landmarks from Links and TOC
	// Common landmark rels: "contents", "start", "copyright", etc.
	landmarkRels := map[string]bool{
		"contents":  true,
		"start":     true,
		"copyright": true,
	}
	landmarks := make([]map[string]interface{}, 0)
	landmarkHrefs := make(map[string]bool) // Track added landmarks to avoid duplicates

	// First, extract landmarks from manifest.Links
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
			// Use relative path (relative to manifest.json location)
			// Remove leading slash if present to ensure it's a relative path
			relativeHref := strings.TrimPrefix(hrefStr, "/")

			item := map[string]interface{}{
				"href": relativeHref,
			}
			if link.Title != "" {
				item["title"] = link.Title
			}
			landmarks = append(landmarks, item)
			landmarkHrefs[hrefStr] = true
		}
	}

	// Also check TOC for common landmark patterns (Table of Contents, Begin Reading, Copyright)
	if len(manifest.TableOfContents) > 0 {
		for _, link := range manifest.TableOfContents {
			hrefStr := link.Href.String()
			// Skip if already added
			if landmarkHrefs[hrefStr] {
				continue
			}

			title := strings.ToLower(link.Title)
			isLandmark := false
			landmarkTitle := ""

			// Check for common landmark titles
			if strings.Contains(title, "table of contents") || strings.Contains(title, "contents") || strings.Contains(title, "toc") {
				isLandmark = true
				landmarkTitle = "Table of Contents"
			} else if strings.Contains(title, "begin reading") || strings.Contains(title, "start") {
				isLandmark = true
				landmarkTitle = "Begin Reading"
			} else if strings.Contains(title, "copyright") {
				isLandmark = true
				landmarkTitle = "Copyright Page"
			}

			if isLandmark {
				// Use relative path (relative to manifest.json location)
				// Remove leading slash if present to ensure it's a relative path
				relativeHref := strings.TrimPrefix(hrefStr, "/")
				item := map[string]interface{}{
					"href": relativeHref,
				}
				if landmarkTitle != "" {
					item["title"] = landmarkTitle
				} else if link.Title != "" {
					item["title"] = link.Title
				}
				landmarks = append(landmarks, item)
				landmarkHrefs[hrefStr] = true
			}
		}
	}

	// If no landmarks found, try to infer from reading order (first item = Begin Reading)
	if len(landmarks) == 0 && len(manifest.ReadingOrder) > 0 {
		firstLink := manifest.ReadingOrder[0]
		hrefStr := firstLink.Href.String()
		// Use relative path (relative to manifest.json location)
		// Remove leading slash if present to ensure it's a relative path
		relativeHref := strings.TrimPrefix(hrefStr, "/")
		landmarks = append(landmarks, map[string]interface{}{
			"href":  relativeHref,
			"title": "Begin Reading",
		})
	}

	if len(landmarks) > 0 {
		updatedManifest["landmarks"] = landmarks
	}

	// Build links array - always include required Readium links
	links := make([]map[string]interface{}, 0)

	// Add self reference (required)
	links = append(links, map[string]interface{}{
		"href": manifestURL,
		"rel":  "self",
		"type": "application/webpub+json",
	})

	// Add Readium-specific links (content.json and positions.json)
	// Use relative paths: readium/content.json (without ~) since:
	// 1. Supabase doesn't allow ~ in storage keys, so we store at readium/
	// 2. Readers will resolve relative to manifest: {basePath}/readium/content.json
	// 3. This matches where we actually stored the files
	links = append(links, map[string]interface{}{
		"href": "readium/content.json",
		"type": "application/vnd.readium.content+json",
	})
	links = append(links, map[string]interface{}{
		"href": "readium/positions.json",
		"type": "application/vnd.readium.position-list+json",
	})

	// Extract license links from manifest.Links (they may have rel="http://creativecommons.org/ns#license")
	// We'll add these when processing manifest.Links below

	// Add non-landmark links from manifest.Links
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
		// Use relative paths for internal links, keep external URLs as-is
		if !strings.HasPrefix(hrefStr, "http://") && !strings.HasPrefix(hrefStr, "https://") && !strings.HasPrefix(hrefStr, "~") {
			// Use relative path (relative to manifest.json location)
			// Remove leading slash if present to ensure it's a relative path
			hrefStr = strings.TrimPrefix(hrefStr, "/")
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

	// Always include links array (required by Readium spec)
	updatedManifest["links"] = links

	// Update resources with relative paths
	resources := make([]map[string]interface{}, 0, len(manifest.Resources))
	for _, link := range manifest.Resources {
		hrefStr := link.Href.String()
		// Use relative path (relative to manifest.json location)
		// Remove leading slash if present to ensure it's a relative path
		relativeHref := strings.TrimPrefix(hrefStr, "/")

		item := map[string]interface{}{
			"href": relativeHref,
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

// getContentType determines the content type based on file extension and path
func getContentType(path string) string {
	pathLower := strings.ToLower(path)
	
	// Check for specific Readium JSON files first
	if strings.HasSuffix(pathLower, "manifest.json") {
		return "application/webpub+json; charset=utf-8"
	}
	if strings.HasSuffix(pathLower, "content.json") {
		return "application/vnd.readium.content+json; charset=utf-8"
	}
	if strings.HasSuffix(pathLower, "positions.json") {
		return "application/vnd.readium.position-list+json; charset=utf-8"
	}
	
	// Check file extension for other files
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".json":
		return "application/json; charset=utf-8"
	case ".html", ".htm":
		return "text/html"
	case ".xhtml":
		return "application/xhtml+xml"
	case ".css":
		return "text/css"
	case ".js":
		return "application/javascript"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".xml":
		return "application/xml"
	case ".ncx":
		return "application/x-dtbncx+xml"
	case ".opf":
		return "application/oebps-package+xml"
	default:
		return "application/octet-stream"
	}
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

	// Determine content type from file extension
	contentType := getContentType(path)

	// Set Supabase authentication headers
	req.Header.Set("apikey", serviceKey)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", serviceKey))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-upsert", "true") // Upsert to allow overwriting
	req.Header.Set("User-Agent", "Readium-Processor-Lambda/1.0")
	
	// Set Content-Disposition to inline for JSON files so browsers display them instead of downloading
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		req.Header.Set("Content-Disposition", "inline")
	}

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
