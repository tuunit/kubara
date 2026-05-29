package catalog

import (
	internalcatalog "github.com/kubara-io/kubara/internal/catalog"
	"github.com/rs/zerolog/log"
)

func UnpackageCatalog(reference, outputPath, workDir, registryConfigPath string) error {
	result, err := internalcatalog.UnpackageCatalog(internalcatalog.UnpackageOptions{
		Reference:          reference,
		OutputPath:         outputPath,
		WorkDir:            workDir,
		RegistryConfigPath: registryConfigPath,
	})
	if err != nil {
		return err
	}

	log.Info().Msgf(
		"Catalog %q version %q has been unpackaged to %s",
		result.Artifact.CatalogName,
		result.Artifact.CatalogVersion,
		result.OutputPath,
	)
	return nil
}
