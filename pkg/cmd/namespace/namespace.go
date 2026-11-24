package namespace

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/input"
	"github.com/jenkins-x/jx-helpers/v3/pkg/input/survey"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/jxclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/jxenv"
	"github.com/jenkins-x/jx-helpers/v3/pkg/options"
	"github.com/jenkins-x/jx-kube-client/v3/pkg/kubeclient"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/rest"

	"github.com/jenkins-x/jx-helpers/v3/pkg/kube"

	jxc "github.com/jenkins-x/jx-api/v4/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd/api"
)

// Options options for namespace
type Options struct {
	KubeClient kubernetes.Interface
	Input      input.Interface
	JXClient   jxc.Interface
	Args       []string
	Env        string
	PickEnv    bool
	Create     bool
	QuiteMode  bool
	BatchMode  bool

	// ConfigAccess is the kubeconfig accessor used to read and update the current context.
	// If nil the default client-go path options are used (the KUBECONFIG environment
	// variable or ~/.kube/config). Tests can inject an accessor backed by a temporary
	// file so the kubeconfig can be read and written without touching the process
	// environment or the user's real kubeconfig.
	ConfigAccess clientcmd.ConfigAccess
}

// ErrNamespaceSkipped is returned by Run when the target namespace does not exist and
// --quiet was given. Nothing has been changed and the command should exit successfully;
// callers decide how to translate that into a process exit code so that Run itself
// never terminates the process.
var ErrNamespaceSkipped = errors.New("namespace does not exist, skipping")

var (
	// configExtension is the key in the kube context's extensions which holds the
	// previously selected namespace. The key name and the JSON encoding of the value
	// (a JSON string) are part of the on-disk format: changing them would lose the
	// records written by older releases.
	configExtension = "previous-ns.jayex.io"

	cmdLong = templates.LongDesc(`
		Displays or changes the current namespace.

		The previously selected namespace is recorded per kube context in the context's
		"previous-ns.jayex.io" extension, so "jx ns -" returns to the namespace that was
		last used with the *current* kube context. Switching to another kube context and
		back therefore does not change where "jx ns -" goes.`)
	cmdExample = templates.Examples(`
		# view the current namespace
		jx --batch-mode ns

		# interactively select the namespace to switch to
		jx ns

		# change the current namespace to 'cheese'
		jx ns cheese

		# change the current namespace to 'brie' creating it if necessary
	    jx ns --create brie

		# switch to the namespace of the staging environment
		jx ns --env staging

		# switch back to the dev environment namespace
		jx ns --e dev

		# interactively select the Environment to switch to
		jx ns --pick

		# change to the previously selected namespace in the current kube context
		jx ns -`)

	info = termcolor.ColorInfo
)

// NewCmdNamespace returns the namespace cmd
func NewCmdNamespace() (*cobra.Command, *Options) {
	o := &Options{}
	cmd := &cobra.Command{
		Use:     "namespace",
		Aliases: []string{"ns"},
		Args:    cobra.MaximumNArgs(1),
		Short:   "View or change the current namespace context in the current Kubernetes cluster",
		Long:    cmdLong,
		Example: cmdExample,
		ValidArgsFunction: func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			client, _, err := kube.LazyCreateKubeClientAndNamespace(o.KubeClient, "")
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			names, err := getNamespaceNames(client)

			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			var contextNames []string
			for _, v := range names {
				if strings.HasPrefix(v, toComplete) {
					contextNames = append(contextNames, v)
				}
			}
			return contextNames, cobra.ShellCompDirectiveNoFileComp
		},
		Run: func(_ *cobra.Command, args []string) {
			o.Args = args
			err := o.Run()
			if errors.Is(err, ErrNamespaceSkipped) {
				// --quiet: the namespace does not exist. Report nothing else and exit
				// successfully, which is the behaviour callers relied on when this used
				// to be an os.Exit(0) inside the command.
				return
			}
			helper.CheckErr(err)
		},
	}

	cmd.Flags().BoolVarP(&o.Create, "create", "c", false, "Creates the specified namespace if it does not exist")
	cmd.Flags().BoolVarP(&o.BatchMode, "batch-mode", "b", false, "Enables batch mode")
	cmd.Flags().BoolVarP(&o.QuiteMode, "quiet", "q", false, "Do not fail if the namespace does not exist")
	cmd.Flags().BoolVarP(&o.PickEnv, "pick", "v", false, "Pick the Environment to switch to")
	cmd.Flags().StringVarP(&o.Env, "env", "e", "", "The Environment name to switch to the namepsace")
	return cmd, o
}

// Run implements the command
func (o *Options) Run() error {
	cfg, configAccess, err := o.loadConfig()
	if err != nil {
		return err
	}

	currentNS := ""
	if ctx := kube.CurrentContext(cfg); ctx != nil {
		currentNS = ctx.Namespace
	}

	client := o.KubeClient
	if o.ConfigAccess == nil {
		// Production path: let the helper create the client and, when the kubeconfig
		// context has no namespace, fall back to the in-pod namespace file (then
		// "default"), which is what the command has always done.
		var resolvedNS string
		client, resolvedNS, err = kube.LazyCreateKubeClientAndNamespace(client, currentNS)
		if err != nil {
			return fmt.Errorf("creating kubernetes client: %w", err)
		}
		currentNS = resolvedNS
	} else if client == nil {
		return fmt.Errorf("no Kubernetes client was provided")
	}
	o.KubeClient = client

	argNS := firstArg(o.Args)
	if o.Env != "" && argNS != "" {
		return fmt.Errorf("cannot use both --env and a namespace argument; remove either %q or --env", argNS)
	}

	ns := ""
	if o.Env != "" || o.PickEnv {
		ns, err = o.findNamespaceFromEnv(currentNS, o.Env)
		if err != nil {
			return fmt.Errorf("failed to find JayeX environment: %s: %w", o.Env, err)
		}
		if ns == "" {
			// nothing was picked; nothing to change
			return nil
		}
	} else if argNS == "-" {
		ns, err = o.previousNamespace(cfg, currentNS)
		if err != nil {
			return err
		}
		if ns == "" {
			// the recorded namespace no longer exists; previousNamespace already
			// explained that and the current namespace is kept as is
			o.logCurrentNamespace(cfg, currentNS)
			return nil
		}
	} else {
		ns = argNS
	}

	if ns == "" && !o.BatchMode {
		ns, err = pickNamespace(o, client, currentNS)
		if err != nil {
			return err
		}
	}

	if ns == "" || ns == currentNS {
		// Nothing to change. In particular do not rewrite the stored previous
		// namespace when the requested namespace is the current one.
		o.logCurrentNamespace(cfg, currentNS)
		return nil
	}

	ctx, err := changeNamespace(client, cfg, configAccess, ns, o.Create, o.QuiteMode)
	if err != nil {
		return err
	}
	if ctx == nil {
		log.Logger().Infof("No kube context - probably in a unit test or pod?\n")
	} else {
		log.Logger().Infof("Now using namespace '%s' on server '%s'.\n", info(ctx.Namespace), info(kube.Server(cfg, ctx)))
	}
	return nil
}

// loadConfig loads the kubeconfig using the injected accessor when there is one and
// the standard client-go locations otherwise.
func (o *Options) loadConfig() (*api.Config, clientcmd.ConfigAccess, error) {
	if o.ConfigAccess != nil {
		cfg, err := o.ConfigAccess.GetStartingConfig()
		if err != nil {
			return nil, o.ConfigAccess, fmt.Errorf("loading Kubernetes configuration from %s: %w", o.ConfigAccess.GetDefaultFilename(), err)
		}
		return cfg, o.ConfigAccess, nil
	}
	cfg, pathOptions, err := kubeclient.LoadConfig()
	if err != nil {
		return nil, pathOptions, fmt.Errorf("loading Kubernetes configuration: %w", err)
	}
	return cfg, pathOptions, nil
}

// previousNamespace returns the namespace recorded for the current kube context.
//
// It returns an error when there is no current context (for example when running
// inside a pod) or when no namespace has ever been selected for that context.
// When the recorded namespace no longer exists it logs an explanation and returns an
// empty namespace so the caller keeps the current namespace unchanged.
func (o *Options) previousNamespace(cfg *api.Config, currentNS string) (string, error) {
	ctx := kube.CurrentContext(cfg)
	if ctx == nil {
		return "", fmt.Errorf("cannot switch to the previous namespace: there is no current Kubernetes context (running inside a pod?)")
	}

	previous := ""
	if ext, ok := ctx.Extensions[configExtension].(*runtime.Unknown); ok {
		if err := json.Unmarshal(ext.Raw, &previous); err != nil {
			return "", fmt.Errorf("can't interpret the previous namespace recorded for context %s: %w", cfg.CurrentContext, err)
		}
	}
	if previous == "" {
		return "", fmt.Errorf("no previous namespace was recorded for context %s; select one first with 'jx ns <namespace>'", cfg.CurrentContext)
	}

	_, err := o.KubeClient.CoreV1().Namespaces().Get(context.TODO(), previous, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Logger().Warnf("the previously selected namespace %s no longer exists; keeping the current namespace %s", info(previous), info(currentNS))
			return "", nil
		}
		return "", fmt.Errorf("checking the previously selected namespace %s: %w", previous, err)
	}
	return previous, nil
}

// logCurrentNamespace reports the namespace which is currently in use.
func (o *Options) logCurrentNamespace(cfg *api.Config, ns string) {
	server := kube.Server(cfg, kube.CurrentContext(cfg))
	if kube.CurrentContext(cfg) == nil {
		log.Logger().Infof("Using namespace '%s' on server '%s'. No context - probably a unit test or pod?\n", info(ns), info(server))
	} else {
		log.Logger().Infof("Using namespace '%s' from context named '%s' on server '%s'.\n", info(ns), info(cfg.CurrentContext), info(server))
	}
}

func (o *Options) findNamespaceFromEnv(ns, name string) (string, error) {
	var err error
	if o.JXClient == nil || ns == "" {
		o.JXClient, ns, err = jxclient.LazyCreateJXClientAndNamespace(o.JXClient, ns)
		if err != nil {
			return "", fmt.Errorf("failed to create jx client: %w", err)
		}
	}

	names, err := o.GetEnvironmentNames(ns)
	if err != nil {
		return "", err
	}

	if len(names) == 0 {
		// lets find the dev namespace to use that to find environments
		devNS := ""
		devNS, _, err = jxenv.GetDevNamespace(o.KubeClient, ns)
		if err != nil {
			return "", fmt.Errorf("failed to find current dev namespace from %s: %w", ns, err)
		}
		if devNS != ns {
			log.Logger().Infof("using the team namespace %s to find Environments", info(devNS))
			ns = devNS
			names, err = o.GetEnvironmentNames(ns)
			if err != nil {
				return "", err
			}
		}
	}
	if name == "" {
		name, err = o.pickName(names, "", "Pick environment:", "pick the kubernetes namespace for the current kubernetes cluster")
		if err != nil {
			return "", fmt.Errorf("failed to pick environment: %w", err)
		}
		if name == "" {
			return "", nil
		}
	}

	env, err := o.JXClient.JenkinsV1().Environments(ns).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", options.InvalidArg(name, names)
		}
		return "", fmt.Errorf("failed to load Environment %s in namespace %s: %w", name, ns, err)
	}
	if env.Spec.Namespace == "" {
		return "", fmt.Errorf("Environment %s in namespace %s does not specify a namespace", name, ns)
	}
	return env.Spec.Namespace, nil
}

// GetEnvironmentNames returns the environment names in te given namespace
func (o *Options) GetEnvironmentNames(ns string) ([]string, error) {
	names, err := jxenv.GetEnvironmentNames(o.JXClient, ns)
	if err != nil && apierrors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return names, fmt.Errorf("failed to find Environment names in namespace %s: %w", ns, err)
	}
	return names, err
}

// firstArg returns the first non-empty positional argument.
func firstArg(args []string) string {
	for _, arg := range args {
		if arg != "" {
			return arg
		}
	}
	return ""
}

func changeNamespace(client kubernetes.Interface, config *api.Config, configAccess clientcmd.ConfigAccess, ns string, create, quietMode bool) (*api.Context, error) {
	if err := ensureNamespaceExists(client, ns, create, quietMode); err != nil {
		return nil, err
	}
	ctx := kube.CurrentContext(config)

	if ctx == nil {
		// There is no kube context (typically when running inside a pod). The switch
		// only happens in memory: there is no context to record the previous
		// namespace in and nothing is written to disk.
		ctx = api.NewContext()
		if config.Contexts == nil {
			config.Contexts = map[string]*api.Context{}
		}
		name := "pod"
		config.Contexts[name] = ctx
		config.CurrentContext = name
		ctx.Namespace = ns
		return ctx, nil
	}

	if ctx.Namespace == ns {
		return ctx, nil
	}

	if ctx.Extensions == nil {
		ctx.Extensions = map[string]runtime.Object{}
	}
	jsonNs, err := json.Marshal(ctx.Namespace)
	if err != nil {
		log.Logger().WithError(err).Warnf("fail to store previous namespace in %s", configAccess.GetDefaultFilename())
	} else {
		ctx.Extensions[configExtension] = &runtime.Unknown{
			Raw:         jsonNs,
			ContentType: runtime.ContentTypeJSON,
		}
	}
	ctx.Namespace = ns

	newConfig := *config
	err = clientcmd.ModifyConfig(configAccess, newConfig, false)
	if err != nil {
		return nil, fmt.Errorf("failed to update the kube config %s", err)
	}
	return ctx, nil
}

// ensureNamespaceExists checks the target namespace, creating it if create is set.
// When the namespace is missing, create takes precedence over quietMode: the user
// explicitly asked for it to be created. With only quietMode set the returned error
// is ErrNamespaceSkipped so the caller can exit successfully without terminating
// the process from a library function.
func ensureNamespaceExists(client kubernetes.Interface, ns string, create, quietMode bool) error {
	_, err := client.CoreV1().Namespaces().Get(context.TODO(), ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting namespace %q: %w", ns, err)
	}
	if create {
		return createNamespace(client, ns)
	}
	if quietMode {
		log.Logger().Infof("namespace %s does not exist yet", ns)
		return ErrNamespaceSkipped
	}
	return err
}

func createNamespace(client kubernetes.Interface, ns string) error {
	namespace := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
		},
	}
	_, err := client.CoreV1().Namespaces().Create(context.TODO(), &namespace, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create namespace %s: %w", ns, err)
	}
	return nil
}

func pickNamespace(o *Options, client kubernetes.Interface, defaultNamespace string) (string, error) {
	names, err := getNamespaceNames(client)
	if err != nil {
		return "", fmt.Errorf("retrieving namespace the names of the namespaces: %w", err)
	}

	selectedNamespace, err := o.pickName(names, defaultNamespace, "Change namespace:", "pick the kubernetes namespace for the current kubernetes cluster")
	if err != nil {
		return "", fmt.Errorf("picking the namespace: %w", err)
	}
	return selectedNamespace, nil
}

// getNamespaceNames returns the sorted list of environment names
func getNamespaceNames(client kubernetes.Interface) ([]string, error) {
	var names []string
	list, err := client.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return names, fmt.Errorf("loading namespaces %s", err)
	}
	for k := range list.Items {
		names = append(names, list.Items[k].Name)
	}
	sort.Strings(names)
	return names, nil
}

func (o *Options) pickName(names []string, defaultValue, message, help string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	if len(names) == 1 {
		return names[0], nil
	}
	if o.Input == nil {
		o.Input = survey.NewInput()
	}
	name, err := o.Input.PickNameWithDefault(names, message, defaultValue, help)
	return name, err
}

// IsInCluster tells if we are running incluster
func IsInCluster() bool {
	_, err := rest.InClusterConfig()
	return err == nil
}
