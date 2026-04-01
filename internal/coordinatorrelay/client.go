package coordinatorrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/r2"
)

type Client struct {
	r2 *r2.Client
}

func NewClient(r2c *r2.Client) *Client {
	return &Client{r2: r2c}
}

func (c *Client) R2() *r2.Client {
	if c == nil {
		return nil
	}
	return c.r2
}

func NewClientFromConfig(cfg *config.Config) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if cfg.CoordinatorHost == "" {
		return nil, nil
	}
	if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return nil, fmt.Errorf("coordinator relay requires [vastai.r2] configuration")
	}
	r2c, err := r2.New(r2.Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	})
	if err != nil {
		return nil, err
	}
	return NewClient(r2c), nil
}

func Enabled(cfg *config.Config) bool {
	return cfg != nil && cfg.CoordinatorHost != ""
}

func (c *Client) Submit(ctx context.Context, req *Request) (*Ack, error) {
	if c == nil || c.r2 == nil {
		return nil, fmt.Errorf("coordinator relay requires R2")
	}
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	if req.RequestID == "" {
		req.RequestID = NewRequestID(req.JobID)
	}
	if req.CreatedAt == "" {
		req.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if req.Client == nil {
		hostname, _ := os.Hostname()
		req.Client = &ClientMetadata{Hostname: hostname, PID: os.Getpid()}
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if err := c.r2.PutObject(ctx, controlplane.CoordinatorRelayRequest(req.RequestID), bytes.NewReader(data), "application/json"); err != nil {
		return nil, fmt.Errorf("upload relay request: %w", err)
	}
	return c.WaitForAck(ctx, req.RequestID)
}

func (c *Client) WaitForAck(ctx context.Context, requestID string) (*Ack, error) {
	if requestID == "" {
		return nil, fmt.Errorf("request ID is required")
	}
	ticker := time.NewTicker(DefaultPollDelay)
	defer ticker.Stop()
	key := controlplane.CoordinatorRelayAck(requestID)
	for {
		data, err := c.r2.GetObject(ctx, key)
		if err == nil {
			var ack Ack
			if err := json.Unmarshal(data, &ack); err != nil {
				return nil, fmt.Errorf("decode ack: %w", err)
			}
			return &ack, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func NewRequestID(jobID int64) string {
	return fmt.Sprintf("%d-%d-%06d", time.Now().UTC().UnixNano(), jobID, rand.Intn(1000000))
}
