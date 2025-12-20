# Readium Processor Lambda

A Go-based AWS Lambda function that processes EPUB files using the Readium Go toolkit. This Lambda uses Function URL for direct HTTP access.

## Prerequisites

- Go 1.21 or later
- AWS CLI configured
- AWS SAM CLI (optional, for local testing)
- Supabase project with storage bucket configured

## Project Structure

```
.
├── main.go              # Lambda handler
├── main_test.go         # Integration tests
├── go.mod               # Go dependencies
├── build.ps1            # PowerShell build script (Windows)
├── Makefile             # Build commands (Linux/Mac)
└── README.md
```

## Building

The Lambda function is built for ARM64 architecture (Amazon Linux 2023):

### Windows (PowerShell):
```powershell
.\build.ps1 build
```

### Linux/Mac (Make):
```bash
make build
```

This will:
1. Compile the Go code for Linux ARM64
2. Create a `bootstrap` binary (required by Lambda)
3. Package it into `function.zip`

### Available build commands:

**PowerShell:**
- `.\build.ps1 build` - Build Lambda function
- `.\build.ps1 clean` - Clean build artifacts
- `.\build.ps1 deps` - Update dependencies

**Make (Linux/Mac):**
- `make build` - Build Lambda function
- `make clean` - Clean build artifacts
- `make deps` - Update dependencies

## Environment Variables

The Lambda function requires the following environment variables:

- `SUPABASE_URL` - Your Supabase project URL (e.g., `https://your-project.supabase.co`)
- `SUPABASE_SERVICE_ROLE_KEY` - Your Supabase service role key (for authenticated storage access)

### Local Testing

For local testing, you can use a `.env` file in the project root:

**Create `.env` file:**
```env
SUPABASE_URL=https://your-project.supabase.co
SUPABASE_SERVICE_ROLE_KEY=your-service-role-key
```

The `.env` file is automatically loaded when running locally. The `.env` file is gitignored, so your credentials won't be committed.

**Alternatively, you can set environment variables directly:**

**Windows (PowerShell):**
```powershell
$env:SUPABASE_URL="https://your-project.supabase.co"
$env:SUPABASE_SERVICE_ROLE_KEY="your-service-role-key"
```

**Linux/Mac:**
```bash
export SUPABASE_URL="https://your-project.supabase.co"
export SUPABASE_SERVICE_ROLE_KEY="your-service-role-key"
```

## API Usage

The Lambda accepts EPUB filenames (relative paths within the `epub-files` bucket) via:

**GET request with query parameter:**
```
GET https://your-lambda-url/?filename=path/to/file.epub
```

**POST request with JSON body:**
```json
POST https://your-lambda-url/
{
  "filename": "path/to/file.epub"
}
```

Example filename format: `c68d1328-863d-4e7a-92d9-ffbf0135d3dc/1e7ce2b0-3b17-419e-85a6-f849cd107602.epub`

The Lambda will:
1. Construct the Supabase storage URL from the filename
2. Download the EPUB file using the service role key for authentication
3. Validate the EPUB file format
4. Return a success response with file size information

## Deployment

### Using AWS CLI

1. Create the Lambda function:
```bash
aws lambda create-function \
  --function-name readium-processor \
  --runtime provided.al2023 \
  --architectures arm64 \
  --role arn:aws:iam::YOUR_ACCOUNT_ID:role/lambda-execution-role \
  --handler bootstrap \
  --zip-file fileb://function.zip \
  --environment Variables="{SUPABASE_URL=https://your-project.supabase.co,SUPABASE_SERVICE_ROLE_KEY=your-service-role-key}"
```

2. Create a Function URL:
```bash
aws lambda create-function-url-config \
  --function-name readium-processor \
  --auth-type NONE \
  --cors '{"AllowOrigins": ["*"], "AllowMethods": ["GET", "POST"], "AllowHeaders": ["*"]}'
```

3. Get the Function URL:
```bash
aws lambda get-function-url-config --function-name readium-processor
```

### Updating Environment Variables

To update environment variables after deployment:

```bash
aws lambda update-function-configuration \
  --function-name readium-processor \
  --environment Variables="{SUPABASE_URL=https://your-project.supabase.co,SUPABASE_SERVICE_ROLE_KEY=your-service-role-key}"
```

### Using Terraform

Example Terraform configuration:

```hcl
resource "aws_lambda_function" "readium_processor" {
  filename         = "function.zip"
  function_name    = "readium-processor"
  role            = aws_iam_role.lambda_exec.arn
  handler         = "bootstrap"
  runtime         = "provided.al2023"
  architectures   = ["arm64"]

  environment {
    variables = {
      SUPABASE_URL              = "https://your-project.supabase.co"
      SUPABASE_SERVICE_ROLE_KEY = var.supabase_service_role_key  # Use variable or secret
    }
  }
}

resource "aws_lambda_function_url" "readium_processor" {
  function_name      = aws_lambda_function.readium_processor.function_name
  authorization_type = "NONE"

  cors {
    allow_origins = ["*"]
    allow_methods = ["GET", "POST"]
    allow_headers = ["*"]
  }
}
```

**Note:** For sensitive values like `SUPABASE_SERVICE_ROLE_KEY`, consider using:
- Terraform variables with `sensitive = true`
- AWS Secrets Manager
- AWS Systems Manager Parameter Store

## Testing

Run the integration tests:

```bash
go test -v
```

The tests mock Lambda Function URL requests and verify the handler responses. You can add more test cases in `main_test.go` as needed.

### Debugging in VSCode

The project includes VSCode debug configurations. To debug:

1. **Install the Go extension** for VSCode (if not already installed)
2. **Set breakpoints** in your code (`main.go` or `main_test.go`)
3. **Open the Run and Debug panel** (Ctrl+Shift+D or Cmd+Shift+D)
4. **Select a debug configuration:**
   - **Debug All Tests** - Runs all tests with debugging
   - **Debug Current Test** - Debugs the test function name you have selected
   - **Debug TestHandler_GET** - Debugs only the GET test
   - **Debug TestHandler_POST** - Debugs only the POST test
5. **Press F5** or click the green play button

You can also right-click on a test function name and select "Debug Test" from the context menu.

## Next Steps

- [x] Add Supabase authentication
- [x] Add Supabase storage integration
- [ ] Integrate Readium Go toolkit
- [ ] Implement EPUB processing logic
- [ ] Generate manifest and reading order files
- [ ] Upload processed files back to Supabase storage

## Notes

- The Lambda uses `provided.al2023` runtime (custom runtime)
- Built for ARM64 architecture for cost efficiency
- Function URL provides direct HTTP access without API Gateway
- The handler uses `events.LambdaFunctionURLRequest` for Function URL requests

