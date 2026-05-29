package catalog

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kubara-io/kubara/internal/utils"

	"github.com/urfave/cli/v3"
)

func resolveCatalogCommandWorkingDir(cmd *cli.Command) (string, error) {
	cwd, err := filepath.Abs(cmd.String("work-dir"))
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return cwd, nil
}

func resolveRegistryConfigPath(cmd *cli.Command, cwd string) (string, error) {
	raw := strings.TrimSpace(cmd.String("registry-config"))
	if raw == "" {
		return "", nil
	}

	resolved, err := utils.GetFullPath(raw, cwd)
	if err != nil {
		return "", fmt.Errorf("get registry config path: %w", err)
	}
	return resolved, nil
}
