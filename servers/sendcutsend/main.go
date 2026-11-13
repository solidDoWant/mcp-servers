// Command sendcutsend serves the sendcutsend MCP server: upload flat parts to
// sendcutsend.com, price them, and build saved carts to check out in a
// browser.
package main

import (
	"fmt"
	"os"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/servers/sendcutsend/internal/sendcutsend"
)

// version is set at build time.
var version = "dev"

func main() {
	client, err := sendcutsend.NewClient(sendcutsend.Options{
		BaseURL:         os.Getenv("SENDCUTSEND_BASE_URL"),
		CredentialsFile: os.Getenv("SENDCUTSEND_CREDENTIALS_FILE"),
		Version:         version,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	mcpserver.Main(mcpserver.Config{
		Name:         "sendcutsend",
		Version:      version,
		Instructions: sendcutsend.Instructions,
		Register:     sendcutsend.Tools(client),
	})
}
