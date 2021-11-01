package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/michaelwasher/kube-strace/pkg/kstrace"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
)

// Optional CLI flags
type KubeStraceCommandArgs struct {
	traceImage      *string
	traceTimeoutStr *string
	socketPath      *string
	logLevelStr     *string
	outputDirectory *string
}
type KubeStraceCommand struct {
	KubeStraceCommandArgs

	// Converted flags
	logLevel     log.Level
	traceTimeout time.Duration

	// Command state
	tracers    []*kstrace.KStracer
	targetPods []corev1.Pod

	// GenericCLI Options
	clientset       *kubernetes.Clientset
	builder         *resource.Builder
	restConfig      *rest.Config
	kubeConfigFlags *genericclioptions.ConfigFlags
}

func stringptr(val string) *string {
	return &val
}

func NewKubeStraceDefaults() KubeStraceCommandArgs {
	return KubeStraceCommandArgs{
		traceImage:      stringptr("quay.io/mwasher/crictl:0.0.2"),
		socketPath:      stringptr("/run/crio/crio.sock"),
		logLevelStr:     stringptr("info"),
		traceTimeoutStr: stringptr("0"),
		outputDirectory: stringptr("strace-collection"),
	}
}

func NewKubeStraceCommand(applicationName string) *cobra.Command {
	kCmd := &KubeStraceCommand{KubeStraceCommandArgs: NewKubeStraceDefaults()}

	cmd := &cobra.Command{
		Use:   applicationName,
		Short: "Run strace against Pods and Deployments in Kubernetes",
		Long:  fmt.Sprintf(`%q is a CLI tool that provides the ability to easily perform debugging of system-calls and process state for applications running on the Kubernetes platform.`, applicationName),
		RunE: func(cmd *cobra.Command, args []string) error {

			if err := kCmd.Complete(cmd, args); err != nil {
				return err
			}
			if err := kCmd.Validate(); err != nil {
				return err
			}
			if err := kCmd.Run(); err != nil {
				return err
			}

			return nil
		},
	}
	// Add Kubectl / Kubernetes CLI flags
	flags := cmd.PersistentFlags()

	stringptr := func(val string) *string {
		return &val
	}

	kCmd.kubeConfigFlags = &genericclioptions.ConfigFlags{
		Namespace: stringptr(""),
		Timeout:   stringptr("30s"),
	}

	kCmd.kubeConfigFlags.AddFlags(flags)

	// Add command-specific flags
	flags.StringVar(kCmd.socketPath, "socket-path", *kCmd.socketPath, "The location of the CRI socket on the host machine.")
	flags.StringVar(kCmd.traceImage, "image", *kCmd.traceImage, "The trace image for use when performing the strace.")
	flags.StringVar(kCmd.traceTimeoutStr, "trace-timeout", *kCmd.traceTimeoutStr, "The length of time to capture the strace output for.")
	flags.StringVarP(kCmd.outputDirectory, "output", "o", *kCmd.outputDirectory, "The directory to store the strace data.")

	// LogLevels
	logLevels := func() []string {
		levels := []string{}

		for _, level := range log.AllLevels {
			levelStr, err := level.MarshalText()
			if err != nil {
				continue
			}

			levels = append(levels, string(levelStr))
		}
		return levels
	}()
	flags.StringVar(kCmd.logLevelStr, "log-level", *kCmd.logLevelStr, fmt.Sprintf("The verbosity level of the output from the command. Available options are [%s].", strings.Join(logLevels, ", ")))

	return cmd
}

func (kCmd *KubeStraceCommand) configureClientset() error {
	// Setup REST APi conf
	var err error

	kCmd.restConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kCmd.kubeConfigFlags.ToRawKubeConfigLoader().ConfigAccess().GetDefaultFilename()},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return err
	}

	kCmd.clientset, err = kubernetes.NewForConfig(kCmd.restConfig)
	if err != nil {
		return err
	}

	return nil
}
func (kCmd *KubeStraceCommand) Complete(cmd *cobra.Command, args []string) error {
	var err error

	// Configure the loglevel
	log.Info(*kCmd.logLevelStr)
	kCmd.logLevel, err = log.ParseLevel(*kCmd.logLevelStr)
	if err != nil {
		return err
	}

	log.SetLevel(kCmd.logLevel)
	log.Infof("Running with loglevel: %v", kCmd.logLevel)

	// Configure ClientSet and API communication
	err = kCmd.configureClientset()
	if err != nil {
		return err
	}

	matchVersionKubeConfigFlags := cmdutil.NewMatchVersionFlags(kCmd.kubeConfigFlags)
	f := cmdutil.NewFactory(matchVersionKubeConfigFlags)

	namespace, _, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	kCmd.builder = f.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		ResourceNames("pod", args...).NamespaceParam(namespace).DefaultNamespace()

	return nil
}

func (kCmd *KubeStraceCommand) Validate() error {
	var err error

	// Collect target pods
	kCmd.targetPods, err = processResources(kCmd.builder, kCmd.clientset)
	if err != nil {
		return err
	}

	// Check flags are valid
	if len(kCmd.targetPods) < 1 {
		return fmt.Errorf("a target pod must be defined")
	}
	if len(kCmd.targetPods) > 1 && *kCmd.outputDirectory == "-" {
		return fmt.Errorf("cannot have multiple target pods but output to standard out")
	}
	if len(kCmd.targetPods[0].Spec.Containers) > 1 && *kCmd.outputDirectory == "-" {
		return fmt.Errorf("there are multiple containers defined for pod %q. unable to output to standard out for pods with multiple containers", kCmd.targetPods[0].Name)
	}

	kCmd.traceTimeout, err = time.ParseDuration(*kCmd.traceTimeoutStr)
	if err != nil {
		return err
	}

	return nil
}

func (kCmd *KubeStraceCommand) Run() error {
	var err error
	ctx := context.TODO()

	// Create namespace for Strace Pods
	ns, err := kstrace.CreateNamespace(ctx, kCmd.clientset)
	defer kstrace.CleanupNamespace(ctx, kCmd.clientset, ns.Name)

	if err != nil {
		return err
	}

	// Create Tracers for each Pod
	for _, targetPod := range kCmd.targetPods {
		tracer := kstrace.NewKStracer(kCmd.clientset, kCmd.restConfig, *kCmd.traceImage, &targetPod, ns.Name, *kCmd.socketPath, kCmd.traceTimeout, *kCmd.outputDirectory)
		kCmd.tracers = append(kCmd.tracers, tracer)
	}

	for _, tracer := range kCmd.tracers {
		// TODO Place in goroutine
		err = tracer.Start()

		// Configure Cleanup
		defer tracer.Cleanup()
		defer tracer.Stop()

		if err != nil {
			return err
		}
	}

	return nil
}

func processResources(builder *resource.Builder, clientset *kubernetes.Clientset) ([]corev1.Pod, error) {
	r := builder.Do()
	podSlice := []corev1.Pod{}
	err := r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			// TODO(verb): configurable early return
			return err
		}
		var visitErr error

		switch obj := info.Object.(type) {

		case *corev1.Pod:
			log.Debugf("Adding pod to strace list %v", obj)
			podSlice = append(podSlice, *obj)

		default:
			visitErr = fmt.Errorf("%q not supported by kstrace", info.Mapping.GroupVersionKind)
		}
		if visitErr != nil {
			return visitErr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	/// Build the list of Nodes and Pods to select from; With
	log.Debugf("Pod List: '%v'", podSlice)

	return podSlice, nil
}
