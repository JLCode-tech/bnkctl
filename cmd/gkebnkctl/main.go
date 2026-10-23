// Command gkebnkctl is the GCP-pinned F5 BNK lifecycle and scenario CLI entrypoint.
package main

import (
	"github.com/JLCode-tech/bnkctl/internal/cli"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
)

func main() {
	cli.ExecuteForCloud(intent.ProviderGCP)
}
