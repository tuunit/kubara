package catalog

import (
	"context"
	_ "crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
	"sigs.k8s.io/yaml"
)

const (
	CatalogArtifactType    = "application/vnd.kubara.catalog.v1"
	CatalogLayerMediaType  = "application/vnd.kubara.catalog.layer.v1.tar+gzip"
	cacheSchemaVersion     = "v1"
	defaultDockerConfigRel = ".docker/config.json"
	defaultCatalogCacheRel = ".kubara/catalogs"
	defaultLocalCatalogRef = "oci://localhost/"
	ociScheme              = "oci://"
)

type SourceKind string

const (
	SourceKindDirectory SourceKind = "directory"
	SourceKindOCI       SourceKind = "oci"
)

type ResolvedSource struct {
	Kind         SourceKind
	RootPath     string
	ServicesPath string
	Manifest     *CatalogManifest
	Artifact     *CachedArtifact
}

type OCIReference struct {
	Raw        string
	Registry   string
	Repository string
	Reference  string
	Tag        string
	Digest     digest.Digest
	IsDigest   bool
}

type CachedArtifact struct {
	SchemaVersion     string `json:"schemaVersion"`
	CatalogName       string `json:"catalogName"`
	CatalogVersion    string `json:"catalogVersion"`
	ManifestDigest    string `json:"manifestDigest"`
	OriginalReference string `json:"originalReference,omitempty"`
	Registry          string `json:"registry,omitempty"`
	Repository        string `json:"repository,omitempty"`
	Tag               string `json:"tag,omitempty"`
	RootDirectory     string `json:"rootDirectory"`
	CreatedAt         string `json:"createdAt"`
}

type cachedReference struct {
	SchemaVersion  string `json:"schemaVersion"`
	ManifestDigest string `json:"manifestDigest"`
	CatalogName    string `json:"catalogName"`
	CatalogVersion string `json:"catalogVersion"`
	Reference      string `json:"reference,omitempty"`
	Registry       string `json:"registry,omitempty"`
	Repository     string `json:"repository,omitempty"`
	Tag            string `json:"tag,omitempty"`
	UpdatedAt      string `json:"updatedAt"`
}

type PackageResult struct {
	Manifest  CatalogManifest
	Artifact  CachedArtifact
	Reference string
}

type PackageOptions struct {
	CatalogRoot   string
	ReferenceBase string
}

type PushOptions struct {
	CatalogRoot        string
	SourceReference    string
	Reference          string
	RegistryConfigPath string
	Insecure           bool
}

type PullOptions struct {
	Reference          string
	RegistryConfigPath string
	Force              bool
	Insecure           bool
}

type UnpackageOptions struct {
	Reference          string
	OutputPath         string
	WorkDir            string
	RegistryConfigPath string
}

func IsOCIReference(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), ociScheme)
}

func ResolveSource(catalogPath, registryConfigPath string) (ResolvedSource, error) {
	raw := strings.TrimSpace(catalogPath)
	if raw == "" {
		return ResolvedSource{}, nil
	}

	if !IsOCIReference(raw) {
		return resolveDirectorySource(raw)
	}

	if _, err := ParseOCIReference(raw, true); err != nil {
		return ResolvedSource{}, err
	}

	artifact, err := EnsureRemoteCatalogCached(PullOptions{
		Reference:          raw,
		RegistryConfigPath: registryConfigPath,
	})
	if err != nil {
		return ResolvedSource{}, err
	}

	return resolvedSourceFromArtifact(artifact)
}

func ParseOCIReference(raw string, allowDigest bool) (OCIReference, error) {
	trimmed := strings.TrimSpace(raw)
	if !IsOCIReference(trimmed) {
		return OCIReference{}, fmt.Errorf("catalog reference %q must use the oci:// scheme", raw)
	}

	parsed, err := registry.ParseReference(strings.TrimPrefix(trimmed, ociScheme))
	if err != nil {
		return OCIReference{}, fmt.Errorf("parse OCI reference %q: %w", raw, err)
	}
	if err := parsed.ValidateRegistry(); err != nil {
		return OCIReference{}, fmt.Errorf("invalid registry in %q: %w", raw, err)
	}
	if err := parsed.ValidateRepository(); err != nil {
		return OCIReference{}, fmt.Errorf("invalid repository in %q: %w", raw, err)
	}
	if strings.TrimSpace(parsed.Reference) == "" {
		return OCIReference{}, fmt.Errorf("OCI reference %q must include either a tag or a digest", raw)
	}

	if err := parsed.ValidateReferenceAsDigest(); err == nil {
		if !allowDigest {
			return OCIReference{}, fmt.Errorf("OCI reference %q must use a tag in exact x.y.z format", raw)
		}
		dgst, err := parsed.Digest()
		if err != nil {
			return OCIReference{}, fmt.Errorf("parse digest from %q: %w", raw, err)
		}
		return OCIReference{
			Raw:        trimmed,
			Registry:   parsed.Registry,
			Repository: parsed.Repository,
			Reference:  parsed.Reference,
			Digest:     dgst,
			IsDigest:   true,
		}, nil
	}

	if err := parsed.ValidateReferenceAsTag(); err != nil {
		return OCIReference{}, fmt.Errorf("invalid OCI tag in %q: %w", raw, err)
	}
	if !StrictCatalogVersion.MatchString(parsed.Reference) {
		return OCIReference{}, fmt.Errorf(`OCI tag %q must match exact semantic version format "x.y.z" without a leading "v"`, parsed.Reference)
	}

	return OCIReference{
		Raw:        trimmed,
		Registry:   parsed.Registry,
		Repository: parsed.Repository,
		Reference:  parsed.Reference,
		Tag:        parsed.Reference,
		IsDigest:   false,
	}, nil
}

func ParseOCIReferenceBase(raw string) (OCIReference, error) {
	base := strings.TrimSpace(raw)
	if base == "" {
		base = defaultLocalCatalogRef
	}
	if !IsOCIReference(base) {
		return OCIReference{}, fmt.Errorf("catalog reference base %q must use the oci:// scheme", raw)
	}

	normalized := base
	if !strings.HasSuffix(normalized, "/") {
		normalized += "/"
	}

	trimmed := strings.TrimSuffix(strings.TrimPrefix(normalized, ociScheme), "/")
	if parsedBase, err := registry.ParseReference(trimmed); err == nil && strings.TrimSpace(parsedBase.Reference) != "" {
		return OCIReference{}, fmt.Errorf("OCI reference base %q must not include a tag or digest", base)
	}
	validationRef := trimmed + "/kubara-placeholder:0.0.1"
	parsed, err := registry.ParseReference(validationRef)
	if err != nil {
		return OCIReference{}, fmt.Errorf("parse OCI reference base %q: %w", base, err)
	}
	if err := parsed.ValidateRegistry(); err != nil {
		return OCIReference{}, fmt.Errorf("invalid registry in %q: %w", base, err)
	}

	repository := ""
	if separator := strings.Index(trimmed, "/"); separator >= 0 {
		repository = trimmed[separator+1:]
	}
	if repository != "" {
		if err := (registry.Reference{Registry: parsed.Registry, Repository: repository}).ValidateRepository(); err != nil {
			return OCIReference{}, fmt.Errorf("invalid repository in %q: %w", base, err)
		}
	}

	return OCIReference{
		Raw:        normalized,
		Registry:   parsed.Registry,
		Repository: repository,
	}, nil
}

func BuildCatalogReference(catalogName, version, rawBase string) (OCIReference, error) {
	base, err := ParseOCIReferenceBase(rawBase)
	if err != nil {
		return OCIReference{}, err
	}

	repository := catalogName
	if base.Repository != "" {
		repository = fmt.Sprintf("%s/%s", base.Repository, catalogName)
	}
	return ParseOCIReference(fmt.Sprintf("oci://%s/%s:%s", base.Registry, repository, version), false)
}

func EnsureRemoteCatalogCached(options PullOptions) (CachedArtifact, error) {
	ref, err := ParseOCIReference(options.Reference, true)
	if err != nil {
		return CachedArtifact{}, err
	}

	if ref.IsDigest {
		if artifact, found, err := findArtifactByDigest(ref.Digest); err != nil {
			return CachedArtifact{}, err
		} else if found {
			return artifact, nil
		}
		return pullRemoteCatalog(ref, options.RegistryConfigPath, options.Insecure)
	}

	if artifact, found, err := findTagArtifact(ref); err != nil {
		return CachedArtifact{}, err
	} else if found && (!options.Force || isLocalhostReference(ref)) {
		return artifact, nil
	}
	if isLocalhostReference(ref) {
		return CachedArtifact{}, fmt.Errorf("cached local catalog %q was not found; package it first with `kubara catalog package`", ref.Raw)
	}

	return pullRemoteCatalog(ref, options.RegistryConfigPath, options.Insecure)
}

func PackageCatalog(options PackageOptions) (PackageResult, error) {
	manifest, err := LoadCatalogManifest(options.CatalogRoot, true)
	if err != nil {
		return PackageResult{}, err
	}

	ref, err := BuildCatalogReference(manifest.Metadata.Name, manifest.Spec.Version, options.ReferenceBase)
	if err != nil {
		return PackageResult{}, err
	}

	artifact, err := createCachedArtifact(manifest, options.CatalogRoot, CachedArtifact{
		SchemaVersion:     cacheSchemaVersion,
		CatalogName:       manifest.Metadata.Name,
		CatalogVersion:    manifest.Spec.Version,
		OriginalReference: ref.Raw,
		Registry:          ref.Registry,
		Repository:        ref.Repository,
		Tag:               ref.Tag,
		RootDirectory:     manifest.Metadata.Name,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return PackageResult{}, err
	}

	if err := writeLocalReference(ref, artifact); err != nil {
		return PackageResult{}, err
	}

	return PackageResult{
		Manifest:  manifest,
		Artifact:  artifact,
		Reference: ref.Raw,
	}, nil
}

func PushCatalog(options PushOptions) (PackageResult, error) {
	ref, err := ParseOCIReference(options.Reference, false)
	if err != nil {
		return PackageResult{}, err
	}

	manifest, artifact, err := resolvePushSource(options)
	if err != nil {
		return PackageResult{}, err
	}
	if manifest.Spec.Version != ref.Tag {
		return PackageResult{}, fmt.Errorf("catalog version %q does not match target tag %q", manifest.Spec.Version, ref.Tag)
	}

	repo, err := newRemoteRepository(ref, options.RegistryConfigPath, options.Insecure)
	if err != nil {
		return PackageResult{}, err
	}

	layoutStore, err := oci.New(artifactLayoutPath(artifact))
	if err != nil {
		return PackageResult{}, fmt.Errorf("open OCI layout store: %w", err)
	}

	ctx := context.Background()
	if _, err := oras.Copy(ctx, layoutStore, manifest.Spec.Version, repo, ref.Tag, oras.DefaultCopyOptions); err != nil {
		return PackageResult{}, fmt.Errorf("push catalog to %q: %w", options.Reference, err)
	}
	if err := writeRemoteTagReference(ref, artifact); err != nil {
		return PackageResult{}, err
	}

	return PackageResult{
		Manifest:  manifest,
		Artifact:  artifact,
		Reference: ref.Raw,
	}, nil
}

func resolvePushSource(options PushOptions) (CatalogManifest, CachedArtifact, error) {
	if strings.TrimSpace(options.SourceReference) == "" {
		result, err := PackageCatalog(PackageOptions{
			CatalogRoot: options.CatalogRoot,
		})
		if err != nil {
			return CatalogManifest{}, CachedArtifact{}, err
		}
		return result.Manifest, result.Artifact, nil
	}

	artifact, err := EnsureRemoteCatalogCached(PullOptions{
		Reference:          options.SourceReference,
		RegistryConfigPath: options.RegistryConfigPath,
		Insecure:           options.Insecure,
	})
	if err != nil {
		return CatalogManifest{}, CachedArtifact{}, err
	}

	manifest, err := LoadCatalogManifest(artifactRootPath(artifact), true)
	if err != nil {
		return CatalogManifest{}, CachedArtifact{}, err
	}

	return manifest, artifact, nil
}

func LoadCatalogManifest(root string, requireVersion bool) (CatalogManifest, error) {
	path := filepath.Join(root, "Catalog.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		return CatalogManifest{}, fmt.Errorf("read %q: %w", path, err)
	}

	var manifest CatalogManifest
	if err := yaml.Unmarshal(content, &manifest); err != nil {
		return CatalogManifest{}, fmt.Errorf("unmarshal %q: %w", path, err)
	}
	if err := manifest.Validate(requireVersion); err != nil {
		return CatalogManifest{}, fmt.Errorf("invalid Catalog.yaml: %w", err)
	}

	return manifest, nil
}

func resolveDirectorySource(catalogPath string) (ResolvedSource, error) {
	cleaned := filepath.Clean(catalogPath)

	rootInfo, err := os.Stat(cleaned)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("catalog directory %q does not exist: %w", cleaned, err)
	}
	if !rootInfo.IsDir() {
		return ResolvedSource{}, fmt.Errorf("catalog path %q is not a directory", cleaned)
	}

	servicesPath, err := resolveServicesPath(cleaned)
	if err != nil {
		return ResolvedSource{}, err
	}

	source := ResolvedSource{
		Kind:         SourceKindDirectory,
		RootPath:     cleaned,
		ServicesPath: servicesPath,
	}

	manifest, err := tryLoadCatalogManifest(cleaned)
	if err != nil {
		return ResolvedSource{}, err
	}
	source.Manifest = manifest

	return source, nil
}

func tryLoadCatalogManifest(root string) (*CatalogManifest, error) {
	path := filepath.Join(root, "Catalog.yaml")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}

	manifest, err := LoadCatalogManifest(root, false)
	if err != nil {
		return nil, err
	}
	return &manifest, nil
}

func resolvedSourceFromArtifact(artifact CachedArtifact) (ResolvedSource, error) {
	rootPath := artifactRootPath(artifact)
	servicesPath, err := resolveServicesPath(rootPath)
	if err != nil {
		return ResolvedSource{}, err
	}

	manifest, err := LoadCatalogManifest(rootPath, true)
	if err != nil {
		return ResolvedSource{}, err
	}

	return ResolvedSource{
		Kind:         SourceKindOCI,
		RootPath:     rootPath,
		ServicesPath: servicesPath,
		Manifest:     &manifest,
		Artifact:     &artifact,
	}, nil
}

func pullRemoteCatalog(ref OCIReference, registryConfigPath string, insecure bool) (CachedArtifact, error) {
	repo, err := newRemoteRepository(ref, registryConfigPath, insecure)
	if err != nil {
		return CachedArtifact{}, err
	}

	ctx := context.Background()
	desc, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return CachedArtifact{}, fmt.Errorf("resolve remote catalog %q: %w", ref.Raw, err)
	}

	tempArtifactDir, cleanup, err := newTempArtifactDir()
	if err != nil {
		return CachedArtifact{}, err
	}
	defer cleanup()

	layoutDir := filepath.Join(tempArtifactDir, "layout")
	layoutStore, err := oci.New(layoutDir)
	if err != nil {
		return CachedArtifact{}, fmt.Errorf("create OCI layout store: %w", err)
	}
	if err := oras.CopyGraph(ctx, repo, layoutStore, desc, oras.DefaultCopyGraphOptions); err != nil {
		return CachedArtifact{}, fmt.Errorf("pull catalog graph from %q: %w", ref.Raw, err)
	}
	if !ref.IsDigest {
		if err := layoutStore.Tag(ctx, desc, ref.Tag); err != nil {
			return CachedArtifact{}, fmt.Errorf("tag OCI layout with %q: %w", ref.Tag, err)
		}
	}

	contentsDir := filepath.Join(tempArtifactDir, "contents")
	rootDir, err := extractCatalogContents(ctx, layoutStore, desc, contentsDir)
	if err != nil {
		return CachedArtifact{}, err
	}

	manifest, err := LoadCatalogManifest(rootDir, true)
	if err != nil {
		return CachedArtifact{}, err
	}
	if !ref.IsDigest && manifest.Spec.Version != ref.Tag {
		return CachedArtifact{}, fmt.Errorf("catalog version %q does not match pulled tag %q", manifest.Spec.Version, ref.Tag)
	}

	artifact := CachedArtifact{
		SchemaVersion:     cacheSchemaVersion,
		CatalogName:       manifest.Metadata.Name,
		CatalogVersion:    manifest.Spec.Version,
		ManifestDigest:    desc.Digest.String(),
		OriginalReference: ref.Raw,
		Registry:          ref.Registry,
		Repository:        ref.Repository,
		Tag:               ref.Tag,
		RootDirectory:     filepath.Base(rootDir),
		CreatedAt:         time.Now().UTC().Format(time.RFC3339),
	}

	if err := finalizeCachedArtifact(tempArtifactDir, artifact); err != nil {
		return CachedArtifact{}, err
	}
	if !ref.IsDigest {
		if err := writeRemoteTagReference(ref, artifact); err != nil {
			return CachedArtifact{}, err
		}
	}

	return artifact, nil
}

func createCachedArtifact(manifest CatalogManifest, catalogRoot string, artifact CachedArtifact) (CachedArtifact, error) {
	tempArtifactDir, cleanup, err := newTempArtifactDir()
	if err != nil {
		return CachedArtifact{}, err
	}
	defer cleanup()

	ctx := context.Background()
	fileStore, err := file.New(tempArtifactDir)
	if err != nil {
		return CachedArtifact{}, fmt.Errorf("create file store: %w", err)
	}
	defer fileStore.Close()
	fileStore.TarReproducible = true

	layerDescriptor, err := fileStore.Add(ctx, manifest.Metadata.Name, CatalogLayerMediaType, catalogRoot)
	if err != nil {
		return CachedArtifact{}, fmt.Errorf("package catalog directory: %w", err)
	}

	packOptions := oras.PackManifestOptions{
		Layers: []v1.Descriptor{layerDescriptor},
		ManifestAnnotations: map[string]string{
			"io.kubara.catalog.name":    manifest.Metadata.Name,
			"io.kubara.catalog.version": manifest.Spec.Version,
		},
	}
	manifestDescriptor, err := oras.PackManifest(ctx, fileStore, oras.PackManifestVersion1_1, CatalogArtifactType, packOptions)
	if err != nil {
		return CachedArtifact{}, fmt.Errorf("pack catalog manifest: %w", err)
	}
	if err := fileStore.Tag(ctx, manifestDescriptor, manifest.Spec.Version); err != nil {
		return CachedArtifact{}, fmt.Errorf("tag packaged catalog: %w", err)
	}

	layoutDir := filepath.Join(tempArtifactDir, "layout")
	layoutStore, err := oci.New(layoutDir)
	if err != nil {
		return CachedArtifact{}, fmt.Errorf("create OCI layout store: %w", err)
	}
	if err := oras.CopyGraph(ctx, fileStore, layoutStore, manifestDescriptor, oras.DefaultCopyGraphOptions); err != nil {
		return CachedArtifact{}, fmt.Errorf("copy packaged catalog into OCI layout: %w", err)
	}
	if err := layoutStore.Tag(ctx, manifestDescriptor, manifest.Spec.Version); err != nil {
		return CachedArtifact{}, fmt.Errorf("tag OCI layout with %q: %w", manifest.Spec.Version, err)
	}

	contentsDir := filepath.Join(tempArtifactDir, "contents")
	rootDir, err := extractCatalogContents(ctx, layoutStore, manifestDescriptor, contentsDir)
	if err != nil {
		return CachedArtifact{}, err
	}

	artifact.ManifestDigest = manifestDescriptor.Digest.String()
	artifact.RootDirectory = filepath.Base(rootDir)
	if artifact.CreatedAt == "" {
		artifact.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	if err := finalizeCachedArtifact(tempArtifactDir, artifact); err != nil {
		return CachedArtifact{}, err
	}

	return artifact, nil
}

func extractCatalogContents(ctx context.Context, layoutStore *oci.Store, desc v1.Descriptor, contentsDir string) (string, error) {
	if err := os.MkdirAll(contentsDir, 0o755); err != nil {
		return "", fmt.Errorf("create extracted catalog directory: %w", err)
	}

	fileStore, err := file.New(contentsDir)
	if err != nil {
		return "", fmt.Errorf("create extracted file store: %w", err)
	}
	defer fileStore.Close()

	if err := oras.CopyGraph(ctx, layoutStore, fileStore, desc, oras.DefaultCopyGraphOptions); err != nil {
		return "", fmt.Errorf("extract catalog contents: %w", err)
	}

	entries, err := os.ReadDir(contentsDir)
	if err != nil {
		return "", fmt.Errorf("read extracted catalog directory: %w", err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return "", fmt.Errorf("expected packaged catalog to extract into a single root directory")
	}

	return filepath.Join(contentsDir, entries[0].Name()), nil
}

func resolveServicesPath(catalogPath string) (string, error) {
	cleaned := filepath.Clean(catalogPath)

	rootInfo, err := os.Stat(cleaned)
	if err != nil {
		return "", fmt.Errorf("catalog directory %q does not exist: %w", cleaned, err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("catalog path %q is not a directory", cleaned)
	}

	servicesDir := filepath.Join(cleaned, servicesDirectory)
	if servicesInfo, err := os.Stat(servicesDir); err == nil {
		if !servicesInfo.IsDir() {
			return "", fmt.Errorf("catalog services path %q is not a directory", servicesDir)
		}
		return servicesDir, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return cleaned, nil
	}
	return "", fmt.Errorf("stat catalog services path %q: %w", servicesDir, err)
}

func newRemoteRepository(ref OCIReference, registryConfigPath string, insecure bool) (*remote.Repository, error) {
	repo, err := remote.NewRepository(fmt.Sprintf("%s/%s", ref.Registry, ref.Repository))
	if err != nil {
		return nil, fmt.Errorf("create remote repository for %q: %w", ref.Raw, err)
	}

	credentialFunc, err := newCredentialFunc(registryConfigPath)
	if err != nil {
		return nil, err
	}
	repo.Client = &auth.Client{
		Client:     newRegistryHTTPClient(insecure),
		Cache:      auth.NewCache(),
		Credential: credentialFunc,
	}

	return repo, nil
}

func newRegistryHTTPClient(insecure bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	return &http.Client{
		Transport: retry.NewTransport(transport),
	}
}

func newCredentialFunc(registryConfigPath string) (auth.CredentialFunc, error) {
	if strings.TrimSpace(registryConfigPath) == "" {
		defaultPath, err := defaultDockerConfigPath()
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(defaultPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("stat default registry config %q: %w", defaultPath, err)
		}
		store, err := credentials.NewStore(defaultPath, credentials.StoreOptions{})
		if err != nil {
			return nil, fmt.Errorf("create registry credential store from %q: %w", defaultPath, err)
		}
		return credentials.Credential(store), nil
	}

	store, err := credentials.NewStore(registryConfigPath, credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("create registry credential store from %q: %w", registryConfigPath, err)
	}
	return credentials.Credential(store), nil
}

func defaultDockerConfigPath() (string, error) {
	if dockerConfigDir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dockerConfigDir != "" {
		return filepath.Join(dockerConfigDir, "config.json"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, defaultDockerConfigRel), nil
}

func defaultCatalogCacheRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, defaultCatalogCacheRel), nil
}

func newTempArtifactDir() (string, func(), error) {
	cacheRoot, err := defaultCatalogCacheRoot()
	if err != nil {
		return "", nil, err
	}
	tempRoot := filepath.Join(cacheRoot, ".tmp")
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		return "", nil, fmt.Errorf("create temporary catalog cache directory: %w", err)
	}
	tempArtifactDir, err := os.MkdirTemp(tempRoot, "artifact-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary artifact directory: %w", err)
	}
	return tempArtifactDir, func() { _ = os.RemoveAll(tempArtifactDir) }, nil
}

func finalizeCachedArtifact(tempArtifactDir string, artifact CachedArtifact) error {
	finalDir, err := artifactDirPath(artifact.ManifestDigest)
	if err != nil {
		return err
	}

	if err := writeArtifactMetadata(filepath.Join(tempArtifactDir, "metadata.json"), artifact); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(finalDir), 0o755); err != nil {
		return fmt.Errorf("create catalog artifact cache directory: %w", err)
	}
	if _, err := os.Stat(finalDir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat catalog artifact cache directory %q: %w", finalDir, err)
	}

	if err := os.Rename(tempArtifactDir, finalDir); err != nil {
		if _, statErr := os.Stat(finalDir); statErr == nil {
			return nil
		}
		return fmt.Errorf("move catalog artifact into cache: %w", err)
	}
	return nil
}

func writeArtifactMetadata(path string, artifact CachedArtifact) error {
	raw, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cached artifact metadata: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write cached artifact metadata: %w", err)
	}
	return nil
}

func artifactDirPath(manifestDigest string) (string, error) {
	cacheRoot, err := defaultCatalogCacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheRoot, "artifacts", digestPathComponent(manifestDigest)), nil
}

func artifactLayoutPath(artifact CachedArtifact) string {
	dir, _ := artifactDirPath(artifact.ManifestDigest)
	return filepath.Join(dir, "layout")
}

func artifactRootPath(artifact CachedArtifact) string {
	dir, _ := artifactDirPath(artifact.ManifestDigest)
	return filepath.Join(dir, "contents", artifact.RootDirectory)
}

func findArtifactByDigest(dgst digest.Digest) (CachedArtifact, bool, error) {
	path, err := artifactDirPath(dgst.String())
	if err != nil {
		return CachedArtifact{}, false, err
	}
	artifact, found, err := readArtifactMetadata(path)
	if err != nil {
		return CachedArtifact{}, false, err
	}
	return artifact, found, nil
}

func findTagArtifact(ref OCIReference) (CachedArtifact, bool, error) {
	refPath, err := tagReferencePath(ref)
	if err != nil {
		return CachedArtifact{}, false, err
	}
	cachedRef, found, err := readCachedReferenceFile(refPath)
	if err != nil || !found {
		return CachedArtifact{}, false, err
	}

	artifact, found, err := findArtifactByDigest(digest.Digest(cachedRef.ManifestDigest))
	if err != nil || !found {
		return CachedArtifact{}, false, err
	}
	return artifact, true, nil
}

func readArtifactMetadata(artifactDir string) (CachedArtifact, bool, error) {
	path := filepath.Join(artifactDir, "metadata.json")
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CachedArtifact{}, false, nil
		}
		return CachedArtifact{}, false, fmt.Errorf("read cached artifact metadata %q: %w", path, err)
	}

	var artifact CachedArtifact
	if err := json.Unmarshal(content, &artifact); err != nil {
		return CachedArtifact{}, false, fmt.Errorf("unmarshal cached artifact metadata %q: %w", path, err)
	}
	if _, err := os.Stat(filepath.Join(artifactDir, "layout", "index.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CachedArtifact{}, false, nil
		}
		return CachedArtifact{}, false, fmt.Errorf("stat cached OCI layout for %q: %w", artifactDir, err)
	}
	if _, err := os.Stat(filepath.Join(artifactDir, "contents", artifact.RootDirectory, "Catalog.yaml")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CachedArtifact{}, false, nil
		}
		return CachedArtifact{}, false, fmt.Errorf("stat cached catalog contents for %q: %w", artifactDir, err)
	}

	return artifact, true, nil
}

func writeLocalReference(ref OCIReference, artifact CachedArtifact) error {
	refPath, err := tagReferencePath(ref)
	if err != nil {
		return err
	}
	return writeReferenceFile(refPath, cachedReference{
		SchemaVersion:  cacheSchemaVersion,
		ManifestDigest: artifact.ManifestDigest,
		CatalogName:    artifact.CatalogName,
		CatalogVersion: artifact.CatalogVersion,
		Reference:      ref.Raw,
		Registry:       ref.Registry,
		Repository:     ref.Repository,
		Tag:            ref.Tag,
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
	})
}

func writeRemoteTagReference(ref OCIReference, artifact CachedArtifact) error {
	refPath, err := tagReferencePath(ref)
	if err != nil {
		return err
	}
	return writeReferenceFile(refPath, cachedReference{
		SchemaVersion:  cacheSchemaVersion,
		ManifestDigest: artifact.ManifestDigest,
		CatalogName:    artifact.CatalogName,
		CatalogVersion: artifact.CatalogVersion,
		Reference:      ref.Raw,
		Registry:       ref.Registry,
		Repository:     ref.Repository,
		Tag:            ref.Tag,
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
	})
}

func writeReferenceFile(path string, ref cachedReference) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create cached catalog reference directory: %w", err)
	}

	raw, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cached catalog reference: %w", err)
	}

	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, raw, 0o600); err != nil {
		return fmt.Errorf("write cached catalog reference %q: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("move cached catalog reference into place: %w", err)
	}
	return nil
}

func tagReferencePath(ref OCIReference) (string, error) {
	cacheRoot, err := defaultCatalogCacheRoot()
	if err != nil {
		return "", err
	}

	repositoryParts := strings.Split(ref.Repository, "/")
	pathParts := []string{cacheRoot, "refs", sanitizePathComponent(ref.Registry)}
	for _, part := range repositoryParts {
		pathParts = append(pathParts, sanitizePathComponent(part))
	}
	pathParts = append(pathParts, "tags", ref.Tag+".json")
	return filepath.Join(pathParts...), nil
}

func sanitizePathComponent(value string) string {
	replacer := strings.NewReplacer(":", "_", "@", "_", "\\", "_")
	return replacer.Replace(value)
}

func digestPathComponent(manifestDigest string) string {
	return strings.ReplaceAll(manifestDigest, ":", "-")
}

func isLocalhostReference(ref OCIReference) bool {
	return ref.Registry == "localhost"
}
