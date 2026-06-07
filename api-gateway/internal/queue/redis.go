package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

type JobFile struct {
	ID        string `json:"id"`
	Original  string `json:"original"`  // UUID filename of original image
	Processed string `json:"processed"` // UUID filename of processed image
	Status    string `json:"status"`    // PENDING, PROCESSING, DONE, FAILED
	Error     string `json:"error,omitempty"`
}

type JobState struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Status    string    `json:"status"` // UPLOADED, QUEUED, PROCESSING, DONE, FAILED
	Files     []JobFile `json:"files"`
	Progress  int       `json:"progress"` // 0 to 100
	UpdatedAt time.Time `json:"updated_at"`
}

type QueueManager struct {
	Client *redis.Client
}

// NewQueueManager initializes the Redis connection
func NewQueueManager() (*QueueManager, error) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	client := redis.NewClient(&redis.Options{
		Addr: addr,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis at %s: %w", addr, err)
	}

	return &QueueManager{Client: client}, nil
}

// EnqueueJob creates the initial state in Redis and pushes the ID onto the queue
func (qm *QueueManager) EnqueueJob(ctx context.Context, job *JobState) error {
	// First save as UPLOADED
	job.Status = "UPLOADED"
	if err := qm.SaveJobState(ctx, job); err != nil {
		return err
	}

	// Push the job ID to the worker queue
	if err := qm.Client.LPush(ctx, "job_queue", job.ID).Err(); err != nil {
		return fmt.Errorf("failed to push to Redis queue: %w", err)
	}

	// Update checkpoint to QUEUED
	job.Status = "QUEUED"
	return qm.SaveJobState(ctx, job)
}

// SaveJobState writes the job checkpoint information to Redis
func (qm *QueueManager) SaveJobState(ctx context.Context, job *JobState) error {
	job.UpdatedAt = time.Now()
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("failed to marshal job state: %w", err)
	}

	// Retain job status logs for 24 hours
	err = qm.Client.Set(ctx, fmt.Sprintf("job:%s", job.ID), data, 24*time.Hour).Err()
	if err != nil {
		return fmt.Errorf("failed to write job key to Redis: %w", err)
	}
	return nil
}

// GetJobState reads the job state by ID
func (qm *QueueManager) GetJobState(ctx context.Context, jobID string) (*JobState, error) {
	data, err := qm.Client.Get(ctx, fmt.Sprintf("job:%s", jobID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, errors.New("job not found")
	} else if err != nil {
		return nil, fmt.Errorf("failed to fetch job from Redis: %w", err)
	}

	var job JobState
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("failed to unmarshal job json: %w", err)
	}
	return &job, nil
}
