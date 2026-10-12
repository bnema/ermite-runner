// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// engine is the part of the Docker Engine API the driver uses. The real
// implementation wraps github.com/moby/moby/client (the maintained client of
// the Moby project; github.com/docker/docker/client is its frozen
// predecessor); tests use a fake.
type engine interface {
	Info(ctx context.Context) (daemonInfo, error)
	// EnsureImage returns nil when ref exists locally, pulling it otherwise.
	EnsureImage(ctx context.Context, ref string) error
	CreateNetwork(ctx context.Context, spec networkSpec) (string, error)
	RemoveNetwork(ctx context.Context, id string) error
	ListNetworks(ctx context.Context, labelKey string) ([]string, error)
	CreateContainer(ctx context.Context, spec containerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	// WaitExit returns a channel that yields the exit code once the container
	// stops running (or -1 and an error when the wait itself fails).
	WaitExit(ctx context.Context, id string) <-chan exitResult
	RemoveContainer(ctx context.Context, id string) error
	ListContainers(ctx context.Context, labelKey string) ([]string, error)
	LogsTail(ctx context.Context, id string, lines int) (string, error)
	Close() error
}

type exitResult struct {
	Code int
	Err  error
}

type daemonInfo struct {
	Rootless bool
	Runtimes []string
	OSType   string
	// Podman: the "Docker API" is podman's compatibility service, whose netavark
	// configures a bridge address only when the first container starts.
	Podman bool
}

type networkSpec struct {
	Name    string
	Bridge  string // host bridge device name
	Subnet  netip.Prefix
	Gateway netip.Addr
	Labels  map[string]string
}

type containerSpec struct {
	Name       string
	Image      string
	Entrypoint []string
	Env        []string
	Labels     map[string]string
	Runtime    string // "" = daemon default
	Privileged bool
	Network    string
	IPv4       netip.Addr
	DNS        netip.Addr
	Binds      []bindMount
	MemoryMiB  int // also the swap limit: no swap
	CPUs       int
	Pids       int // PidsLimit
}

type bindMount struct {
	Source, Target string
	ReadOnly       bool
}

// dockerEngine implements engine with the moby client.
type dockerEngine struct{ cli *client.Client }

// newDockerEngine connects like the docker CLI: DOCKER_HOST, DOCKER_TLS_VERIFY,
// DOCKER_CERT_PATH, DOCKER_API_VERSION, with API version negotiation.
func newDockerEngine() (*dockerEngine, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &dockerEngine{cli: cli}, nil
}

func (e *dockerEngine) Close() error { return e.cli.Close() }

func (e *dockerEngine) Info(ctx context.Context) (daemonInfo, error) {
	r, err := e.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return daemonInfo{}, fmt.Errorf("docker info: %w", err)
	}
	di := daemonInfo{OSType: r.Info.OSType}
	for _, o := range r.Info.SecurityOptions {
		if o == "name=rootless" {
			di.Rootless = true
		}
	}
	for name := range r.Info.Runtimes {
		di.Runtimes = append(di.Runtimes, name)
	}
	// Best effort: a daemon that cannot say what it is is treated as Docker.
	if v, err := e.cli.ServerVersion(ctx, client.ServerVersionOptions{}); err == nil {
		for _, c := range v.Components {
			if strings.HasPrefix(c.Name, "Podman") {
				di.Podman = true
			}
		}
	}
	return di, nil
}

func (e *dockerEngine) EnsureImage(ctx context.Context, ref string) error {
	if _, err := e.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %s: %w", ref, err)
	}
	rc, err := e.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("image %s is not present and cannot be pulled (build it with scripts/build-image.sh): %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	if err := rc.Wait(ctx); err != nil {
		return fmt.Errorf("pull image %s: %w", ref, err)
	}
	return nil
}

func (e *dockerEngine) CreateNetwork(ctx context.Context, s networkSpec) (string, error) {
	f := false
	r, err := e.cli.NetworkCreate(ctx, s.Name, client.NetworkCreateOptions{
		Driver:     "bridge",
		EnableIPv6: &f,
		IPAM: &network.IPAM{Config: []network.IPAMConfig{{
			Subnet: s.Subnet, Gateway: s.Gateway,
		}}},
		Options: map[string]string{
			"com.docker.network.bridge.name": s.Bridge,
			// The runner's nftables table does the NAT, with its own policy.
			"com.docker.network.bridge.enable_ip_masquerade": "false",
		},
		Labels: s.Labels,
	})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", s.Name, err)
	}
	return r.ID, nil
}

func (e *dockerEngine) RemoveNetwork(ctx context.Context, id string) error {
	_, err := e.cli.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	return ignoreNotFound(err)
}

func (e *dockerEngine) ListNetworks(ctx context.Context, key string) ([]string, error) {
	r, err := e.cli.NetworkList(ctx, client.NetworkListOptions{Filters: client.Filters{}.Add("label", key)})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, n := range r.Items {
		ids = append(ids, n.ID)
	}
	return ids, nil
}

// hostConfigFor builds the Docker host configuration of a job container.
func hostConfigFor(s containerSpec) *container.HostConfig {
	hc := &container.HostConfig{
		NetworkMode:  container.NetworkMode(s.Network),
		Privileged:   s.Privileged,
		Runtime:      s.Runtime,
		CgroupnsMode: container.CgroupnsModePrivate,
		DNS:          []netip.Addr{s.DNS},
		Sysctls:      map[string]string{"net.ipv6.conf.all.disable_ipv6": "1", "net.ipv6.conf.default.disable_ipv6": "1"},
	}
	if s.MemoryMiB > 0 {
		hc.Memory = int64(s.MemoryMiB) << 20
		hc.MemorySwap = hc.Memory // swap = memory: the job cannot use swap
	}
	if s.CPUs > 0 {
		hc.NanoCPUs = int64(s.CPUs) * 1_000_000_000
	}
	if s.Pids > 0 {
		p := int64(s.Pids)
		hc.PidsLimit = &p
	}
	for _, b := range s.Binds {
		hc.Mounts = append(hc.Mounts, mount.Mount{Type: mount.TypeBind, Source: b.Source, Target: b.Target, ReadOnly: b.ReadOnly,
			BindOptions: &mount.BindOptions{CreateMountpoint: true}})
	}
	return hc
}

func (e *dockerEngine) CreateContainer(ctx context.Context, s containerSpec) (string, error) {
	hc := hostConfigFor(s)
	cfg := &container.Config{
		Image:      s.Image,
		Entrypoint: s.Entrypoint,
		Cmd:        []string{},
		Env:        s.Env,
		Labels:     s.Labels,
		Hostname:   "ermite-job",
	}
	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		s.Network: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: s.IPv4}},
	}}
	r, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Name: s.Name, Config: cfg, HostConfig: hc, NetworkingConfig: nc})
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	return r.ID, nil
}

func (e *dockerEngine) StartContainer(ctx context.Context, id string) error {
	_, err := e.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (e *dockerEngine) WaitExit(ctx context.Context, id string) <-chan exitResult {
	w := e.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	return awaitExit(ctx, w.Result, w.Error)
}

// awaitExit turns the two channels of a Docker wait into one result. A context
// that ended is reported as such (code -1, ctx.Err()), whichever channel the
// select picked: a cancelled wait must never look like a clean exit.
func awaitExit(ctx context.Context, result <-chan container.WaitResponse, errc <-chan error) <-chan exitResult {
	out := make(chan exitResult, 1)
	go func() {
		var res exitResult
		select {
		case r := <-result:
			res = exitResult{Code: int(r.StatusCode)}
		case err := <-errc:
			res = exitResult{Code: -1, Err: err}
		case <-ctx.Done():
			res = exitResult{Code: -1, Err: ctx.Err()}
		}
		if err := ctx.Err(); err != nil {
			res = exitResult{Code: -1, Err: err}
		}
		out <- res
	}()
	return out
}

func (e *dockerEngine) RemoveContainer(ctx context.Context, id string) error {
	_, err := e.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return ignoreNotFound(err)
}

func (e *dockerEngine) ListContainers(ctx context.Context, key string) ([]string, error) {
	r, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label", key)})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range r.Items {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

func (e *dockerEngine) LogsTail(ctx context.Context, id string, lines int) (string, error) {
	rc, err := e.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: fmt.Sprint(lines)})
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	return string(b), err
}

func ignoreNotFound(err error) error {
	if err == nil || cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

var errRootless = errors.New("the Docker daemon is rootless: a job needs its own nested dockerd, which requires a rootful daemon (or the sysbox runtime); point DOCKER_HOST at a rootful daemon or use ERMITE_RUNNER_ISOLATION=vm")
