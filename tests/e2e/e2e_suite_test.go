package e2e_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/stdr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	clusterv1beta2 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	configPath         string
	artifactFolder     string
	useExistingCluster bool
	kindClusterName    string

	e2eConfig            *clusterctl.E2EConfig
	clusterctlConfigPath string

	bootstrapClusterProvider bootstrap.ClusterProvider
	bootstrapClusterProxy    framework.ClusterProxy
)

func init() {
	flag.StringVar(&configPath, "e2e.config", "clusterctl.yaml", "path to the e2e config file")
	flag.StringVar(&artifactFolder, "e2e.artifacts-folder", "_artifacts", "folder where e2e test artifact should be stored")
	flag.BoolVar(&useExistingCluster, "e2e.use-existing-cluster", false, "if true, the test uses the current cluster instead of creating a new one (default discovery rules apply)")
	flag.StringVar(&kindClusterName, "e2e.kind-cluster-name", "", "name of the Kind cluster used to load the images")
}

func TestE2e(t *testing.T) {
	ctrl.SetLogger(stdr.New(log.New(os.Stdout, "", log.LstdFlags)))
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2e Suite")
}

// Using a SynchronizedBeforeSuite for controlling how to create resources shared across ParallelNodes (~ginkgo threads).
// The local clusterctl repository & the bootstrap cluster are created once and shared across all the tests.
var _ = SynchronizedBeforeSuite(beforeAllParallelNodes, beforeEachParallelNode)

// After all ParallelNodes.
func beforeAllParallelNodes() []byte {
	var err error
	ctx := context.Background()

	Expect(configPath).To(BeAnExistingFile(), "Invalid test suite argument. e2e.config should be an existing file.")
	artifactFolder, err = filepath.Abs(artifactFolder)
	Expect(err).NotTo(HaveOccurred(), "Invalid test suite argument. e2e.artifacts-folder should be a valid path.")

	By("Loading the e2e test configuration from " + configPath)
	By("Storing artifacts in " + artifactFolder)

	clusterctlConfigPath := createClusterctlLocalRepository(ctx, configPath, artifactFolder)

	if kindClusterName != "" {
		e2eConfig.ManagementClusterName = kindClusterName
	}

	By("Setting up the bootstrap cluster")
	bootstrapClusterProvider, bootstrapClusterProxy = setupBootstrapCluster(ctx, e2eConfig, useExistingCluster)

	By("Initializing the bootstrap cluster")
	initBootstrapCluster(ctx, bootstrapClusterProxy, e2eConfig, clusterctlConfigPath)

	By("Installing the k3s bootstrap provider")
	installK3sBootstrapProvider(ctx, bootstrapClusterProxy, e2eConfig.MustGetVariable("K3S_BOOTSTRAP_COMPONENTS"))

	// encode the e2e config into the byte array.
	var configBuf bytes.Buffer
	enc := gob.NewEncoder(&configBuf)
	Expect(enc.Encode(e2eConfig)).To(Succeed())
	configStr := base64.StdEncoding.EncodeToString(configBuf.Bytes())

	return []byte(
		strings.Join([]string{
			artifactFolder,
			clusterctlConfigPath,
			configStr,
			bootstrapClusterProxy.GetKubeconfigPath(),
		}, ","),
	)
}

// Before each ParallelNode.
func beforeEachParallelNode(data []byte) {
	parts := strings.Split(string(data), ",")
	Expect(parts).To(HaveLen(4))

	artifactFolder = parts[0]
	clusterctlConfigPath = parts[1]

	// Decode the e2e config
	configBytes, err := base64.StdEncoding.DecodeString(parts[2])
	Expect(err).NotTo(HaveOccurred())
	buf := bytes.NewBuffer(configBytes)
	dec := gob.NewDecoder(buf)
	Expect(dec.Decode(&e2eConfig)).To(Succeed())

	kubeconfigPath := parts[3]
	By(kubeconfigPath)
}

func createClusterctlLocalRepository(ctx context.Context, configPath string, artifactFolder string) string {
	e2eConfig = clusterctl.LoadE2EConfig(ctx, clusterctl.LoadE2EConfigInput{ConfigPath: configPath})
	Expect(e2eConfig).NotTo(BeNil(), "Failed to load E2E config from %s", configPath)

	// set default intervals
	e2eConfig.Intervals = map[string][]string{}
	e2eConfig.Intervals["default/wait-cluster"] = []string{"3m"}
	e2eConfig.Intervals["default/wait-worker-nodes"] = []string{"10m", "10s"}

	repositoryFolder := filepath.Join(artifactFolder, "repository")

	createRepositoryInput := clusterctl.CreateRepositoryInput{
		E2EConfig:        e2eConfig,
		RepositoryFolder: repositoryFolder,
	}

	clusterctlConfigPath = clusterctl.CreateRepository(ctx, createRepositoryInput)
	Expect(clusterctlConfigPath).To(BeAnExistingFile(), "The clusterctl config file does not exists in the local repository: "+repositoryFolder)

	return clusterctlConfigPath
}

// Using a SynchronizedAfterSuite for controlling how to delete resources shared across ParallelNodes (~ginkgo threads).
// The bootstrap cluster is shared across all the tests, so it should be deleted only after all ParallelNodes completes.
// The local clusterctl repository is preserved like everything else created into the artifact folder.
var _ = SynchronizedAfterSuite(afterEachParallelNode, afterAllParallelNodes)

// After each ParallelNode.
func afterEachParallelNode() {}

// After all ParallelNodes.
func afterAllParallelNodes() {
	By("Tearing down the management cluster")
	tearDown(context.Background(), bootstrapClusterProvider, bootstrapClusterProxy)
}

func setupBootstrapCluster(ctx context.Context, config *clusterctl.E2EConfig, useExistingCluster bool) (bootstrap.ClusterProvider, framework.ClusterProxy) {
	var clusterProvider bootstrap.ClusterProvider
	var kubeconfigPath string

	if useExistingCluster {
		By("Reusing existing Kind cluster: " + config.ManagementClusterName)

		kubeconfigPath = clientcmd.NewDefaultClientConfigLoadingRules().GetDefaultFilename()

		// Loading image for already created cluster
		imagesInput := bootstrap.LoadImagesToKindClusterInput{
			Name:   config.ManagementClusterName,
			Images: config.Images,
		}
		err := bootstrap.LoadImagesToKindCluster(ctx, imagesInput)
		Expect(err).NotTo(HaveOccurred(), "Failed to load images to the bootstrap cluster: %s", err)
	} else {
		By("Creating a new Kind cluster")

		clusterProvider = bootstrap.CreateKindBootstrapClusterAndLoadImages(ctx, bootstrap.CreateKindBootstrapClusterAndLoadImagesInput{
			Name:               config.ManagementClusterName,
			RequiresDockerSock: config.HasDockerProvider(),
			Images:             config.Images,
			// K3k sets a blockOwnerDeletion owner reference to its Cluster on the server PVC template,
			// which the StatefulSet controller is not allowed to set when this admission plugin is enabled.
			// See https://github.com/rancher/k3k/issues/1340
			DisableOwnerReferencesPermissionEnforcement: true,
		})
		Expect(clusterProvider).NotTo(BeNil(), "Failed to create a bootstrap cluster")

		kubeconfigPath = clusterProvider.GetKubeconfigPath()
		Expect(kubeconfigPath).To(BeAnExistingFile(), "Failed to get the kubeconfig file for the bootstrap cluster")
	}

	scheme := runtime.NewScheme()
	framework.TryAddDefaultSchemes(scheme)
	clusterProxy := framework.NewClusterProxy("bootstrap", kubeconfigPath, scheme)
	Expect(clusterProxy).NotTo(BeNil(), "Failed to get a bootstrap cluster proxy")

	return clusterProvider, clusterProxy
}

func initBootstrapCluster(ctx context.Context, bootstrapClusterProxy framework.ClusterProxy, config *clusterctl.E2EConfig, clusterctlConfig string) {
	logFolder := filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName())

	clusterctl.InitManagementClusterAndWatchControllerLogs(ctx, clusterctl.InitManagementClusterAndWatchControllerLogsInput{
		ClusterProxy:            bootstrapClusterProxy,
		ClusterctlConfigPath:    clusterctlConfig,
		InfrastructureProviders: config.InfrastructureProviders(),
		AddonProviders:          config.AddonProviders(),
		LogFolder:               logFolder,
	}, config.GetIntervals(bootstrapClusterProxy.GetName(), "wait-controllers")...)
}

// installK3sBootstrapProvider installs the k3s bootstrap provider (cluster-api-k3s) from its components URL.
func installK3sBootstrapProvider(ctx context.Context, clusterProxy framework.ClusterProxy, componentsURL string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, componentsURL, nil)
	Expect(err).ToNot(HaveOccurred())

	resp, err := http.DefaultClient.Do(req)
	Expect(err).ToNot(HaveOccurred(), "Failed to download the k3s bootstrap provider components")

	defer func() { _ = resp.Body.Close() }()

	Expect(resp.StatusCode).To(Equal(http.StatusOK), "Failed to download the k3s bootstrap provider components")

	components, err := io.ReadAll(resp.Body)
	Expect(err).ToNot(HaveOccurred())

	// the webhook certificate is issued by cert-manager, so retry until the webhooks are ready to serve
	Eventually(func() error {
		return clusterProxy.CreateOrUpdate(ctx, components)
	}, "3m", "10s").Should(Succeed())

	framework.WaitForDeploymentsAvailable(ctx, framework.WaitForDeploymentsAvailableInput{
		Getter: clusterProxy.GetClient(),
		Deployment: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:      "capi-k3s-bootstrap-controller-manager",
			Namespace: "capi-k3s-bootstrap-system",
		}},
	}, "3m", "10s")
}

func tearDown(ctx context.Context, bootstrapClusterProvider bootstrap.ClusterProvider, bootstrapClusterProxy framework.ClusterProxy) {
	if bootstrapClusterProxy != nil {
		bootstrapClusterProxy.Dispose(ctx)
	}
	if bootstrapClusterProvider != nil {
		bootstrapClusterProvider.Dispose(ctx)
	}
}

func noOpWaiter(ctx context.Context, input clusterctl.ApplyCustomClusterTemplateAndWaitInput, result *clusterctl.ApplyCustomClusterTemplateAndWaitResult) {
}

var _ = When("creating a Cluster from the cluster-template", func() {
	It("works", func() {
		specName := "cluster-template"

		ctx := context.Background()
		logFolder := filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName())

		By("Creating a namespace for the test")

		namespace, cancelWatches := framework.CreateNamespaceAndWatchEvents(ctx, framework.CreateNamespaceAndWatchEventsInput{
			Creator:   bootstrapClusterProxy.GetClient(),
			ClientSet: bootstrapClusterProxy.GetClientSet(),
			Name:      specName,
			LogFolder: logFolder,
		})

		defer cancelWatches()
		defer framework.DeleteNamespace(ctx, framework.DeleteNamespaceInput{
			Deleter: bootstrapClusterProxy.GetClient(),
			Name:    namespace.Name,
		})

		input := clusterctl.ApplyClusterTemplateAndWaitInput{
			ClusterProxy: bootstrapClusterProxy,
			ConfigCluster: clusterctl.ConfigClusterInput{
				Namespace:                namespace.Name,
				ClusterName:              "cluster-" + utilrand.String(5),
				ClusterctlConfigPath:     clusterctlConfigPath,
				KubeconfigPath:           bootstrapClusterProxy.GetKubeconfigPath(),
				InfrastructureProvider:   e2eConfig.GetVariableOrEmpty("INFRASTRUCTURE_PROVIDER"),
				Flavor:                   e2eConfig.GetVariableOrEmpty("CONTROL_PLANE_FLAVOR"),
				KubernetesVersion:        e2eConfig.GetVariableOrEmpty("KUBERNETES_VERSION"),
				ControlPlaneMachineCount: ptr.To[int64](1),
				WorkerMachineCount:       ptr.To[int64](1),
				LogFolder:                logFolder,
			},
			WaitForClusterIntervals: e2eConfig.GetIntervals(specName, "wait-cluster"),
			ControlPlaneWaiters: clusterctl.ControlPlaneWaiters{
				WaitForControlPlaneInitialized:   noOpWaiter,
				WaitForControlPlaneMachinesReady: noOpWaiter,
			},
		}

		var result clusterctl.ApplyClusterTemplateAndWaitResult
		clusterctl.ApplyClusterTemplateAndWait(ctx, input, &result)

		Expect(result.Cluster).ToNot(BeNil(), "The returned Cluster object should not be nil")

		By("Waiting for the control plane to be initialized and available")

		Eventually(func(g Gomega) {
			cluster := &clusterv1beta2.Cluster{}
			g.Expect(bootstrapClusterProxy.GetClient().Get(ctx, client.ObjectKeyFromObject(result.Cluster), cluster)).To(Succeed())
			g.Expect(ptr.Deref(cluster.Status.Initialization.ControlPlaneInitialized, false)).To(BeTrue())
			g.Expect(conditions.IsTrue(cluster, clusterv1beta2.ClusterInfrastructureReadyCondition)).To(BeTrue())
			g.Expect(conditions.IsTrue(cluster, clusterv1beta2.ClusterControlPlaneAvailableCondition)).To(BeTrue())
		}, "5m", "10s").Should(Succeed())
	})
})

var _ = When("creating an HCP Cluster with worker Machines", func() {
	It("joins the worker nodes", func() {
		specName := "cluster-hcp"

		ctx := context.Background()
		logFolder := filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName())

		By("Creating a namespace for the test")

		namespace, cancelWatches := framework.CreateNamespaceAndWatchEvents(ctx, framework.CreateNamespaceAndWatchEventsInput{
			Creator:   bootstrapClusterProxy.GetClient(),
			ClientSet: bootstrapClusterProxy.GetClientSet(),
			Name:      specName,
			LogFolder: logFolder,
		})

		defer cancelWatches()
		defer framework.DeleteNamespace(ctx, framework.DeleteNamespaceInput{
			Deleter: bootstrapClusterProxy.GetClient(),
			Name:    namespace.Name,
		})

		// the K3k server is exposed with a NodePort on the management cluster node, reachable by the CAPD Machines
		endpointHost := managementClusterNodeIP(ctx)

		input := clusterctl.ApplyClusterTemplateAndWaitInput{
			ClusterProxy: bootstrapClusterProxy,
			ConfigCluster: clusterctl.ConfigClusterInput{
				Namespace:                namespace.Name,
				ClusterName:              "hcp-" + utilrand.String(5),
				ClusterctlConfigPath:     clusterctlConfigPath,
				KubeconfigPath:           bootstrapClusterProxy.GetKubeconfigPath(),
				InfrastructureProvider:   e2eConfig.GetVariableOrEmpty("INFRASTRUCTURE_PROVIDER"),
				Flavor:                   "hcp-docker",
				KubernetesVersion:        e2eConfig.GetVariableOrEmpty("KUBERNETES_VERSION"),
				ControlPlaneMachineCount: ptr.To[int64](1),
				WorkerMachineCount:       ptr.To[int64](1),
				ClusterctlVariables:      map[string]string{"HCP_ENDPOINT_HOST": endpointHost},
				LogFolder:                logFolder,
			},
			WaitForClusterIntervals:   e2eConfig.GetIntervals(specName, "wait-cluster"),
			WaitForMachineDeployments: e2eConfig.GetIntervals(specName, "wait-worker-nodes"),
			ControlPlaneWaiters: clusterctl.ControlPlaneWaiters{
				WaitForControlPlaneInitialized:   noOpWaiter,
				WaitForControlPlaneMachinesReady: noOpWaiter,
			},
		}

		// waits for the MachineDeployment Machines to get a Node: they joined the K3k cluster with the
		// token from the K3kControlPlane, and their providerID matches the Node one
		var result clusterctl.ApplyClusterTemplateAndWaitResult
		clusterctl.ApplyClusterTemplateAndWait(ctx, input, &result)

		Expect(result.Cluster).ToNot(BeNil(), "The returned Cluster object should not be nil")
		Expect(result.MachineDeployments).To(HaveLen(1))

		By("Waiting for the worker nodes to be ready")

		workloadClient := bootstrapClusterProxy.GetWorkloadCluster(ctx, namespace.Name, result.Cluster.Name).GetClient()

		Eventually(func(g Gomega) {
			nodes := &corev1.NodeList{}
			g.Expect(workloadClient.List(ctx, nodes)).To(Succeed())
			g.Expect(nodes.Items).To(HaveLen(1))

			for _, node := range nodes.Items {
				g.Expect(node.Spec.ProviderID).To(HavePrefix("docker://"))
				g.Expect(isNodeReady(node)).To(BeTrue(), "node %s is not ready", node.Name)
			}
		}, "5m", "10s").Should(Succeed())
	})
})

// managementClusterNodeIP returns the internal IP of the management cluster node.
func managementClusterNodeIP(ctx context.Context) string {
	nodes := &corev1.NodeList{}
	Expect(bootstrapClusterProxy.GetClient().List(ctx, nodes)).To(Succeed())
	Expect(nodes.Items).ToNot(BeEmpty())

	for _, address := range nodes.Items[0].Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			return address.Address
		}
	}

	Fail("management cluster node has no internal IP")

	return ""
}

func isNodeReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
