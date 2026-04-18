package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/veenone/pvesnap/internal/config"
)

type Client struct {
	nodes map[string]nodeClient
}

type nodeClient struct {
	endpoint string
	token    string
	http     *http.Client
}

func NewClient(cfg *config.Config) *Client {
	m := make(map[string]nodeClient, len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		tr := &http.Transport{
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: !n.VerifyTLS},
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}
		m[n.Name] = nodeClient{
			endpoint: strings.TrimRight(n.Endpoint, "/"),
			token:    n.APIToken,
			http:     &http.Client{Transport: tr, Timeout: 60 * time.Second},
		}
	}
	return &Client{nodes: m}
}

type apiResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors map[string]any  `json:"errors,omitempty"`
}

// do issues a request against a specific configured node. The node name here
// identifies which endpoint/credentials to use; the path may still target a
// different cluster node (e.g. /cluster/resources or /nodes/<other>/...).
func (c *Client) do(ctx context.Context, node, method, path string, form url.Values, out any) error {
	nc, ok := c.nodes[node]
	if !ok {
		return fmt.Errorf("proxmox: unknown node %q", node)
	}
	full := nc.endpoint + path
	var body io.Reader
	if form != nil && (method == http.MethodPost || method == http.MethodPut) {
		body = strings.NewReader(form.Encode())
	} else if form != nil {
		if !strings.Contains(full, "?") {
			full += "?"
		} else {
			full += "&"
		}
		full += form.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+nc.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := nc.http.Do(req)
	if err != nil {
		return fmt.Errorf("proxmox %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("proxmox %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("proxmox %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var api apiResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &api); err != nil {
			return fmt.Errorf("proxmox %s %s: decode: %w (body=%s)", method, path, err, string(raw))
		}
	}
	if len(api.Errors) > 0 {
		return fmt.Errorf("proxmox %s %s: %v", method, path, api.Errors)
	}
	if out != nil && len(api.Data) > 0 {
		if err := json.Unmarshal(api.Data, out); err != nil {
			return fmt.Errorf("proxmox %s %s: decode data: %w", method, path, err)
		}
	}
	return nil
}

// doString is a small helper for endpoints whose data field is a bare string
// (notably UPIDs returned by snapshot/rollback/delete).
func (c *Client) doString(ctx context.Context, node, method, path string, form url.Values) (string, error) {
	var s string
	if err := c.do(ctx, node, method, path, form, &s); err != nil {
		return "", err
	}
	return s, nil
}
