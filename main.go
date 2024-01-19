package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/log/klogv2"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
)

const (
	providerName = "mock"
	kubeVersion  = "v1.28.6" // in-sync with go.mod

	labelProvider = "virtual-kubelet.io/provider"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, os.Args); err != nil {
		fmt.Fprint(os.Stderr, "Command failed: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	logr := klogv2.New(log.Fields{"provider": providerName})
	ctx = log.WithLogger(ctx, logr)
	// NOTE(antoineco): api.HandleRunningPods shadows the context-injected
	// logger with the global logger (defaults to nopLogger), so we must
	// override it as well.
	log.L = logr
	defer klog.Flush()

	opts := readOpts(args)

	n, err := nodeutil.NewNode(opts.nodeName, newProviderFunc,
		withVersion(kubeVersion+"+"+providerName+".0"),
		withProviderLabel(providerName),
		withProviderTaint(providerName),
		withInitCapacity,
		withKubeconfig(ctx, opts.kubeconfig),
	)
	if err != nil {
		return fmt.Errorf("instantiating node controllers wrapper: %w", err)
	}

	go func() {
		<-ctx.Done()
		log.G(ctx).Info("Termination signal received. Stopping node controllers")
	}()

	if err = n.Run(ctx); err != nil {
		return fmt.Errorf("during runtime of node controllers: %w", err)
	}
	return nil
}

type cliOpts struct {
	kubeconfig string
	nodeName   string
}

func readOpts(args []string) cliOpts {
	var opts cliOpts

	fs := flag.NewFlagSet(filepath.Base(args[0]), flag.ExitOnError)

	fs.StringVar(&opts.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"),
		"Path to kubeconfig file. Falls back to in-cluster config")
	fs.StringVar(&opts.nodeName, "node_name", "mocklet",
		"Node name to register against Kubernetes")

	kfs := flag.NewFlagSet("klog", flag.PanicOnError)
	klog.InitFlags(kfs)
	kfs.VisitAll(func(f *flag.Flag) {
		fs.Var(f.Value, "klog."+f.Name, f.Usage)
	})

	_ = fs.Parse(args[1:]) // FlagSet has ExitOnError handling enabled

	return opts
}

// newProviderFunc satisfies the nodeutil.NewProviderFunc prototype.
func newProviderFunc(nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})

	p := &mockProvider{
		podStore:  idx,
		podLister: corev1listers.NewPodLister(idx),
	}

	return p, nil, nil
}

// withVersion sets the kubelet version.
func withVersion(v string) nodeutil.NodeOpt {
	return func(cfg *nodeutil.NodeConfig) error {
		cfg.NodeSpec.Status.NodeInfo.KubeletVersion = v
		return nil
	}
}

// withProviderLabel sets the Virtual Kubelet provider label.
func withProviderLabel(val string) nodeutil.NodeOpt {
	return func(cfg *nodeutil.NodeConfig) error {
		cfg.NodeSpec.Labels[labelProvider] = val
		return nil
	}
}

// withProviderLabel sets the Virtual Kubelet provider taint.
func withProviderTaint(val string) nodeutil.NodeOpt {
	return func(cfg *nodeutil.NodeConfig) error {
		cfg.NodeSpec.Spec.Taints = append(cfg.NodeSpec.Spec.Taints, corev1.Taint{
			Key:    labelProvider,
			Value:  val,
			Effect: corev1.TaintEffectNoSchedule,
		})
		return nil
	}
}

// withProviderLabel sets the node resources to their maximum values.
func withInitCapacity(cfg *nodeutil.NodeConfig) error {
	maxPodsQty := resource.MustParse(strconv.Itoa(math.MaxInt64))

	// the scheduler returns "Insufficient cpu" above this value, because CPU
	// requests are evaluated in milliCPU
	maxCPUQty := resource.MustParse(strconv.Itoa(math.MaxInt64 / 1000))

	// must stay below maxInt due to internal conversions
	const maxGibibytes = 8_589_934_592 // maxInt bytes
	maxBinQty := resource.MustParse(strconv.Itoa(maxGibibytes-1) + "Gi")

	cfg.NodeSpec.Status.Capacity = corev1.ResourceList{
		corev1.ResourcePods:             maxPodsQty,
		corev1.ResourceCPU:              maxCPUQty,
		corev1.ResourceMemory:           maxBinQty,
		corev1.ResourceEphemeralStorage: maxBinQty,
	}

	return nil
}

// withKubeconfig sets up a Kubernetes clientSet from the provided kubeconfig.
func withKubeconfig(ctx context.Context, kubeconfig string) nodeutil.NodeOpt {
	// NOTE(antoineco): nodeutil.NewNode attempts to create a clientSet before
	// executing the functional options passed by the caller, so setting
	// cfg.KubeconfigPath has no effect. This is mitigated here by creating a
	// clientSet explicitly.
	c, err := nodeutil.ClientsetFromEnv(kubeconfig)
	if err != nil {
		if kubeconfig == "" {
			log.G(ctx).Warn("The creation of the clientSet failed while the value of kubeconfig was ",
				"empty. Consider passing an explicit value.")
		}
		err = fmt.Errorf("creating Kubernetes clientSet: %w", err)
	}

	// NOTE(antoineco): nodeutil.NewNode calls a helper function (defaultClientFromEnv)
	// which returns a nil interface in case of failure. As a result,
	// nodeutil.NewNode suffers from a comparison to nil-interface bug which
	// later causes a panic. This is mitigated here by overriding the value of
	// cfg.Client unconditionally, potentially with a true nil value if the
	// clientSet creation failed above.
	withClient := nodeutil.WithClient(c)

	return func(cfg *nodeutil.NodeConfig) error {
		return errors.Join(err, withClient(cfg))
	}
}
