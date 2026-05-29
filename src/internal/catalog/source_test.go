package catalog

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func TestParseOCIReference_StrictVersionAndDigest(t *testing.T) {
	t.Parallel()

	tagged, err := ParseOCIReference("oci://ghcr.io/example/catalog:1.2.3", true)
	require.NoError(t, err)
	assert.False(t, tagged.IsDigest)
	assert.Equal(t, "1.2.3", tagged.Tag)

	digested, err := ParseOCIReference("oci://ghcr.io/example/catalog@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true)
	require.NoError(t, err)
	assert.True(t, digested.IsDigest)

	_, err = ParseOCIReference("oci://ghcr.io/example/catalog:v1.2.3", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `must match exact semantic version format "x.y.z"`)
}

func TestParseOCIReferenceBase_DefaultAndExplicit(t *testing.T) {
	t.Parallel()

	defaultBase, err := ParseOCIReferenceBase("")
	require.NoError(t, err)
	assert.Equal(t, defaultLocalCatalogRef, defaultBase.Raw)
	assert.Empty(t, defaultBase.Repository)

	explicitBase, err := ParseOCIReferenceBase("oci://ghcr.io/example")
	require.NoError(t, err)
	assert.Equal(t, "oci://ghcr.io/example/", explicitBase.Raw)

	_, err = ParseOCIReferenceBase("oci://ghcr.io/example/demo-catalog:1.2.3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not include a tag or digest")
}

func TestNewRegistryHTTPClient_InsecureTLS(t *testing.T) {
	t.Parallel()

	client := newRegistryHTTPClient(true)

	transport, ok := client.Transport.(*retry.Transport)
	require.True(t, ok)

	base, ok := transport.Base.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, base.TLSClientConfig)
	assert.True(t, base.TLSClientConfig.InsecureSkipVerify)
}

func TestPackageCatalog_CachesArtifactAndReference(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	catalogRoot := filepath.Join(t.TempDir(), "demo-catalog")
	writeCatalogFixture(t, catalogRoot, "demo-catalog", "1.2.3")

	result, err := PackageCatalog(PackageOptions{CatalogRoot: catalogRoot})
	require.NoError(t, err)

	assert.Equal(t, "demo-catalog", result.Manifest.Metadata.Name)
	assert.Equal(t, "1.2.3", result.Manifest.Spec.Version)
	assert.Equal(t, "oci://localhost/demo-catalog:1.2.3", result.Reference)
	assert.NotEmpty(t, result.Artifact.ManifestDigest)

	artifactDir := filepath.Join(homeDir, ".kubara", "catalogs", "artifacts", digestPathComponent(result.Artifact.ManifestDigest))
	assert.FileExists(t, filepath.Join(artifactDir, "layout", "index.json"))
	assert.FileExists(t, filepath.Join(artifactDir, "contents", "demo-catalog", "Catalog.yaml"))
	assert.FileExists(t, filepath.Join(homeDir, ".kubara", "catalogs", "refs", "localhost", "demo-catalog", "tags", "1.2.3.json"))
}

func TestPackageCatalog_UsesProvidedReferenceBase(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	catalogRoot := filepath.Join(t.TempDir(), "demo-catalog")
	writeCatalogFixture(t, catalogRoot, "demo-catalog", "1.2.3")

	result, err := PackageCatalog(PackageOptions{
		CatalogRoot:   catalogRoot,
		ReferenceBase: "oci://ghcr.io/example/catalogs/",
	})
	require.NoError(t, err)

	assert.Equal(t, "oci://ghcr.io/example/catalogs/demo-catalog:1.2.3", result.Reference)
	assert.FileExists(t, filepath.Join(homeDir, ".kubara", "catalogs", "refs", "ghcr.io", "example", "catalogs", "demo-catalog", "tags", "1.2.3.json"))
}

func TestResolveSource_DirectoryCatalog(t *testing.T) {
	t.Parallel()

	catalogRoot := filepath.Join(t.TempDir(), "demo-catalog")
	writeCatalogFixture(t, catalogRoot, "demo-catalog", "1.2.3")

	source, err := ResolveSource(catalogRoot, "")
	require.NoError(t, err)

	assert.Equal(t, SourceKindDirectory, source.Kind)
	assert.Equal(t, catalogRoot, source.RootPath)
	assert.Equal(t, filepath.Join(catalogRoot, servicesDirectory), source.ServicesPath)
	require.NotNil(t, source.Manifest)
	assert.Equal(t, "1.2.3", source.Manifest.Spec.Version)
}

func TestLoadCatalogManifest_RequiresVersionForPackaging(t *testing.T) {
	t.Parallel()

	catalogRoot := filepath.Join(t.TempDir(), "demo-catalog")
	require.NoError(t, os.MkdirAll(catalogRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(catalogRoot, "Catalog.yaml"), []byte(`
apiVersion: kubara.io/v1alpha1
kind: Catalog
metadata:
  name: demo-catalog
`), 0o600))

	_, err := LoadCatalogManifest(catalogRoot, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing spec.version")
}

func TestListCachedCatalogs_IncludesLocalAndRemoteReferences(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	catalogRoot := filepath.Join(t.TempDir(), "demo-catalog")
	writeCatalogFixture(t, catalogRoot, "demo-catalog", "1.2.3")

	result, err := PackageCatalog(PackageOptions{CatalogRoot: catalogRoot})
	require.NoError(t, err)

	remoteRef, err := ParseOCIReference("oci://ghcr.io/example/demo-catalog:1.2.3", true)
	require.NoError(t, err)
	require.NoError(t, writeRemoteTagReference(remoteRef, CachedArtifact{
		SchemaVersion:     cacheSchemaVersion,
		CatalogName:       result.Manifest.Metadata.Name,
		CatalogVersion:    result.Manifest.Spec.Version,
		ManifestDigest:    result.Artifact.ManifestDigest,
		OriginalReference: remoteRef.Raw,
		Registry:          remoteRef.Registry,
		Repository:        remoteRef.Repository,
		Tag:               remoteRef.Tag,
	}))

	entries, err := ListCachedCatalogs()
	require.NoError(t, err)

	assert.Contains(t, entries, CachedCatalogEntry{
		Reference:      result.Reference,
		CatalogName:    "demo-catalog",
		CatalogVersion: "1.2.3",
		ManifestDigest: result.Artifact.ManifestDigest,
	})
	assert.Contains(t, entries, CachedCatalogEntry{
		Reference:      remoteRef.Raw,
		CatalogName:    "demo-catalog",
		CatalogVersion: "1.2.3",
		ManifestDigest: result.Artifact.ManifestDigest,
	})
}

func TestUnpackageCatalog_UsesCachedReferenceAndDefaultDirectory(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	workDir := t.TempDir()
	catalogRoot := filepath.Join(t.TempDir(), "demo-catalog")
	writeCatalogFixture(t, catalogRoot, "demo-catalog", "1.2.3")

	result, err := PackageCatalog(PackageOptions{CatalogRoot: catalogRoot})
	require.NoError(t, err)

	unpackResult, err := UnpackageCatalog(UnpackageOptions{
		Reference: result.Reference,
		WorkDir:   workDir,
	})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(workDir, "demo-catalog"), unpackResult.OutputPath)
	assert.FileExists(t, filepath.Join(unpackResult.OutputPath, "Catalog.yaml"))
	assert.FileExists(t, filepath.Join(unpackResult.OutputPath, "services", "demo-service.yaml"))
}

func TestEnsureRemoteCatalogCached_DefaultLocalReferenceRequiresLocalCache(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	_, err := EnsureRemoteCatalogCached(PullOptions{
		Reference: "oci://localhost/demo-catalog:1.2.3",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "package it first")
}

func TestPushCatalog_FromCachedReferenceRejectsMismatchedTargetVersion(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	catalogRoot := filepath.Join(t.TempDir(), "demo-catalog")
	writeCatalogFixture(t, catalogRoot, "demo-catalog", "1.2.3")

	result, err := PackageCatalog(PackageOptions{CatalogRoot: catalogRoot})
	require.NoError(t, err)

	_, err = PushCatalog(PushOptions{
		SourceReference: result.Reference,
		Reference:       "oci://ghcr.io/example/demo-catalog:2.0.0",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `catalog version "1.2.3" does not match target tag "2.0.0"`)
}

func writeCatalogFixture(t *testing.T, root, name, version string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(root, "services"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "managed-service-catalog", "helm"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Catalog.yaml"), []byte(`
apiVersion: kubara.io/v1alpha1
kind: Catalog
metadata:
  name: `+name+`
spec:
  version: `+version+`
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "services", "demo-service.yaml"), []byte(`
apiVersion: kubara.io/v1alpha1
kind: ServiceDefinition
metadata:
  name: demo-service
spec:
  chartPath: demo-service
  status: enabled
`), 0o600))
}
