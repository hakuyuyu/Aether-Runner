package docker

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Client wraps the Docker SDK client with GPU-specific functionality
type Client struct {
	cli *client.Client
}

// LaunchOptions contains options for launching a container
type LaunchOptions struct {
	ModelImage      string
	GPUCount        int    // -1 for all GPUs
	GPUDeviceIDs    []string // Specific GPU UUIDs
	Memory          int64  // Memory limit in bytes
	CPUShares       int64
	NetworkMode     string
	Env             []string
	Labels          map[string]string
	ExposedPort     int
}

// ContainerInfo contains information about a running container
type ContainerInfo struct {
	ID        string
	IPAddress string
	Port      int
	Status    string
	StartedAt time.Time
}

// NewClient creates a new Docker client
func NewClient() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &Client{cli: cli}, nil
}

// Close closes the Docker client connection
func (c *Client) Close() error {
	return c.cli.Close()
}

// Ping checks if the Docker daemon is accessible
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx)
	return err
}

// PullImage pulls a container image if not present
func (c *Client) PullImage(ctx context.Context, imageName string) error {
	// Check if image exists locally
	_, _, err := c.cli.ImageInspectWithRaw(ctx, imageName)
	if err == nil {
		return nil // Image already exists
	}

	// Pull the image
	reader, err := c.cli.ImagePull(ctx, imageName, types.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", imageName, err)
	}
	defer reader.Close()

	// Consume the output (required for pull to complete)
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		return fmt.Errorf("failed to read pull response: %w", err)
	}

	return nil
}

// LaunchContainer creates and starts a GPU-enabled container
func (c *Client) LaunchContainer(ctx context.Context, opts LaunchOptions) (*ContainerInfo, error) {
	// Validate options
	if opts.ModelImage == "" {
		return nil, fmt.Errorf("model_image is required")
	}

	// Set defaults
	if opts.ExposedPort == 0 {
		opts.ExposedPort = 8000
	}
	if opts.NetworkMode == "" {
		opts.NetworkMode = "bridge"
	}

	// Build container config
	exposedPorts := nat.PortSet{
		nat.Port(fmt.Sprintf("%d/tcp", opts.ExposedPort)): struct{}{},
	}
	config := &container.Config{
		Image:        opts.ModelImage,
		Env:          opts.Env,
		Labels:       opts.Labels,
		ExposedPorts: exposedPorts,
	}

	// Build host config with GPU support
	hostConfig := &container.HostConfig{
		NetworkMode: container.NetworkMode(opts.NetworkMode),
		Resources: container.Resources{
			Memory:    opts.Memory,
			CPUShares: opts.CPUShares,
		},
	}

	// Configure GPU access via NVIDIA Container Toolkit
	if opts.GPUCount != 0 || len(opts.GPUDeviceIDs) > 0 {
		deviceRequest := container.DeviceRequest{
			Driver:       "nvidia",
			Capabilities: [][]string{{"gpu"}},
		}

		if len(opts.GPUDeviceIDs) > 0 {
			// Specific GPUs requested
			deviceRequest.DeviceIDs = opts.GPUDeviceIDs
		} else if opts.GPUCount == -1 {
			// All GPUs
			deviceRequest.Count = -1
		} else {
			// Specific count
			deviceRequest.Count = opts.GPUCount
		}

		hostConfig.Resources.DeviceRequests = []container.DeviceRequest{deviceRequest}
	}

	// Create the container
	resp, err := c.cli.ContainerCreate(ctx, config, hostConfig, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Start the container
	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created container
		_ = c.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Get container info
	info, err := c.GetContainerInfo(ctx, resp.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get container info: %w", err)
	}

	return info, nil
}

// GetContainerInfo retrieves information about a running container
func (c *Client) GetContainerInfo(ctx context.Context, containerID string) (*ContainerInfo, error) {
	inspect, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	info := &ContainerInfo{
		ID:     containerID,
		Status: inspect.State.Status,
	}

	// Parse started time
	if inspect.State.StartedAt != "" {
		startedAt, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt)
		if err == nil {
			info.StartedAt = startedAt
		}
	}

	// Get IP address from network settings
	if inspect.NetworkSettings != nil {
		// Try to get IP from default bridge network
		if inspect.NetworkSettings.IPAddress != "" {
			info.IPAddress = inspect.NetworkSettings.IPAddress
		}

		// Or from the first available network
		for _, netConfig := range inspect.NetworkSettings.Networks {
			if netConfig.IPAddress != "" {
				info.IPAddress = netConfig.IPAddress
				break
			}
		}
	}

	// Get exposed port
	for portSpec := range inspect.Config.ExposedPorts {
		port := portSpec.Int()
		if port > 0 {
			info.Port = port
			break
		}
	}

	return info, nil
}

// StopContainer stops a running container
func (c *Client) StopContainer(ctx context.Context, containerID string, timeout int) error {
	stopTimeout := timeout
	return c.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &stopTimeout})
}

// RemoveContainer removes a container
func (c *Client) RemoveContainer(ctx context.Context, containerID string, force bool) error {
	return c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force:         force,
		RemoveVolumes: true,
	})
}

// TerminateContainer stops and removes a container
func (c *Client) TerminateContainer(ctx context.Context, containerID string) error {
	// Try graceful stop first
	if err := c.StopContainer(ctx, containerID, 10); err != nil {
		// Force remove if stop fails
		return c.RemoveContainer(ctx, containerID, true)
	}

	return c.RemoveContainer(ctx, containerID, false)
}

// ListGPUContainers lists all containers created by this orchestrator
func (c *Client) ListGPUContainers(ctx context.Context, labelFilter map[string]string) ([]ContainerInfo, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	result := make([]ContainerInfo, 0)
	for _, ctr := range containers {
		// Filter by labels if provided
		if labelFilter != nil {
			match := true
			for key, value := range labelFilter {
				if ctr.Labels[key] != value {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}

		info, err := c.GetContainerInfo(ctx, ctr.ID)
		if err != nil {
			continue // Skip containers we can't inspect
		}
		result = append(result, *info)
	}

	return result, nil
}

// WaitForHealthy waits for a container to become healthy
func (c *Client) WaitForHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		inspect, err := c.cli.ContainerInspect(ctx, containerID)
		if err != nil {
			return fmt.Errorf("failed to inspect container: %w", err)
		}

		// Check if container is running
		if !inspect.State.Running {
			return fmt.Errorf("container exited with code %d", inspect.State.ExitCode)
		}

		// If no health check defined, consider it healthy once running
		if inspect.State.Health == nil {
			return nil
		}

		// Check health status
		if inspect.State.Health.Status == "healthy" {
			return nil
		}

		// Wait and retry
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
			continue
		}
	}

	return fmt.Errorf("timeout waiting for container to become healthy")
}

// NetworkInfo retrieves network information for a container
func (c *Client) NetworkInfo(ctx context.Context, containerID string) (*network.EndpointSettings, error) {
	inspect, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	// Return the first network endpoint
	for _, settings := range inspect.NetworkSettings.Networks {
		return settings, nil
	}

	return nil, fmt.Errorf("container has no network settings")
}
