package docker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MockClient is a simulated Docker client for testing without actual containers
type MockClient struct {
	mu         sync.RWMutex
	containers map[string]*MockContainer
	
	// Simulation settings
	PullDelay    time.Duration // Simulated image pull time
	StartDelay   time.Duration // Simulated container start time
	FailPull     bool          // Simulate pull failures
	FailStart    bool          // Simulate start failures
}

// MockContainer represents a simulated container
type MockContainer struct {
	ID        string
	Image     string
	Status    string
	IPAddress string
	Port      int
	StartedAt time.Time
	Labels    map[string]string
	GPUCount  int
}

// NewMockClient creates a new mock Docker client
func NewMockClient() *MockClient {
	return &MockClient{
		containers: make(map[string]*MockContainer),
		PullDelay:  100 * time.Millisecond,  // Fast for tests
		StartDelay: 200 * time.Millisecond,  // Fast for tests
	}
}

// NewMockClientWithDelays creates a mock client with custom delays
// Use this to simulate realistic cold-start times
func NewMockClientWithDelays(pullDelay, startDelay time.Duration) *MockClient {
	return &MockClient{
		containers: make(map[string]*MockContainer),
		PullDelay:  pullDelay,
		StartDelay: startDelay,
	}
}

// Close is a no-op for mock client
func (c *MockClient) Close() error {
	return nil
}

// Ping always succeeds for mock client
func (c *MockClient) Ping(ctx context.Context) error {
	return nil
}

// PullImage simulates pulling an image
func (c *MockClient) PullImage(ctx context.Context, imageName string) error {
	if c.FailPull {
		return fmt.Errorf("simulated pull failure for %s", imageName)
	}
	
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.PullDelay):
		return nil
	}
}

// LaunchContainer simulates launching a container
func (c *MockClient) LaunchContainer(ctx context.Context, opts LaunchOptions) (*ContainerInfo, error) {
	if c.FailStart {
		return nil, fmt.Errorf("simulated start failure for %s", opts.ModelImage)
	}
	
	// Simulate startup delay
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(c.StartDelay):
	}
	
	// Create mock container
	containerID := uuid.New().String()[:12]
	mockContainer := &MockContainer{
		ID:        containerID,
		Image:     opts.ModelImage,
		Status:    "running",
		IPAddress: fmt.Sprintf("172.17.0.%d", len(c.containers)+2),
		Port:      opts.ExposedPort,
		StartedAt: time.Now(),
		Labels:    opts.Labels,
		GPUCount:  opts.GPUCount,
	}
	
	if mockContainer.Port == 0 {
		mockContainer.Port = 8000
	}
	
	c.mu.Lock()
	c.containers[containerID] = mockContainer
	c.mu.Unlock()
	
	return &ContainerInfo{
		ID:        containerID,
		IPAddress: mockContainer.IPAddress,
		Port:      mockContainer.Port,
		Status:    mockContainer.Status,
		StartedAt: mockContainer.StartedAt,
	}, nil
}

// GetContainerInfo returns mock container info
func (c *MockClient) GetContainerInfo(ctx context.Context, containerID string) (*ContainerInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	container, exists := c.containers[containerID]
	if !exists {
		return nil, fmt.Errorf("container %s not found", containerID)
	}
	
	return &ContainerInfo{
		ID:        container.ID,
		IPAddress: container.IPAddress,
		Port:      container.Port,
		Status:    container.Status,
		StartedAt: container.StartedAt,
	}, nil
}

// StopContainer simulates stopping a container
func (c *MockClient) StopContainer(ctx context.Context, containerID string, timeout int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	container, exists := c.containers[containerID]
	if !exists {
		return fmt.Errorf("container %s not found", containerID)
	}
	
	container.Status = "exited"
	return nil
}

// RemoveContainer simulates removing a container
func (c *MockClient) RemoveContainer(ctx context.Context, containerID string, force bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	delete(c.containers, containerID)
	return nil
}

// TerminateContainer simulates stopping and removing a container
func (c *MockClient) TerminateContainer(ctx context.Context, containerID string) error {
	c.StopContainer(ctx, containerID, 10)
	return c.RemoveContainer(ctx, containerID, false)
}

// WaitForHealthy simulates waiting for container health
func (c *MockClient) WaitForHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	c.mu.RLock()
	_, exists := c.containers[containerID]
	c.mu.RUnlock()
	
	if !exists {
		return fmt.Errorf("container %s not found", containerID)
	}
	
	// Simulate small health check delay
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

// ListGPUContainers returns all mock containers
func (c *MockClient) ListGPUContainers(ctx context.Context, labelFilter map[string]string) ([]ContainerInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	result := make([]ContainerInfo, 0)
	for _, container := range c.containers {
		// Filter by labels if provided
		if labelFilter != nil {
			match := true
			for key, value := range labelFilter {
				if container.Labels[key] != value {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		
		result = append(result, ContainerInfo{
			ID:        container.ID,
			IPAddress: container.IPAddress,
			Port:      container.Port,
			Status:    container.Status,
			StartedAt: container.StartedAt,
		})
	}
	
	return result, nil
}

// GetContainerCount returns number of running containers (for testing)
func (c *MockClient) GetContainerCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.containers)
}

// GetContainer returns a mock container by ID (for testing)
func (c *MockClient) GetContainer(containerID string) (*MockContainer, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	container, exists := c.containers[containerID]
	return container, exists
}
