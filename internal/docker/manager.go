package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/netip"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/pkg/stdcopy"
	dclient "github.com/moby/moby/client"
)

// Manager handles Docker container lifecycle for managed MCP servers.
type Manager struct {
	cli         *dclient.Client
	networkName string
}

// NewManager creates a Docker manager connected to the given socket.
func NewManager(socket, networkName string) (*Manager, error) {
	var opts []dclient.Opt
	if socket != "" {
		opts = append(opts, dclient.WithHost(socket))
	}

	cli, err := dclient.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}

	m := &Manager{cli: cli, networkName: networkName}

	if err := m.ensureNetwork(context.Background()); err != nil {
		log.Printf("Warning: could not ensure docker network %q: %v", networkName, err)
	}

	return m, nil
}

func (m *Manager) ensureNetwork(ctx context.Context) error {
	result, err := m.cli.NetworkList(ctx, dclient.NetworkListOptions{})
	if err != nil {
		return err
	}
	for _, n := range result.Items {
		if n.Name == m.networkName {
			return nil
		}
	}
	_, err = m.cli.NetworkCreate(ctx, m.networkName, dclient.NetworkCreateOptions{
		Driver: "bridge",
	})
	return err
}

// PullImage pulls a Docker image.
func (m *Manager) PullImage(ctx context.Context, ref string) error {
	resp, err := m.cli.ImagePull(ctx, ref, dclient.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", ref, err)
	}
	defer resp.Close()
	io.Copy(io.Discard, resp)
	return nil
}

// EnsureImage checks if an image exists locally, and pulls it if not.
func (m *Manager) EnsureImage(ctx context.Context, ref string) error {
	_, err := m.cli.ImageInspect(ctx, ref)
	if err == nil {
		return nil // image exists locally
	}
	return m.PullImage(ctx, ref)
}

// ContainerConfig holds the parameters for creating a container.
type ContainerConfig struct {
	Name       string
	Image      string
	Entrypoint []string // overrides image ENTRYPOINT
	Command    []string
	Env        map[string]string
	Port       int // 0 for stdio servers
}

// StartContainer creates and starts a container. Returns the container ID.
func (m *Manager) StartContainer(ctx context.Context, cfg ContainerConfig) (string, error) {
	env := make([]string, 0, len(cfg.Env))
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}

	containerCfg := &container.Config{
		Image:     cfg.Image,
		Env:       env,
		OpenStdin: cfg.Port == 0,
		Tty:       false,
	}
	if len(cfg.Entrypoint) > 0 {
		containerCfg.Entrypoint = cfg.Entrypoint
	}
	if len(cfg.Command) > 0 {
		containerCfg.Cmd = cfg.Command
	}

	hostCfg := &container.HostConfig{}
	networkCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			m.networkName: {},
		},
	}

	if cfg.Port > 0 {
		port := network.MustParsePort(fmt.Sprintf("%d/tcp", cfg.Port))
		containerCfg.ExposedPorts = network.PortSet{
			port: {},
		}
		hostCfg.PortBindings = network.PortMap{
			port: []network.PortBinding{
				{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "0"},
			},
		}
	}

	containerName := "mcp-wrangler-" + cfg.Name
	createResult, err := m.cli.ContainerCreate(ctx, dclient.ContainerCreateOptions{
		Config:           containerCfg,
		HostConfig:       hostCfg,
		NetworkingConfig: networkCfg,
		Name:             containerName,
	})
	if err != nil {
		return "", fmt.Errorf("creating container %s: %w", containerName, err)
	}

	if _, err := m.cli.ContainerStart(ctx, createResult.ID, dclient.ContainerStartOptions{}); err != nil {
		m.cli.ContainerRemove(ctx, createResult.ID, dclient.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("starting container %s: %w", containerName, err)
	}

	return createResult.ID, nil
}

// StopContainer stops and removes a container.
func (m *Manager) StopContainer(ctx context.Context, containerID string) error {
	timeout := 10
	if _, err := m.cli.ContainerStop(ctx, containerID, dclient.ContainerStopOptions{Timeout: &timeout}); err != nil {
		log.Printf("Warning: error stopping container %s: %v", containerID, err)
	}
	_, err := m.cli.ContainerRemove(ctx, containerID, dclient.ContainerRemoveOptions{Force: true})
	return err
}

// AttachStdio attaches to a running container's stdin/stdout.
// The Docker stream is multiplexed (8-byte header per frame) when TTY=false,
// so we demux stdout into a clean pipe for JSON-RPC communication.
func (m *Manager) AttachStdio(ctx context.Context, containerID string) (io.WriteCloser, io.ReadCloser, error) {
	resp, err := m.cli.ContainerAttach(ctx, containerID, dclient.ContainerAttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("attaching to container %s: %w", containerID, err)
	}

	// Demux the Docker multiplexed stream into clean stdout/stderr pipes
	stdoutR, stdoutW := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(stdoutW, io.Discard, resp.Reader)
		if err != nil {
			log.Printf("docker demux error for %s: %v", containerID[:12], err)
		}
		stdoutW.Close()
	}()

	return resp.Conn, stdoutR, nil
}

// GetHostPort returns the host port mapped to the given container port.
func (m *Manager) GetHostPort(ctx context.Context, containerID string, containerPort int) (string, error) {
	result, err := m.cli.ContainerInspect(ctx, containerID, dclient.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspecting container %s: %w", containerID, err)
	}
	port := network.MustParsePort(fmt.Sprintf("%d/tcp", containerPort))
	bindings, ok := result.Container.NetworkSettings.Ports[port]
	if !ok || len(bindings) == 0 {
		return "", fmt.Errorf("no host port mapping for %d/tcp in container %s", containerPort, containerID)
	}
	return bindings[0].HostPort, nil
}

// IsRunning checks if a container is running.
func (m *Manager) IsRunning(ctx context.Context, containerID string) (bool, error) {
	result, err := m.cli.ContainerInspect(ctx, containerID, dclient.ContainerInspectOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "No such container") {
			return false, nil
		}
		return false, err
	}
	return result.Container.State.Running, nil
}

// WaitForHTTP waits for a container to be running.
func (m *Manager) WaitForHTTP(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout waiting for container %s", containerID)
		case <-tick.C:
			running, err := m.IsRunning(ctx, containerID)
			if err != nil {
				return err
			}
			if running {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) Close() error {
	return m.cli.Close()
}
