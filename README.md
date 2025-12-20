# Readium Processor Lambda

A Go-based AWS Lambda function that processes EPUB files using the Readium Go toolkit. This Lambda uses Function URL for direct HTTP access.

## Prerequisites

- Go 1.21 or later
- AWS CLI configured
- AWS SAM CLI (optional, for local testing)

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
  --zip-file fileb://function.zip
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

### Using Terraform (optional)

You can also use Terraform or AWS CDK for infrastructure as code.

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

- [ ] Integrate Readium Go toolkit
- [ ] Add Supabase authentication
- [ ] Implement EPUB processing logic
- [ ] Add Supabase storage integration

## Notes

- The Lambda uses `provided.al2023` runtime (custom runtime)
- Built for ARM64 architecture for cost efficiency
- Function URL provides direct HTTP access without API Gateway
- The handler uses `events.LambdaFunctionURLRequest` for Function URL requests

