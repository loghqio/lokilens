package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

var (
	services = []string{"payments", "orders", "users", "gateway", "inventory"}
	levels   = []string{"info", "warn", "error", "debug"}
	envs     = []string{"production", "staging"}

	logGroupPrefix = "/lokilens/"

	infoMessages = []string{
		"request completed successfully",
		"health check passed",
		"connection pool initialized",
		"cache hit for key",
		"processing batch of records",
		"scheduled task completed",
		"metrics exported",
		"configuration reloaded",
	}

	warnMessages = []string{
		"response time exceeded threshold",
		"connection pool nearing capacity",
		"retry attempt for failed request",
		"deprecated API endpoint called",
		"cache miss rate above threshold",
		"disk usage at 80 percent",
	}

	errorMessages = []string{
		"failed to process payment: connection refused",
		"database query timeout after 30s",
		"authentication failed for user",
		"upstream service returned 503",
		"out of memory: cannot allocate",
		"TLS handshake failed",
		"rate limit exceeded for client",
		"failed to serialize response",
		"null pointer exception in handler",
		"circuit breaker open for dependency",
	}

	endpoints = []string{
		"POST /api/v1/payments/charge",
		"GET /api/v1/orders",
		"POST /api/v1/users/login",
		"GET /api/v1/inventory/check",
		"POST /api/v1/gateway/proxy",
		"GET /api/v1/health",
		"PUT /api/v1/orders/status",
		"DELETE /api/v1/users/session",
	}
)

func main() {
	endpointURL := os.Getenv("CW_ENDPOINT_URL")
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	prefix := os.Getenv("CW_LOG_GROUP_PREFIX")
	if prefix != "" {
		logGroupPrefix = prefix
	}

	ctx := context.Background()

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	cwOpts := []func(*cloudwatchlogs.Options){}
	if endpointURL != "" {
		cwOpts = append(cwOpts, func(o *cloudwatchlogs.Options) {
			o.BaseEndpoint = aws.String(endpointURL)
		})
	}

	cw := cloudwatchlogs.NewFromConfig(awsCfg, cwOpts...)

	// Create log groups and streams
	for _, svc := range services {
		groupName := logGroupPrefix + svc
		createLogGroup(ctx, cw, groupName)
		createLogStream(ctx, cw, groupName, "loggen")
	}

	log.Printf("CloudWatch log generator starting (endpoint=%s, region=%s, prefix=%s)", endpointURL, region, logGroupPrefix)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	errorSpikeTicker := time.NewTicker(10 * time.Minute)
	defer errorSpikeTicker.Stop()

	var errorSpikeService string
	var errorSpikeUntil time.Time

	for {
		select {
		case <-errorSpikeTicker.C:
			errorSpikeService = services[rand.Intn(len(services))]
			errorSpikeUntil = time.Now().Add(2 * time.Minute)
			log.Printf("Error spike started for service=%s", errorSpikeService)

		case <-ticker.C:
			count := rand.Intn(6) + 3
			for range count {
				service := services[rand.Intn(len(services))]
				env := envs[rand.Intn(len(envs))]
				level := pickLevel(service, errorSpikeService, errorSpikeUntil)
				msg := pickMessage(level)
				traceID := fmt.Sprintf("%016x", rand.Int63())
				endpoint := endpoints[rand.Intn(len(endpoints))]
				duration := rand.Intn(2000) + 5
				statusCode := pickStatusCode(level)

				logLine := fmt.Sprintf(
					`{"timestamp":"%s","level":"%s","service":"%s","msg":"%s","trace_id":"%s","endpoint":"%s","duration_ms":%d,"status_code":%d,"env":"%s"}`,
					time.Now().Format(time.RFC3339Nano),
					level, service, msg, traceID, endpoint, duration, statusCode, env,
				)

				groupName := logGroupPrefix + service
				pushLogEvent(ctx, cw, groupName, "loggen", logLine)
			}
		}
	}
}

func createLogGroup(ctx context.Context, cw *cloudwatchlogs.Client, name string) {
	_, err := cw.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(name),
	})
	if err != nil {
		// Ignore AlreadyExistsException — idempotent
		log.Printf("CreateLogGroup %s: %v (may already exist)", name, err)
	} else {
		log.Printf("Created log group: %s", name)
	}
}

func createLogStream(ctx context.Context, cw *cloudwatchlogs.Client, group, stream string) {
	_, err := cw.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(group),
		LogStreamName: aws.String(stream),
	})
	if err != nil {
		log.Printf("CreateLogStream %s/%s: %v (may already exist)", group, stream, err)
	} else {
		log.Printf("Created log stream: %s/%s", group, stream)
	}
}

func pushLogEvent(ctx context.Context, cw *cloudwatchlogs.Client, group, stream, message string) {
	_, err := cw.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName:  aws.String(group),
		LogStreamName: aws.String(stream),
		LogEvents: []types.InputLogEvent{
			{
				Message:   aws.String(message),
				Timestamp: aws.Int64(time.Now().UnixMilli()),
			},
		},
	})
	if err != nil {
		log.Printf("PutLogEvents %s: %v", group, err)
	}
}

func pickLevel(service, errorSpikeService string, spikeUntil time.Time) string {
	if service == errorSpikeService && time.Now().Before(spikeUntil) {
		roll := rand.Intn(100)
		if roll < 60 {
			return "error"
		}
		if roll < 80 {
			return "warn"
		}
		return "info"
	}

	roll := rand.Intn(100)
	switch {
	case roll < 60:
		return "info"
	case roll < 75:
		return "debug"
	case roll < 90:
		return "warn"
	default:
		return "error"
	}
}

func pickMessage(level string) string {
	switch level {
	case "error":
		return errorMessages[rand.Intn(len(errorMessages))]
	case "warn":
		return warnMessages[rand.Intn(len(warnMessages))]
	default:
		return infoMessages[rand.Intn(len(infoMessages))]
	}
}

func pickStatusCode(level string) int {
	switch level {
	case "error":
		codes := []int{500, 502, 503, 504, 408, 429}
		return codes[rand.Intn(len(codes))]
	case "warn":
		codes := []int{200, 201, 429, 408}
		return codes[rand.Intn(len(codes))]
	default:
		return 200
	}
}
