// Command printables serves the printables MCP server: search printables.com
// and hand out direct download links for model files.
package main

import (
	"os"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/servers/printables/internal/printables"
)

// version is set at build time.
var version = "dev"

func main() {
	endpoint := os.Getenv("PRINTABLES_API_URL")
	if endpoint == "" {
		endpoint = printables.DefaultEndpoint
	}

	mcpserver.Main(mcpserver.Config{
		Name:         "printables",
		Version:      version,
		Instructions: printables.Instructions,
		Register:     printables.Tools(printables.NewClient(endpoint, version)),
	})
}
