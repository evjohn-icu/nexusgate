package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestStdioHandshake builds the binary and runs an MCP initialize + tools/list
// round-trip over stdio. It does not call the Hub (no tokens set), so it only
// proves the protocol layer and tool registration survive a real client.
func TestStdioHandshake(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary handshake in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("skipping: 'go' not found in PATH — build step requires the Go toolchain")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bin := filepath.Join(t.TempDir(), "timingdex-mcp")
	build := exec.Command("go", "build", "-o", bin, "./cmd/timingdex-mcp")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build timingdex-mcp: %v\n%s", err, out)
	}

	cli, err := client.NewStdioMCPClient(bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	if err := cli.Start(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := cli.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "handshake-test", Version: "0.0.1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerInfo.Name != "timingdex" {
		t.Fatalf("server name = %q, want timingdex", info.ServerInfo.Name)
	}
	tools, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 5 {
		t.Fatalf("tools = %d, want 5", len(tools.Tools))
	}
}
