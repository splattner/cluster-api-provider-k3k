// Package helm contains a simple Helm client that implements a few actions found in the main Helm CLI.
package helm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// K3kNamespace is the default namespace for k3k resources
	K3kNamespace = "k3k-system"
	// K3kRepoURL is the URL of the default k3k Helm repository
	K3kRepoURL = "https://rancher.github.io/k3k"
	// K3kRepoName is the name of the default k3k Helm repository
	K3kRepoName = "k3k"
	// K3kChartRef
	K3kChartRef = "k3k/k3k"
	// K3kChartVersion is the k3k chart version installed on the host cluster. It must match the
	// github.com/rancher/k3k version in go.mod, so the controller understands the Cluster spec we create.
	K3kChartVersion = "1.2.0"
	// K3kReleaseName
	K3kReleaseName = "k3k"
)

var ErrReleaseMismatch = errors.New("chart release is not matching the current build")

type Client struct {
	restClientGetter genericclioptions.RESTClientGetter
	settings         *cli.EnvSettings

	namespace   string
	chartRef    string
	releaseName string
}

// New creates a Client using a RESTClientGetter that will be used for Helm calls to the Kubernetes API.
// The chartRef must be a repository reference in the format "repo/chart" (e.g., "k3k/k3k", "bitnami/nginx").
// Automatically adds the k3k repository (https://rancher.github.io/k3k) if not already present.
func New(ctx context.Context, clientGetter genericclioptions.RESTClientGetter) (*Client, error) {
	settings := cli.New()

	if err := os.MkdirAll(settings.RepositoryCache, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	repoFile := repo.NewFile()
	if _, err := os.Stat(settings.RepositoryConfig); os.IsNotExist(err) {
		if err := repoFile.WriteFile(settings.RepositoryConfig, 0o644); err != nil {
			return nil, fmt.Errorf("failed to initialize repo file: %w", err)
		}
	}

	return &Client{
		restClientGetter: clientGetter,
		settings:         settings,
		chartRef:         K3kChartRef,
		namespace:        K3kNamespace,
		releaseName:      K3kReleaseName,
	}, nil
}

// AddRepository adds a Helm repository by name and URL.
func (c *Client) AddRepository(ctx context.Context, repoName, repoURL string) error {
	logger := helmLogger(ctrl.LoggerFrom(ctx))

	repoFile, err := repo.LoadFile(c.settings.RepositoryConfig)
	if err != nil {
		return fmt.Errorf("failed to load repository file: %w", err)
	}

	if repoFile.Has(repoName) {
		logger("repository %s already exists", repoName)
		return nil
	}

	entry := &repo.Entry{
		Name: repoName,
		URL:  repoURL,
	}

	chartRepo, err := repo.NewChartRepository(entry, getter.All(c.settings))
	if err != nil {
		return fmt.Errorf("failed to create chart repository: %w", err)
	}

	chartRepo.CachePath = c.settings.RepositoryCache

	if _, err := chartRepo.DownloadIndexFile(); err != nil {
		return fmt.Errorf("failed to download repository index for %s: %w", repoName, err)
	}

	repoFile.Update(entry)

	if err := repoFile.WriteFile(c.settings.RepositoryConfig, 0o644); err != nil {
		return fmt.Errorf("failed to write repository file: %w", err)
	}

	logger("successfully added repository %s", repoName)

	return nil
}

// WithRESTClientGetter returns a copy of the Client running the Helm actions on the cluster of the RESTClientGetter.
func (c *Client) WithRESTClientGetter(clientGetter genericclioptions.RESTClientGetter) *Client {
	clientCopy := *c
	clientCopy.restClientGetter = clientGetter

	return &clientCopy
}

// GetRelease returns the current Helm release, or nil if it doesn't exist.
func (c *Client) GetRelease(ctx context.Context) (*release.Release, error) {
	cfg, err := c.actionConfig(ctx)
	if err != nil {
		return nil, err
	}

	rel, err := action.NewGet(cfg).Run(c.releaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}

	return rel, err
}

// InstallChart installs a Helm release from a repository chart.
func (c *Client) InstallChart(ctx context.Context, values map[string]any, labels map[string]string) error {
	cfg, err := c.actionConfig(ctx)
	if err != nil {
		return err
	}

	install := action.NewInstall(cfg)
	install.Namespace = c.namespace
	install.CreateNamespace = true
	install.ReleaseName = c.releaseName
	install.Version = K3kChartVersion
	install.Labels = labels
	install.Wait = true
	install.Timeout = time.Minute

	chart, err := c.loadChart(&install.ChartPathOptions)
	if err != nil {
		return err
	}

	_, err = install.RunWithContext(ctx, chart, values)

	return err
}

// UpgradeChart upgrades the Helm release to the chart version, reusing the values of the release.
// The upgrade is rolled back if it fails.
func (c *Client) UpgradeChart(ctx context.Context, labels map[string]string) error {
	cfg, err := c.actionConfig(ctx)
	if err != nil {
		return err
	}

	upgrade := action.NewUpgrade(cfg)
	upgrade.Namespace = c.namespace
	upgrade.Version = K3kChartVersion
	upgrade.Labels = labels
	upgrade.ReuseValues = true
	upgrade.Atomic = true
	upgrade.Timeout = 2 * time.Minute

	chart, err := c.loadChart(&upgrade.ChartPathOptions)
	if err != nil {
		return err
	}

	_, err = upgrade.RunWithContext(ctx, c.releaseName, chart, nil)

	return err
}

func (c *Client) actionConfig(ctx context.Context) (*action.Configuration, error) {
	var cfg action.Configuration
	if err := cfg.Init(c.restClientGetter, c.namespace, "", helmLogger(ctrl.LoggerFrom(ctx))); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Client) loadChart(options *action.ChartPathOptions) (*chart.Chart, error) {
	chartPath, err := options.LocateChart(c.chartRef, c.settings)
	if err != nil {
		return nil, fmt.Errorf("failed to locate chart %s: %w", c.chartRef, err)
	}

	return loader.Load(chartPath)
}

// helmLogger wraps a logger that will write messages using an expected pattern of a fmt string followed by arguments.
func helmLogger(logger logr.Logger) action.DebugLog {
	return func(format string, v ...any) {
		logger.Info(fmt.Sprintf(format, v...))
	}
}
