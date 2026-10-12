// SPDX-License-Identifier: MIT

package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Firecracker is controlled through its HTTP API on a unix socket, with
// net/http, rather than through firecracker-go-sdk: the SDK's last release
// (v1.0.0, 2022) predates Firecracker 1.17 and drags in CNI, go-openapi and
// netlink for what is a handful of PUT requests. The structures below are the
// subset of the API this driver uses; Firecracker rejects unknown or invalid
// fields with a fault message, which surfaces in the returned error.

type machineConfig struct {
	VcpuCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type networkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac"`
}

type vsockDevice struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type action struct {
	ActionType string `json:"action_type"`
}

// apiClient talks to one Firecracker instance.
type apiClient struct {
	http *http.Client
}

func newAPIClient(socket string) *apiClient {
	return &apiClient{http: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		},
	}}
}

// put sends a PUT with a JSON body and expects 204 (or 200).
func (c *apiClient) put(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://firecracker"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker PUT %s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// ready reports whether the API answers GET /.
func (c *apiClient) ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://firecracker/", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("firecracker GET /: %s", resp.Status)
	}
	return nil
}

// vmConfig is everything configured before boot.
type vmConfig struct {
	Machine machineConfig
	Boot    bootSource
	Drives  []drive
	Net     networkInterface
	Vsock   vsockDevice
}

// configure applies the pre-boot configuration and boots the VM. The order is
// fixed (and covered by a unit test) because Firecracker rejects some changes
// after boot and drive order decides the guest device names vda, vdb, vdc.
func (c *apiClient) configure(ctx context.Context, cfg vmConfig) error {
	steps := []struct {
		path string
		body any
	}{
		{"/machine-config", cfg.Machine},
		{"/boot-source", cfg.Boot},
	}
	for _, d := range cfg.Drives {
		steps = append(steps, struct {
			path string
			body any
		}{"/drives/" + d.DriveID, d})
	}
	steps = append(steps,
		struct {
			path string
			body any
		}{"/network-interfaces/" + cfg.Net.IfaceID, cfg.Net},
		struct {
			path string
			body any
		}{"/vsock", cfg.Vsock},
		struct {
			path string
			body any
		}{"/entropy", struct{}{}},
		struct {
			path string
			body any
		}{"/actions", action{ActionType: "InstanceStart"}},
	)
	for _, s := range steps {
		if err := c.put(ctx, s.path, s.body); err != nil {
			return err
		}
	}
	return nil
}
