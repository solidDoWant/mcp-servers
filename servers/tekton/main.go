// Command tekton serves the tekton MCP server: read Tekton Pipelines runs and
// their logs through the Kubernetes API.
package main

import (
	"fmt"
	"os"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/servers/tekton/internal/tekton"
)

// version is set at build time.
var version = "dev"

func main() {
	cfg, err := tekton.LoadConfig(version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	client, err := tekton.NewClient(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	mcpserver.Main(mcpserver.Config{
		Name:         "tekton",
		Version:      version,
		Instructions: tekton.Instructions,
		Register:     tekton.Tools(client),
	})
}
