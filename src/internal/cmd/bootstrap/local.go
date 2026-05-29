package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kubara-io/kubara/internal/catalog"
	generatecmd "github.com/kubara-io/kubara/internal/cmd/generate"
	"github.com/kubara-io/kubara/internal/config"
	"github.com/kubara-io/kubara/internal/envconfig"
	"github.com/kubara-io/kubara/internal/helm"
	"github.com/kubara-io/kubara/internal/k8s"
	"github.com/kubara-io/kubara/internal/localmode"
	"github.com/kubara-io/kubara/internal/render"
	"github.com/kubara-io/kubara/internal/utils"

	"github.com/rs/zerolog/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	localDirectoryName          = ".local"
	localKindConfigName         = "kind-config.yaml"
	localKindHostCACertPath     = "/etc/ssl/certs/ca-certificates.crt"
	localTraefikNamespace       = "traefik"
	localTraefikReleaseName     = "traefik"
	localOpenBaoNamespace       = "openbao"
	localOpenBaoReleaseName     = "openbao"
	localOpenBaoPodName         = "openbao-0"
	localOpenBaoOIDCClientName  = "argocd-dex"
	localOAuth2ProxyOIDCClient  = "oauth2-proxy"
	localOpenBaoUserName        = "wizard"
	localExternalSecretsRole    = "any-sa"
	localExternalSecretsPolicy  = "read-access"
	localOpenBaoInternalAddress = "http://openbao.openbao.svc:8200"
)

type localOIDCCredentials struct {
	ClientID     string
	ClientSecret string
}

type localOAuth2ProxyCredentials struct {
	ClientID     string
	ClientSecret string
	CookieSecret string
}

func prepareLocalBootstrap(ctx context.Context, opts *Options) error {
	if opts.ClusterConfig == nil {
		return fmt.Errorf("cluster config is required for --local")
	}

	log.Info().
		Str("cluster", opts.ClusterName).
		Str("runtimeDir", filepath.Join(opts.WorkDir, localDirectoryName)).
		Msg("Preparing local evaluation bootstrap")

	if err := utils.AddGitignore(opts.WorkDir); err != nil {
		return fmt.Errorf("ensure .gitignore contains kubara local entries: %w", err)
	}

	state := newLocalState(opts)
	opts.LocalState = state

	log.Info().Msg("Checking local bootstrap prerequisites")
	if err := ensureLocalPrerequisites(); err != nil {
		return err
	}
	if err := os.MkdirAll(state.RuntimeDir, 0o750); err != nil {
		return fmt.Errorf("create local runtime directory: %w", err)
	}

	log.Info().
		Str("cluster", opts.ClusterName).
		Str("kubeconfig", state.KubeconfigPath).
		Msg("Ensuring local kind cluster")
	log.Info().
		Str("configFile", state.KindConfigPath).
		Msg("Writing local kind configuration")
	if err := writeLocalKindConfig(state); err != nil {
		return err
	}
	if err := ensureKindCluster(ctx, opts.ClusterName, state.KubeconfigPath, state.KindConfigPath); err != nil {
		return err
	}

	log.Info().
		Str("valuesFile", state.TraefikValuesPath).
		Msg("Writing local Traefik values")
	if err := writeLocalTraefikValues(state); err != nil {
		return err
	}
	log.Info().Msg("Installing local Traefik")
	if err := installLocalTraefik(ctx, state.KubeconfigPath, state.TraefikValuesPath, opts.Timeout); err != nil {
		return err
	}
	log.Info().
		Str("logFile", state.CloudProviderLogPath).
		Msg("Starting cloud-provider-kind for local LoadBalancer and ingress support")
	if err := ensureCloudProviderKindRunning(state); err != nil {
		return err
	}

	log.Info().Msg("Connecting to the local Kubernetes cluster")
	client, err := k8s.NewClient(k8s.Config{
		KubeconfigPath: state.KubeconfigPath,
		QPS:            50,
		Burst:          100,
		Timeout:        30 * time.Second,
		UserAgent:      "kubara-local-bootstrap",
	})
	if err != nil {
		return fmt.Errorf("create local kubernetes client: %w", err)
	}

	log.Info().Msg("Waiting for the local Traefik LoadBalancer IP")
	loadBalancerIP, err := waitForTraefikLoadBalancer(ctx, client)
	if err != nil {
		return err
	}
	state.LoadBalancerIP = loadBalancerIP
	state.BaseHost = fmt.Sprintf("%s.traefik.me", loadBalancerIP)
	state.OpenBaoHost = fmt.Sprintf("openbao.%s", state.BaseHost)
	state.OIDCUsername = localOpenBaoUserName
	state.OIDCPassword = opts.EnvMap.ArgocdWizardAccountPassword
	log.Info().
		Str("loadBalancerIP", state.LoadBalancerIP).
		Str("baseHost", state.BaseHost).
		Str("openBaoHost", state.OpenBaoHost).
		Msg("Local ingress hostnames are ready")

	log.Info().
		Str("configFile", opts.ConfigFilePath).
		Msg("Updating config.yaml for the local evaluation profile")
	if err := updateLocalClusterConfigAndGenerate(opts, state); err != nil {
		return err
	}
	log.Info().
		Str("valuesFile", state.OpenBaoValuesPath).
		Msg("Writing local OpenBao values")
	if err := writeLocalOpenBaoValues(state); err != nil {
		return err
	}
	log.Info().Msg("Installing local OpenBao")
	if err := installLocalOpenBao(ctx, state.KubeconfigPath, state.OpenBaoValuesPath, opts.Timeout); err != nil {
		return err
	}
	log.Info().Msg("Waiting for OpenBao to be ready for configuration")
	if err := waitForLocalOpenBaoReady(ctx, client, state.KubeconfigPath); err != nil {
		return err
	}

	log.Info().Msg("Configuring OpenBao for local OIDC and external-secrets")
	oidcCredentials, _, err := configureLocalOpenBao(ctx, state.KubeconfigPath, state)
	if err != nil {
		return err
	}
	log.Info().
		Str("clusterSecretStore", state.ClusterSecretStorePath).
		Msg("Writing local ClusterSecretStore manifest")
	if err := writeLocalClusterSecretStore(opts, state); err != nil {
		return err
	}
	log.Info().
		Str("valuesFile", state.ArgocdValuesPath).
		Msg("Writing local Argo CD overrides")
	if err := writeLocalArgocdValues(opts, state, oidcCredentials); err != nil {
		return err
	}
	log.Info().
		Str("valuesFile", state.OAuth2ProxyValuesPath).
		Msg("Writing local oauth2-proxy overrides")
	if err := writeLocalOAuth2ProxyValues(state); err != nil {
		return err
	}

	opts.WithES = true
	opts.WithProm = true
	opts.WithESCSSPath = state.ClusterSecretStorePath
	opts.ClusterConfig.DNSName = state.BaseHost

	log.Info().Msg("Local bootstrap preparation completed")
	return nil
}

func newLocalState(opts *Options) *LocalState {
	repoLocalRuntime := filepath.Join(opts.WorkDir, localDirectoryName)

	return &LocalState{
		RuntimeDir:             repoLocalRuntime,
		KubeconfigPath:         filepath.Join(repoLocalRuntime, "kind.kubeconfig"),
		KindConfigPath:         filepath.Join(repoLocalRuntime, localKindConfigName),
		TraefikValuesPath:      filepath.Join(repoLocalRuntime, "traefik", "values.yaml"),
		OpenBaoValuesPath:      filepath.Join(repoLocalRuntime, "openbao", "values.yaml"),
		ArgocdValuesPath:       filepath.Join(repoLocalRuntime, "argocd", "bootstrap-values.yaml"),
		OAuth2ProxyValuesPath:  filepath.Join(opts.OverlayValues, "helm", opts.ClusterName, "oauth2-proxy", "additional-values.yaml"),
		ClusterSecretStorePath: filepath.Join(repoLocalRuntime, "external-secrets", "clustersecretstore.yaml"),
		CloudProviderLogPath:   filepath.Join(repoLocalRuntime, "cloud-provider-kind.log"),
		CloudProviderPIDPath:   filepath.Join(repoLocalRuntime, "cloud-provider-kind.pid"),
	}
}

func ensureLocalPrerequisites() error {
	for _, command := range []string{"kind", "docker", "kubectl", "helm", "cloud-provider-kind", "sudo"} {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("%q is required for --local and must be available on PATH", command)
		}
	}
	return nil
}

func ensureKindCluster(ctx context.Context, clusterName, kubeconfigPath, kindConfigPath string) error {
	output, err := runCommand(ctx, "kind", nil, "", "get", "clusters")
	if err != nil {
		return fmt.Errorf("list kind clusters: %w", err)
	}

	clusterExists := false
	for _, existingCluster := range strings.Fields(string(output)) {
		if existingCluster == clusterName {
			clusterExists = true
			break
		}
	}

	env := map[string]string{"KUBECONFIG": kubeconfigPath}
	if clusterExists {
		log.Info().Str("cluster", clusterName).Msg("Reusing existing kind cluster")
		log.Warn().
			Str("cluster", clusterName).
			Msg("Existing kind clusters keep their original extraMounts. Recreate the cluster if you need updated kind mount configuration.")
		if _, err := runCommand(ctx, "kind", env, "", "export", "kubeconfig", "--name", clusterName); err != nil {
			return fmt.Errorf("export kubeconfig for kind cluster %q: %w", clusterName, err)
		}
		return nil
	}

	log.Info().Str("cluster", clusterName).Msg("Creating kind cluster")
	if _, err := runCommand(ctx, "kind", env, "", "create", "cluster", "--name", clusterName, "--config", kindConfigPath); err != nil {
		return fmt.Errorf("create kind cluster %q: %w", clusterName, err)
	}
	return nil
}

func writeLocalKindConfig(state *LocalState) error {
	content := fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: %s
    containerPath: %s
    readOnly: true
`, localKindHostCACertPath, localKindHostCACertPath)
	return writeLocalFile(state.KindConfigPath, content)
}

func installLocalTraefik(ctx context.Context, kubeconfigPath, valuesPath string, timeout time.Duration) error {
	repo := helm.RepoOptions{Name: "traefik", URL: "https://traefik.github.io/charts"}
	if err := helm.AddRepository(ctx, repo); err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("add Traefik helm repository: %w", err)
	}
	if err := helm.UpdateRepository(ctx, repo); err != nil {
		return fmt.Errorf("update Traefik helm repository: %w", err)
	}
	return helmUpgradeInstall(ctx, kubeconfigPath, localTraefikReleaseName, "traefik/traefik", localTraefikNamespace, valuesPath, timeout)
}

func installLocalOpenBao(ctx context.Context, kubeconfigPath, valuesPath string, timeout time.Duration) error {
	repo := helm.RepoOptions{Name: "openbao", URL: "https://openbao.github.io/openbao-helm"}
	if err := helm.AddRepository(ctx, repo); err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("add OpenBao helm repository: %w", err)
	}
	if err := helm.UpdateRepository(ctx, repo); err != nil {
		return fmt.Errorf("update OpenBao helm repository: %w", err)
	}
	return helmUpgradeInstall(ctx, kubeconfigPath, localOpenBaoReleaseName, "openbao/openbao", localOpenBaoNamespace, valuesPath, timeout)
}

func waitForLocalOpenBaoReady(ctx context.Context, client *k8s.Client, kubeconfigPath string) error {
	if err := client.WaitForPod(ctx, localOpenBaoNamespace, fmt.Sprintf("statefulset.kubernetes.io/pod-name=%s", localOpenBaoPodName)); err != nil {
		return fmt.Errorf("wait for OpenBao pod to be ready: %w", err)
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastErr error

	for {
		if _, err := kubectlExec(ctx, kubeconfigPath,
			"-n", localOpenBaoNamespace,
			"exec", localOpenBaoPodName,
			"--",
			"bao", "status",
		); err == nil {
			log.Info().Msg("OpenBao is ready for configuration")
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("wait for OpenBao command readiness after last error %v: %w", lastErr, ctx.Err())
			}
			return fmt.Errorf("wait for OpenBao command readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func helmUpgradeInstall(ctx context.Context, kubeconfigPath, releaseName, chartRef, namespace, valuesPath string, timeout time.Duration) error {
	args := []string{
		"upgrade",
		"--install",
		releaseName,
		chartRef,
		"--namespace",
		namespace,
		"--create-namespace",
		"--wait",
		"--kubeconfig",
		kubeconfigPath,
	}
	if valuesPath != "" {
		args = append(args, "--values", valuesPath)
	}
	if timeout > 0 {
		args = append(args, "--timeout", timeout.String())
	}
	if _, err := runCommand(ctx, "helm", nil, "", args...); err != nil {
		return fmt.Errorf("helm upgrade --install %q: %w", releaseName, err)
	}
	return nil
}

func ensureCloudProviderKindRunning(state *LocalState) error {
	if pid, ok := readPID(state.CloudProviderPIDPath); ok && processRunning(pid, "cloud-provider-kind") {
		log.Info().
			Int("pid", pid).
			Str("logFile", state.CloudProviderLogPath).
			Msg("Reusing running cloud-provider-kind process")
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(state.CloudProviderLogPath), 0o750); err != nil {
		return fmt.Errorf("create cloud-provider-kind runtime directory: %w", err)
	}

	fmt.Fprintln(os.Stderr, "Local bootstrap needs elevated privileges to start cloud-provider-kind for local LoadBalancer/ingress support. sudo may ask for your password.")

	authorizeCmd := exec.Command("sudo", "-v")
	authorizeCmd.Stdin = os.Stdin
	authorizeCmd.Stdout = os.Stdout
	authorizeCmd.Stderr = os.Stderr
	if err := authorizeCmd.Run(); err != nil {
		return fmt.Errorf("request elevated privileges for cloud-provider-kind: %w", err)
	}
	log.Info().Msg("Elevated privileges granted for cloud-provider-kind startup")

	command := fmt.Sprintf(
		"umask 022; nohup cloud-provider-kind >> %s 2>&1 < /dev/null & echo $! > %s; chmod 0644 %s %s",
		shellQuote(state.CloudProviderLogPath),
		shellQuote(state.CloudProviderPIDPath),
		shellQuote(state.CloudProviderLogPath),
		shellQuote(state.CloudProviderPIDPath),
	)

	cmd := exec.Command("sudo", "-n", "-b", "sh", "-c", command)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cloud-provider-kind with elevated privileges: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("detach cloud-provider-kind launcher: %w", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pid, ok := readPID(state.CloudProviderPIDPath)
		if ok && processRunning(pid, "cloud-provider-kind") {
			log.Info().
				Int("pid", pid).
				Str("logFile", state.CloudProviderLogPath).
				Msg("cloud-provider-kind is running")
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("cloud-provider-kind did not start successfully; check %s", state.CloudProviderLogPath)
}

func readPID(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return pid, true
}

func processRunning(pid int, expectedCommand string) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}

	output, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), expectedCommand)
}

func waitForTraefikLoadBalancer(ctx context.Context, client *k8s.Client) (string, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		serviceList, err := client.Clientset.CoreV1().Services(localTraefikNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/instance=traefik-%s", localTraefikReleaseName),
		})
		if err != nil {
			return "", fmt.Errorf("list Traefik services: %w", err)
		}
		for _, svc := range serviceList.Items {
			for _, ingress := range svc.Status.LoadBalancer.Ingress {
				if strings.TrimSpace(ingress.IP) != "" {
					log.Info().
						Str("service", svc.Name).
						Str("ip", ingress.IP).
						Msg("Traefik LoadBalancer IP assigned")
					return ingress.IP, nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for Traefik LoadBalancer IP: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func updateLocalClusterConfigAndGenerate(opts *Options, state *LocalState) error {
	configStore := config.NewConfigStoreWithCatalog(opts.ConfigFilePath, catalog.LoadOptions{
		CatalogPath: opts.CatalogPath,
		Overwrite:   opts.CatalogOverwrite,
	})
	if err := configStore.Load(); err != nil {
		return fmt.Errorf("load config for local profile update: %w", err)
	}

	var clusterConfig *config.Cluster
	for i := range configStore.GetConfig().Clusters {
		if configStore.GetConfig().Clusters[i].Name == opts.ClusterName {
			clusterConfig = &configStore.GetConfig().Clusters[i]
			break
		}
	}
	if clusterConfig == nil {
		return fmt.Errorf("cluster %q not found in config file %q", opts.ClusterName, opts.ConfigFilePath)
	}

	localmode.ApplyClusterProfile(clusterConfig, state.BaseHost)
	if err := configStore.SaveToFile(); err != nil {
		return fmt.Errorf("save config with local profile: %w", err)
	}
	opts.ClusterConfig = clusterConfig

	localEnvPath, err := writeLocalGenerateEnvFile(state, opts.EnvMap)
	if err != nil {
		return err
	}

	log.Info().
		Str("envFile", localEnvPath).
		Msg("Regenerating local Helm artifacts")
	generateOptions := &generatecmd.Options{
		TemplateType:       render.Helm,
		DryRun:             false,
		CWD:                opts.WorkDir,
		ConfigFilePath:     opts.ConfigFilePath,
		CatalogPath:        opts.CatalogPath,
		CatalogOverwrite:   opts.CatalogOverwrite,
		ManagedCatalogPath: opts.ManagedCatalog,
		OverlayValuesPath:  opts.OverlayValues,
		EnvPath:            localEnvPath,
	}
	if err := generateOptions.Run(); err != nil {
		return fmt.Errorf("generate Helm catalog for local profile: %w", err)
	}
	return nil
}

func writeLocalGenerateEnvFile(state *LocalState, envMap *envconfig.EnvMap) (string, error) {
	envPath := filepath.Join(state.RuntimeDir, "generate.env")
	store := envconfig.NewEnvStore(envPath, ".", "")
	store.SetEnvMap(*envMap)
	if err := store.SaveToFile(envPath); err != nil {
		return "", fmt.Errorf("write local env file for generation: %w", err)
	}
	return envPath, nil
}

func configureLocalOpenBao(ctx context.Context, kubeconfigPath string, state *LocalState) (localOIDCCredentials, localOAuth2ProxyCredentials, error) {
	if err := ensureOpenBaoSecretEngine(ctx, kubeconfigPath); err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, err
	}
	if err := ensureOpenBaoAuthMethod(ctx, kubeconfigPath, "kubernetes"); err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, err
	}
	if err := ensureOpenBaoAuthMethod(ctx, kubeconfigPath, "userpass"); err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, err
	}

	if _, err := kubectlExec(ctx, kubeconfigPath,
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "write", "auth/kubernetes/config",
		"token_reviewer_jwt=@/var/run/secrets/kubernetes.io/serviceaccount/token",
		"kubernetes_host=https://kubernetes.default.svc:443",
		"kubernetes_ca_cert=@/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
	); err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, fmt.Errorf("configure OpenBao kubernetes auth: %w", err)
	}

	policyDocument := `path "kv/data/*" {
  capabilities = ["read", "list"]
}

path "kv/metadata/*" {
  capabilities = ["read", "list"]
}
`
	if _, err := kubectlExecWithInput(ctx, kubeconfigPath, policyDocument,
		"-n", localOpenBaoNamespace,
		"exec", "-i", localOpenBaoPodName,
		"--",
		"bao", "policy", "write", localExternalSecretsPolicy, "-",
	); err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, fmt.Errorf("configure OpenBao policy: %w", err)
	}

	if _, err := kubectlExec(ctx, kubeconfigPath,
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "write",
		fmt.Sprintf("auth/kubernetes/role/%s", localExternalSecretsRole),
		"bound_service_account_names=*",
		"bound_service_account_namespaces=*",
		fmt.Sprintf("policies=%s", localExternalSecretsPolicy),
		"ttl=24h",
	); err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, fmt.Errorf("configure OpenBao kubernetes role: %w", err)
	}

	if _, err := kubectlExec(ctx, kubeconfigPath,
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "write",
		fmt.Sprintf("auth/userpass/users/%s", state.OIDCUsername),
		fmt.Sprintf("password=%s", state.OIDCPassword),
	); err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, fmt.Errorf("configure local OpenBao user: %w", err)
	}

	argocdCredentials, err := ensureOpenBaoOIDCClientCredentials(ctx, kubeconfigPath, localOpenBaoOIDCClientName, fmt.Sprintf("http://%s/argocd/api/dex/callback", state.BaseHost))
	if err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, err
	}
	oauth2ProxyCredentials, err := newLocalOAuth2ProxyCredentials(ctx, kubeconfigPath, state)
	if err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, err
	}
	if err := writeOpenBaoKVSecret(ctx, kubeconfigPath, "argo_oauth2_credentials", map[string]string{
		"client-id":     argocdCredentials.ClientID,
		"client-secret": argocdCredentials.ClientSecret,
	}); err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, err
	}
	if err := writeOpenBaoKVSecret(ctx, kubeconfigPath, "oauth2_credentials", map[string]string{
		"client-id":     oauth2ProxyCredentials.ClientID,
		"client-secret": oauth2ProxyCredentials.ClientSecret,
		"cookie-secret": oauth2ProxyCredentials.CookieSecret,
	}); err != nil {
		return localOIDCCredentials{}, localOAuth2ProxyCredentials{}, err
	}

	return argocdCredentials, oauth2ProxyCredentials, nil
}

func newLocalOAuth2ProxyCredentials(ctx context.Context, kubeconfigPath string, state *LocalState) (localOAuth2ProxyCredentials, error) {
	credentials, err := ensureOpenBaoOIDCClientCredentials(ctx, kubeconfigPath, localOAuth2ProxyOIDCClient, fmt.Sprintf("http://%s/oauth2/callback", state.BaseHost))
	if err != nil {
		return localOAuth2ProxyCredentials{}, err
	}
	cookieSecret, err := generateCookieSecret()
	if err != nil {
		return localOAuth2ProxyCredentials{}, fmt.Errorf("generate oauth2-proxy cookie secret: %w", err)
	}
	return localOAuth2ProxyCredentials{
		ClientID:     credentials.ClientID,
		ClientSecret: credentials.ClientSecret,
		CookieSecret: cookieSecret,
	}, nil
}

func ensureOpenBaoOIDCClientCredentials(ctx context.Context, kubeconfigPath, clientName, redirectURI string) (localOIDCCredentials, error) {
	if _, err := kubectlExec(ctx, kubeconfigPath,
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "write",
		fmt.Sprintf("identity/oidc/client/%s", clientName),
		fmt.Sprintf("redirect_uris=%s", redirectURI),
		"assignments=allow_all",
	); err != nil {
		return localOIDCCredentials{}, fmt.Errorf("configure OpenBao OIDC client %q: %w", clientName, err)
	}

	rawClient, err := kubectlExec(ctx, kubeconfigPath,
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "read", "-format=json",
		fmt.Sprintf("identity/oidc/client/%s", clientName),
	)
	if err != nil {
		return localOIDCCredentials{}, fmt.Errorf("read OpenBao OIDC client credentials for %q: %w", clientName, err)
	}

	type openBaoReadResponse struct {
		Data struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		} `json:"data"`
	}
	var response openBaoReadResponse
	if err := json.Unmarshal(rawClient, &response); err != nil {
		return localOIDCCredentials{}, fmt.Errorf("decode OpenBao OIDC client credentials for %q: %w", clientName, err)
	}
	if response.Data.ClientID == "" || response.Data.ClientSecret == "" {
		return localOIDCCredentials{}, fmt.Errorf("OpenBao OIDC client credentials for %q are incomplete", clientName)
	}

	return localOIDCCredentials{
		ClientID:     response.Data.ClientID,
		ClientSecret: response.Data.ClientSecret,
	}, nil
}

func writeOpenBaoKVSecret(ctx context.Context, kubeconfigPath, remoteKey string, fields map[string]string) error {
	args := []string{
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "kv", "put", fmt.Sprintf("kv/%s", remoteKey),
	}
	for key, value := range fields {
		args = append(args, fmt.Sprintf("%s=%s", key, value))
	}
	if _, err := kubectlExec(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("write OpenBao secret %q: %w", remoteKey, err)
	}
	return nil
}

func generateCookieSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func ensureOpenBaoSecretEngine(ctx context.Context, kubeconfigPath string) error {
	raw, err := kubectlExec(ctx, kubeconfigPath,
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "secrets", "list", "-format=json",
	)
	if err != nil {
		return fmt.Errorf("list OpenBao secrets engines: %w", err)
	}

	var mounts map[string]any
	if err := json.Unmarshal(raw, &mounts); err != nil {
		return fmt.Errorf("decode OpenBao secrets engines: %w", err)
	}
	if _, exists := mounts["kv/"]; exists {
		return nil
	}

	if _, err := kubectlExec(ctx, kubeconfigPath,
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "secrets", "enable", "-path=kv", "kv-v2",
	); err != nil {
		return fmt.Errorf("enable OpenBao kv secrets engine: %w", err)
	}
	return nil
}

func ensureOpenBaoAuthMethod(ctx context.Context, kubeconfigPath, method string) error {
	raw, err := kubectlExec(ctx, kubeconfigPath,
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "auth", "list", "-format=json",
	)
	if err != nil {
		return fmt.Errorf("list OpenBao auth methods: %w", err)
	}

	var authMethods map[string]any
	if err := json.Unmarshal(raw, &authMethods); err != nil {
		return fmt.Errorf("decode OpenBao auth methods: %w", err)
	}
	if _, exists := authMethods[method+"/"]; exists {
		return nil
	}

	if _, err := kubectlExec(ctx, kubeconfigPath,
		"-n", localOpenBaoNamespace,
		"exec", localOpenBaoPodName,
		"--",
		"bao", "auth", "enable", method,
	); err != nil {
		return fmt.Errorf("enable OpenBao auth method %q: %w", method, err)
	}
	return nil
}

func kubectlExec(ctx context.Context, kubeconfigPath string, args ...string) ([]byte, error) {
	return runCommand(ctx, "kubectl", map[string]string{"KUBECONFIG": kubeconfigPath}, "", args...)
}

func kubectlExecWithInput(ctx context.Context, kubeconfigPath, input string, args ...string) ([]byte, error) {
	return runCommand(ctx, "kubectl", map[string]string{"KUBECONFIG": kubeconfigPath}, input, args...)
}

func writeLocalTraefikValues(state *LocalState) error {
	content := `api:
  dashboard: true
`
	return writeLocalFile(state.TraefikValuesPath, content)
}

func writeLocalOpenBaoValues(state *LocalState) error {
	content := fmt.Sprintf(`server:
  dev:
    enabled: true
    devRootToken: root
  ha:
    apiAddr: "%s"
  ingress:
    enabled: true
    ingressClassName: traefik
    activeService: false
    hosts:
      - host: %s
        paths:
          - /
    tls:
      - hosts:
          - %s
  extraEnvironmentVars:
    BAO_DEV_LISTEN_ADDRESS: "0.0.0.0:8200"
injector:
  enabled: false
ui:
  enabled: true
`, localOpenBaoExternalAddress(state), state.OpenBaoHost, state.OpenBaoHost)
	return writeLocalFile(state.OpenBaoValuesPath, content)
}

func writeLocalClusterSecretStore(opts *Options, state *LocalState) error {
	content := fmt.Sprintf(`apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: %s-%s
spec:
  provider:
    vault:
      server: %s
      path: kv
      version: v2
      auth:
        kubernetes:
          mountPath: kubernetes
          role: %s
          serviceAccountRef:
            name: external-secrets
            namespace: external-secrets
`, opts.ClusterConfig.Name, opts.ClusterConfig.Stage, localOpenBaoInternalAddress, localExternalSecretsRole)
	return writeLocalFile(state.ClusterSecretStorePath, content)
}

func writeLocalArgocdValues(opts *Options, state *LocalState, credentials localOIDCCredentials) error {
	content := fmt.Sprintf(`bootstrapValues:
  projects:
    %s-%s:
      sourceRepos:
%s
argo-cd:
  repoServer:
    volumeMounts:
      - name: host-ca-certificates
        mountPath: %s
        readOnly: true
    volumes:
      - name: host-ca-certificates
        hostPath:
          path: %s
          type: File
  server:
    ingressGrpc:
      enabled: false
      annotations:
        cert-manager.io/cluster-issuer: ""
      tls: false
    ingress:
      enabled: true
      ingressClassName: traefik
      annotations:
        cert-manager.io/cluster-issuer: ""
      tls: false
  configs:
    cm:
      url: http://%s/argocd
      dex.config: |
        connectors:
          - type: oidc
            id: openbao
            name: OpenBao
            config:
              issuer: %s
              clientID: %s
              clientSecret: %s
              redirectURI: http://%s/argocd/api/dex/callback
              scopes:
                - openid
              insecureSkipEmailVerified: true
              userIDKey: sub
              userNameKey: sub
    rbac:
      policy.default: role:admin
      policy.csv: ""
`, opts.ClusterConfig.Name, opts.ClusterConfig.Stage, formatYAMLList(localProjectSourceRepos(opts)),
		localKindHostCACertPath, localKindHostCACertPath,
		state.BaseHost, localOpenBaoOIDCIssuerURL(state), credentials.ClientID, credentials.ClientSecret, state.BaseHost)
	return writeLocalFile(state.ArgocdValuesPath, content)
}

func writeLocalOAuth2ProxyValues(state *LocalState) error {
	content := fmt.Sprintf(`oauth2-proxy:
  config:
    existingSecret: oauth2-credentials
    configFile: |-
      reverse_proxy = true
      redirect_url = "http://%s/oauth2/callback"
      email_domains = [ "*" ]
      cookie_secure = false
      upstreams = [ "static://200" ]
      provider = "oidc"
      oidc_issuer_url = "%s"
      scope = "openid"
      insecure_oidc_allow_unverified_email = true
  ingress:
    annotations:
      cert-manager.io/cluster-issuer: ""
    tls: []
`, state.BaseHost, localOpenBaoOIDCIssuerURL(state))
	return writeLocalFile(state.OAuth2ProxyValuesPath, content)
}

func localOpenBaoExternalAddress(state *LocalState) string {
	return fmt.Sprintf("http://%s", state.OpenBaoHost)
}

func localOpenBaoOIDCIssuerURL(state *LocalState) string {
	return fmt.Sprintf("%s/v1/identity/oidc/provider/default", localOpenBaoExternalAddress(state))
}

func localProjectSourceRepos(opts *Options) []string {
	repos := []string{
		opts.ClusterConfig.ArgoCD.Repo.HTTPS.Managed.URL,
		opts.ClusterConfig.ArgoCD.Repo.HTTPS.Customer.URL,
		"https://charts.external-secrets.io/",
		"https://charts.jetstack.io",
		"https://oauth2-proxy.github.io/manifests",
		"https://prometheus-community.github.io/helm-charts",
		"ghcr.io/traefik/helm",
		"oci://ghcr.io/traefik/helm",
	}
	if opts.ClusterConfig.ArgoCD.HelmRepo != nil && strings.TrimSpace(opts.ClusterConfig.ArgoCD.HelmRepo.URL) != "" {
		repos = append(repos, opts.ClusterConfig.ArgoCD.HelmRepo.URL)
	}

	seen := make(map[string]struct{}, len(repos))
	result := make([]string, 0, len(repos))
	for _, repo := range repos {
		trimmed := strings.TrimSpace(repo)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func formatYAMLList(values []string) string {
	var b strings.Builder
	for _, value := range values {
		b.WriteString("        - ")
		b.WriteString(strconv.Quote(value))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeLocalFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create directory for %q: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write file %q: %w", path, err)
	}
	return nil
}

func runCommand(ctx context.Context, name string, env map[string]string, input string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", key, value))
		}
	}
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s failed: %w\nstderr: %s", name, strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
