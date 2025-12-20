package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

type Response struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func handler(ctx context.Context, request events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	log.Printf("Received request: Method=%s, Path=%s", request.RequestContext.HTTP.Method, request.RawPath)

	// Create a simple response
	responseBody := Response{
		Message: "Hello from Lambda Function URL!",
		Status:  200,
	}

	body, err := json.Marshal(responseBody)
	if err != nil {
		log.Printf("Error marshaling response: %v", err)
		return events.LambdaFunctionURLResponse{
			StatusCode: 500,
			Body:       `{"error":"Internal server error"}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		}, nil
	}

	return events.LambdaFunctionURLResponse{
		StatusCode: 200,
		Body:       string(body),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, nil
}

func main() {
	lambda.Start(handler)
}
