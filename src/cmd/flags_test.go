package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kubara-io/kubara/internal/catalog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestCatalogLoadOptionsFromCommand_PreservesOCIReference(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	var got catalog.LoadOptions

	app := &cli.Command{
		Name:  "kubara",
		Flags: NewGlobalFlags().CLIFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			var err error
			got, err = catalogLoadOptionsFromCommand(cmd)
			return err
		},
	}

	err := app.Run(context.Background(), []string{
		"kubara",
		"--work-dir", tempDir,
		"--catalog", "oci://ghcr.io/example/catalog:1.2.3",
		"--registry-config", "docker/config.json",
	})
	require.NoError(t, err)

	assert.Equal(t, "oci://ghcr.io/example/catalog:1.2.3", got.CatalogPath)
	assert.Equal(t, filepath.Join(tempDir, "docker", "config.json"), got.RegistryConfigPath)
}
