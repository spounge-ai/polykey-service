package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/spounge-ai/polykey-service/internal/config"
	"github.com/spounge-ai/polykey-service/test/utils"
	cmn "github.com/spounge-ai/spounge-proto/gen/go/common/v2"
	pk "github.com/spounge-ai/spounge-proto/gen/go/polykey/v2"
	"google.golang.org/protobuf/types/known/structpb"
)

type Client struct {
	conn   *grpc.ClientConn
	client pk.PolykeyServiceClient
	logger *slog.Logger
}

func NewClient(cfg *config.Config, logger *slog.Logger) (*Client, error) {
	conn, err := createGRPCConnection(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection: %w", err)
	}

	return &Client{
		conn:   conn,
		client: pk.NewPolykeyServiceClient(conn),
		logger: logger,
	}, nil
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *Client) ExecuteTool(ctx context.Context, req *pk.ExecuteToolRequest) (*pk.ExecuteToolResponse, error) {
	c.logger.Info("Executing tool",
		"tool_name", req.ToolName,
		"secret_id", req.SecretId,  
		"has_metadata", req.Metadata != nil, 
	)

	resp, err := c.client.ExecuteTool(ctx, req)
	if err != nil {
		if grpcErr, ok := status.FromError(err); ok {
			c.logger.Error("gRPC call failed",
				"code", grpcErr.Code(),
				"message", grpcErr.Message(),
				"details", grpcErr.Details(),
			)
		}
		return nil, fmt.Errorf("ExecuteTool failed: %w", err)
	}

	c.logResponse(resp)
	return resp, nil
}


func (c *Client) logResponse(resp *pk.ExecuteToolResponse) {
	if resp.Status != nil {
		c.logger.Info("Tool execution completed",
			"status_code", resp.Status.Code,
			"status_message", resp.Status.Message,
		)
	}

	// Log output based on type
	switch output := resp.Output.(type) {
	case *pk.ExecuteToolResponse_StringOutput:
		c.logger.Info("Received string output",
			"output_length", len(output.StringOutput),
			"output_preview", truncateString(output.StringOutput, 100),
		)
	case *pk.ExecuteToolResponse_StructOutput:
		c.logger.Info("Received struct output",
			"field_count", len(output.StructOutput.AsMap()),
		)
	case *pk.ExecuteToolResponse_FileOutput:
		c.logger.Info("Received file output",
			"file_name", output.FileOutput.FileName,
			"mime_type", output.FileOutput.MimeType,
			"size_bytes", len(output.FileOutput.Content),
		)
	default:
		c.logger.Warn("No output returned")
	}
}

func main() {
	logBuffer := new(bytes.Buffer)
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		logger.Info("Received shutdown signal")
		cancel()
	}()

	if err := run(ctx, logger); err != nil {
		logger.Error("Application failed", "error", err)
	}

	logLines := strings.Split(logBuffer.String(), "\n")
	utils.PrintJestReport(logLines)
}

func run(ctx context.Context, logger *slog.Logger) error {
	logger.Info("Starting polykey client...")

	loader := config.NewConfigLoader()
	cfg, err := loader.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	logger.Info("Configuration loaded",
		"runtime", loader.Detector.DetectRuntime(),
		"server", cfg.ServerAddress,
	)

	if err := testNetworkConnection(ctx, cfg, logger); err != nil {
		return fmt.Errorf("network test failed: %w", err)
	}

	client, err := NewClient(cfg, logger)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			logger.Error("Failed to close client", "error", closeErr)
		}
	}()

	if err := executeTestRequest(ctx, client, logger); err != nil {
		return fmt.Errorf("test request failed: %w", err)
	}

	return nil
}

func testNetworkConnection(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("Testing network connectivity...")
	
	tester := config.NewNetworkTester()
	if err := tester.TestConnection(ctx, cfg.ServerAddress); err != nil {
		return err
	}
	
	logger.Info("Network connectivity test passed")
	return nil
}

func createGRPCConnection(cfg *config.Config, logger *slog.Logger) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(4*1024*1024),
			grpc.MaxCallSendMsgSize(4*1024*1024),
		),
	}

	logger.Info("Creating gRPC connection", "server", cfg.ServerAddress)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	conn, err := grpc.NewClient(cfg.ServerAddress, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client: %w", err)
	}

	if err := waitForConnection(ctx, conn, logger); err != nil {
		if cerr := conn.Close(); cerr != nil {
			logger.Warn("failed to close connection", "error", cerr)
		}
		return nil, fmt.Errorf("connection failed: %w", err)
	}

	logger.Info("gRPC connection established successfully")
	return conn, nil
}

func waitForConnection(ctx context.Context, conn *grpc.ClientConn, logger *slog.Logger) error {
	state := conn.GetState()
	logger.Debug("Initial connection state", "state", state)

	if state == connectivity.Idle {
		conn.Connect()
	}

	for state != connectivity.Ready {
		if !conn.WaitForStateChange(ctx, state) {
			return fmt.Errorf("connection timeout")
		}
		
		state = conn.GetState()
		logger.Debug("Connection state changed", "state", state)
		
		if state == connectivity.TransientFailure || state == connectivity.Shutdown {
			return fmt.Errorf("connection failed with state: %v", state)
		}
	}

	return nil
}

func executeTestRequest(ctx context.Context, client *Client, logger *slog.Logger) error {
	params, err := structpb.NewStruct(map[string]any{
		"example_param": "value",
		"timestamp":     time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("failed to create request parameters: %w", err)
	}

	req := &pk.ExecuteToolRequest{
		ToolName:   "example_tool",
		Parameters: params,
		SecretId:   stringPtr("secret-123"), // Changed from UserId, removed WorkflowRunId
		Metadata: &cmn.Metadata{
			Fields: map[string]string{
				"client_version": "1.0.0",
				"request_source": "dev_client",
				"request_id":     fmt.Sprintf("req-%d", time.Now().UnixNano()),
			},
		},
	}

	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := client.ExecuteTool(requestCtx, req)
	if err != nil {
		return err
	}

	if resp.Status == nil {
		logger.Warn("Response missing status field")
	}

	return nil
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func stringPtr(s string) *string {
	return &s
}