package namespace_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jxv1 "github.com/jenkins-x/jx-api/v4/pkg/apis/jenkins.io/v1"
	jxfake "github.com/jenkins-x/jx-api/v4/pkg/client/clientset/versioned/fake"
	"github.com/jenkins-x/jx/pkg/cmd/namespace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

const (
	previousNSKey = "previous-ns.jayex.io"
	devCluster    = "dev-cluster"
	otherCluster  = "other-cluster"
)

// kubeContext describes a context to put in a test kubeconfig. previous is the
// namespace recorded in the context's previous-ns.jayex.io extension, if any.
type kubeContext struct {
	name     string
	cluster  string
	ns       string
	previous string
}

// writeKubeconfig writes a kubeconfig to a temp dir and returns its path and a
// ConfigAccess pointing at it. It never touches the process environment or the
// user's real ~/.kube/config.
func writeKubeconfig(t *testing.T, contexts []kubeContext, current string) (string, clientcmd.ConfigAccess) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")

	cfg := api.NewConfig()
	for _, name := range []string{devCluster, otherCluster} {
		cfg.Clusters[name] = &api.Cluster{Server: "https://" + name + ":6443"}
	}
	cfg.AuthInfos["user"] = &api.AuthInfo{Token: "secret"}
	for _, c := range contexts {
		ctx := api.NewContext()
		ctx.Cluster = c.cluster
		ctx.AuthInfo = "user"
		ctx.Namespace = c.ns
		if c.previous != "" {
			raw, err := json.Marshal(c.previous)
			require.NoError(t, err)
			ctx.Extensions[previousNSKey] = &runtime.Unknown{Raw: raw, ContentType: runtime.ContentTypeJSON}
		}
		cfg.Contexts[c.name] = ctx
	}
	cfg.CurrentContext = current
	require.NoError(t, clientcmd.WriteToFile(*cfg, path))

	return path, &clientcmd.PathOptions{
		GlobalFile:   path,
		EnvVar:       "",
		LoadingRules: &clientcmd.ClientConfigLoadingRules{ExplicitPath: path, DoNotResolvePaths: true},
	}
}

// loadKubeconfig reads the kubeconfig back from disk.
func loadKubeconfig(t *testing.T, path string) *api.Config {
	t.Helper()
	cfg, err := clientcmd.LoadFromFile(path)
	require.NoError(t, err)
	return cfg
}

// contextNamespace returns the namespace of the named context on disk.
func contextNamespace(t *testing.T, path, name string) string {
	t.Helper()
	ctx := loadKubeconfig(t, path).Contexts[name]
	require.NotNil(t, ctx, "context %s missing from kubeconfig", name)
	return ctx.Namespace
}

// previousFromFile returns the raw previous-ns extension value of the named context
// and whether it is present.
func previousFromFile(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	ctx := loadKubeconfig(t, path).Contexts[name]
	require.NotNil(t, ctx, "context %s missing from kubeconfig", name)
	obj, ok := ctx.Extensions[previousNSKey]
	if !ok {
		return "", false
	}
	unknown, ok := obj.(*runtime.Unknown)
	require.True(t, ok, "extension has unexpected type %T", obj)
	var previous string
	require.NoError(t, json.Unmarshal(unknown.Raw, &previous))
	return previous, true
}

func newNamespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func newEnvironment(name, ns, targetNamespace string) *jxv1.Environment {
	return &jxv1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       jxv1.EnvironmentSpec{Namespace: targetNamespace},
	}
}

func namespaceExists(t *testing.T, client *fake.Clientset, name string) bool {
	t.Helper()
	_, err := client.CoreV1().Namespaces().Get(t.Context(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

// TestRunSwitchSequence walks the concrete sequence from the design:
//
//	dev-cluster/jx: jx ns jx-staging, jx ns -, jx ns -
//
// checking the kubeconfig file, including the recorded previous namespace, after
// every step. The record follows the kube context, so switching to another context
// and back in between does not change where "jx ns -" goes.
func TestRunSwitchSequence(t *testing.T) {
	path, access := writeKubeconfig(t, []kubeContext{
		{name: "dev", cluster: devCluster, ns: "jx"},
		{name: "other", cluster: otherCluster, ns: "prod"},
	}, "dev")

	kubeClient := fake.NewSimpleClientset(
		newNamespace("jx", nil),
		newNamespace("jx-staging", nil),
		newNamespace("prod", nil),
	)

	run := func(args ...string) {
		t.Helper()
		o := &namespace.Options{
			KubeClient:   kubeClient,
			ConfigAccess: access,
			Args:         args,
			BatchMode:    true,
		}
		require.NoError(t, o.Run())
	}

	// step 1: jx ns jx-staging
	run("jx-staging")
	assert.Equal(t, "jx-staging", contextNamespace(t, path, "dev"))
	previous, ok := previousFromFile(t, path, "dev")
	assert.True(t, ok, "previous namespace should have been recorded")
	assert.Equal(t, "jx", previous)

	// step 2: jx ns - goes back to jx and records jx-staging
	run("-")
	assert.Equal(t, "jx", contextNamespace(t, path, "dev"))
	previous, _ = previousFromFile(t, path, "dev")
	assert.Equal(t, "jx-staging", previous)

	// switch away to another context and back: the record is per context
	cfg := loadKubeconfig(t, path)
	cfg.CurrentContext = "other"
	require.NoError(t, clientcmd.WriteToFile(*cfg, path))
	cfg = loadKubeconfig(t, path)
	cfg.CurrentContext = "dev"
	require.NoError(t, clientcmd.WriteToFile(*cfg, path))

	// step 3: jx ns - still goes to jx-staging and records jx
	run("-")
	assert.Equal(t, "jx-staging", contextNamespace(t, path, "dev"))
	previous, _ = previousFromFile(t, path, "dev")
	assert.Equal(t, "jx", previous)

	// the other context was never touched
	assert.Equal(t, "prod", contextNamespace(t, path, "other"))
	_, ok = previousFromFile(t, path, "other")
	assert.False(t, ok, "the other context must not gain a previous namespace")
}

// TestRunPreviousNamespaceIsPerContext proves the record is scoped to the context:
// each context returns to its own previously selected namespace.
func TestRunPreviousNamespaceIsPerContext(t *testing.T) {
	path, access := writeKubeconfig(t, []kubeContext{
		{name: "dev", cluster: devCluster, ns: "jx-current", previous: "jx-previous"},
		{name: "other", cluster: otherCluster, ns: "prod-current", previous: "prod-previous"},
	}, "dev")

	kubeClient := fake.NewSimpleClientset(
		newNamespace("jx-previous", nil),
		newNamespace("prod-previous", nil),
	)

	o := &namespace.Options{KubeClient: kubeClient, ConfigAccess: access, Args: []string{"-"}, BatchMode: true}
	require.NoError(t, o.Run())
	assert.Equal(t, "jx-previous", contextNamespace(t, path, "dev"))
	assert.Equal(t, "prod-current", contextNamespace(t, path, "other"))
}

func TestRunTable(t *testing.T) {
	tests := []struct {
		name string
		// fixture
		namespaces []*corev1.Namespace
		envs       []*jxv1.Environment
		contexts   []kubeContext
		current    string

		// invocation
		args      []string
		env       string
		pick      bool
		create    bool
		quiet     bool
		batch     bool
		input     map[string]string
		inCluster bool

		// expectations
		wantErr      string
		wantSkip     bool
		wantNS       string            // namespace of the current context after Run
		wantPrevious map[string]string // expected previous-ns per context; "" means absent
		wantCreated  []string          // namespaces expected to exist (created) after Run
	}{
		{
			name:         "switches namespace and records the old one",
			namespaces:   []*corev1.Namespace{newNamespace("jx", nil), newNamespace("jx-staging", nil)},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:      "dev",
			args:         []string{"jx-staging"},
			batch:        true,
			wantNS:       "jx-staging",
			wantPrevious: map[string]string{"dev": "jx"},
		},
		{
			name:         "switching to the current namespace is a no-op and keeps the record",
			namespaces:   []*corev1.Namespace{newNamespace("jx", nil)},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx", previous: "jx-staging"}},
			current:      "dev",
			args:         []string{"jx"},
			batch:        true,
			wantNS:       "jx",
			wantPrevious: map[string]string{"dev": "jx-staging"},
		},
		{
			name:         "dash with no record fails in batch mode",
			namespaces:   []*corev1.Namespace{newNamespace("jx", nil)},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:      "dev",
			args:         []string{"-"},
			batch:        true,
			wantErr:      "no previous namespace",
			wantNS:       "jx",
			wantPrevious: map[string]string{"dev": ""},
		},
		{
			name:       "dash with no record fails instead of prompting in interactive mode",
			namespaces: []*corev1.Namespace{newNamespace("jx", nil), newNamespace("jx-staging", nil)},
			contexts:   []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:    "dev",
			args:       []string{"-"},
			batch:      false,
			// a selection would return jx-staging; it must never be consulted
			input:        map[string]string{"Change namespace:": "jx-staging"},
			wantErr:      "no previous namespace",
			wantNS:       "jx",
			wantPrevious: map[string]string{"dev": ""},
		},
		{
			name:         "dash where the recorded namespace was deleted keeps the current one",
			namespaces:   []*corev1.Namespace{newNamespace("jx", nil)},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx", previous: "deleted"}},
			current:      "dev",
			args:         []string{"-"},
			batch:        true,
			wantNS:       "jx",
			wantPrevious: map[string]string{"dev": "deleted"},
		},
		{
			name:       "dash without a current context errors and does not touch the file",
			namespaces: []*corev1.Namespace{newNamespace("jx", nil), newNamespace("jx-staging", nil)},
			contexts:   []kubeContext{},
			current:    "",
			args:       []string{"-"},
			batch:      true,
			wantErr:    "no current Kubernetes context",
		},
		{
			name:       "switching without a context only changes memory",
			namespaces: []*corev1.Namespace{newNamespace("jx-staging", nil)},
			contexts:   []kubeContext{},
			current:    "",
			args:       []string{"jx-staging"},
			batch:      true,
		},
		{
			name:         "create creates the namespace and switches",
			namespaces:   []*corev1.Namespace{newNamespace("jx", nil)},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:      "dev",
			args:         []string{"brand-new"},
			create:       true,
			batch:        true,
			wantNS:       "brand-new",
			wantCreated:  []string{"brand-new"},
			wantPrevious: map[string]string{"dev": "jx"},
		},
		{
			name:         "create wins over quiet",
			namespaces:   []*corev1.Namespace{newNamespace("jx", nil)},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:      "dev",
			args:         []string{"brand-new"},
			create:       true,
			quiet:        true,
			batch:        true,
			wantNS:       "brand-new",
			wantCreated:  []string{"brand-new"},
			wantPrevious: map[string]string{"dev": "jx"},
		},
		{
			name:         "quiet with a missing namespace reports skipped and changes nothing",
			namespaces:   []*corev1.Namespace{newNamespace("jx", nil)},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:      "dev",
			args:         []string{"missing"},
			quiet:        true,
			batch:        true,
			wantSkip:     true,
			wantNS:       "jx",
			wantPrevious: map[string]string{"dev": ""},
		},
		{
			name:       "missing namespace without quiet returns the API error",
			namespaces: []*corev1.Namespace{newNamespace("jx", nil)},
			contexts:   []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:    "dev",
			args:       []string{"missing"},
			batch:      true,
			wantErr:    "not found",
			wantNS:     "jx",
		},
		{
			name: "env found in the current namespace wins",
			namespaces: []*corev1.Namespace{
				newNamespace("jx", nil),
				newNamespace("jx-dev", nil),
				newNamespace("jx-staging", nil),
				newNamespace("jx-dev-staging", nil),
			},
			envs: []*jxv1.Environment{
				newEnvironment("staging", "jx", "jx-staging"),
				newEnvironment("staging", "jx-dev", "jx-dev-staging"),
			},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx-dev"}},
			current:      "dev",
			env:          "staging",
			batch:        true,
			wantNS:       "jx-dev-staging",
			wantPrevious: map[string]string{"dev": "jx-dev"},
		},
		{
			name: "env falls back to the dev namespace when the current one has none",
			namespaces: []*corev1.Namespace{
				newNamespace("jx-dev", map[string]string{"team": "jx"}),
				newNamespace("jx", nil),
				newNamespace("jx-staging", nil),
			},
			envs: []*jxv1.Environment{
				newEnvironment("staging", "jx", "jx-staging"),
			},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx-dev"}},
			current:      "dev",
			env:          "staging",
			batch:        true,
			wantNS:       "jx-staging",
			wantPrevious: map[string]string{"dev": "jx-dev"},
		},
		{
			name:       "env not found lists the available environments",
			namespaces: []*corev1.Namespace{newNamespace("jx-dev", nil)},
			envs: []*jxv1.Environment{
				newEnvironment("staging", "jx-dev", "jx-dev-staging"),
			},
			contexts: []kubeContext{{name: "dev", cluster: devCluster, ns: "jx-dev"}},
			current:  "dev",
			env:      "prod",
			batch:    true,
			wantErr:  "Invalid argument: prod",
			wantNS:   "jx-dev",
		},
		{
			name:         "env with an empty Spec.Namespace is an error",
			namespaces:   []*corev1.Namespace{newNamespace("jx", nil)},
			envs:         []*jxv1.Environment{newEnvironment("staging", "jx", "")},
			contexts:     []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:      "dev",
			env:          "staging",
			batch:        true,
			wantErr:      "does not specify a namespace",
			wantNS:       "jx",
			wantPrevious: map[string]string{"dev": ""},
		},
		{
			name:       "env together with a positional argument is a conflict",
			namespaces: []*corev1.Namespace{newNamespace("jx", nil), newNamespace("jx-staging", nil)},
			envs:       []*jxv1.Environment{newEnvironment("staging", "jx", "jx-staging")},
			contexts:   []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:    "dev",
			env:        "staging",
			args:       []string{"jx-staging"},
			batch:      true,
			wantErr:    "cannot use both --env and a namespace argument",
			wantNS:     "jx",
		},
		{
			name:       "missing namespace without batch mode drops into the picker",
			namespaces: []*corev1.Namespace{newNamespace("jx", nil), newNamespace("jx-staging", nil)},
			contexts:   []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}},
			current:    "dev",
			input:      map[string]string{"Change namespace:": "jx-staging"},
			batch:      false,
			wantNS:     "jx-staging",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, access := writeKubeconfig(t, tt.contexts, tt.current)
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			kubeObjects := make([]runtime.Object, 0, len(tt.namespaces))
			for _, ns := range tt.namespaces {
				kubeObjects = append(kubeObjects, ns)
			}
			kubeClient := fake.NewSimpleClientset(kubeObjects...)
			jxObjects := make([]runtime.Object, 0, len(tt.envs))
			for _, e := range tt.envs {
				jxObjects = append(jxObjects, e)
			}
			jxClient := jxfake.NewSimpleClientset(jxObjects...)

			o := &namespace.Options{
				KubeClient:   kubeClient,
				JXClient:     jxClient,
				ConfigAccess: access,
				Args:         tt.args,
				Env:          tt.env,
				PickEnv:      tt.pick,
				Create:       tt.create,
				QuiteMode:    tt.quiet,
				BatchMode:    tt.batch,
			}
			if tt.input != nil {
				o.Input = &fakeInput{values: tt.input}
			}

			err = o.Run()

			if tt.wantSkip {
				assert.ErrorIs(t, err, namespace.ErrNamespaceSkipped, "quiet mode should report the skip as a sentinel error")
			} else if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}

			// kubeconfig on disk
			after, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			if len(tt.contexts) == 0 {
				// no current context: the switch is memory-only and nothing is written
				assert.Equal(t, string(before), string(after), "kubeconfig must not be modified without a context")
			}
			for _, c := range tt.contexts {
				if want, ok := tt.wantPrevious[c.name]; ok {
					got, exists := previousFromFile(t, path, c.name)
					if want == "" {
						assert.False(t, exists, "context %s should have no previous namespace", c.name)
					} else {
						assert.True(t, exists, "context %s should have a previous namespace", c.name)
						assert.Equal(t, want, got, "previous namespace of context %s", c.name)
					}
				}
			}
			if len(tt.contexts) > 0 && tt.wantNS != "" {
				assert.Equal(t, tt.wantNS, contextNamespace(t, path, tt.current))
			}
			if (tt.wantErr != "" || tt.wantSkip) && len(tt.contexts) > 0 {
				// failures must leave the current namespace as it was
				assert.Equal(t, tt.contexts[len(tt.contexts)-1].ns, contextNamespace(t, path, tt.current))
			}
			for _, name := range tt.wantCreated {
				assert.True(t, namespaceExists(t, kubeClient, name), "namespace %s should exist", name)
			}
		})
	}
}

// TestRunQuietDoesNotExitProcess ensures quiet mode returns a sentinel instead of
// terminating the process: reaching the assertion at all proves os.Exit was not hit.
func TestRunQuietDoesNotExitProcess(t *testing.T) {
	path, access := writeKubeconfig(t, []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}}, "dev")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	o := &namespace.Options{
		KubeClient:   fake.NewSimpleClientset(newNamespace("jx", nil)),
		ConfigAccess: access,
		Args:         []string{"nope"},
		QuiteMode:    true,
		BatchMode:    true,
	}
	err = o.Run()
	require.ErrorIs(t, err, namespace.ErrNamespaceSkipped)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// TestPreviousNamespaceExtensionFormat pins the on-disk encoding of the record so
// upgrades keep reading records written by older releases.
func TestPreviousNamespaceExtensionFormat(t *testing.T) {
	path, access := writeKubeconfig(t, []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}}, "dev")
	kubeClient := fake.NewSimpleClientset(newNamespace("jx", nil), newNamespace("jx-staging", nil))

	o := &namespace.Options{
		KubeClient:   kubeClient,
		ConfigAccess: access,
		Args:         []string{"jx-staging"},
		BatchMode:    true,
	}
	require.NoError(t, o.Run())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(raw)
	assert.Contains(t, text, previousNSKey, "the extension key is part of the on-disk format")
	assert.Contains(t, text, "extension: jx", "the previous namespace is stored as a JSON string")

	cfg := loadKubeconfig(t, path)
	unknown, ok := cfg.Contexts["dev"].Extensions[previousNSKey].(*runtime.Unknown)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(string(unknown.Raw), `"`), `record should be a JSON string, got %s`, unknown.Raw)
	var previous string
	require.NoError(t, json.Unmarshal(unknown.Raw, &previous))
	assert.Equal(t, "jx", previous)
}

// TestRunEnvPickUsesInjectedInput covers the interactive --pick path through the
// injectable input interface.
func TestRunEnvPickUsesInjectedInput(t *testing.T) {
	path, access := writeKubeconfig(t, []kubeContext{{name: "dev", cluster: devCluster, ns: "jx"}}, "dev")
	kubeClient := fake.NewSimpleClientset(newNamespace("jx", nil), newNamespace("jx-staging", nil))
	jxClient := jxfake.NewSimpleClientset(
		newEnvironment("staging", "jx", "jx-staging"),
		newEnvironment("dev", "jx", "jx"),
	)

	o := &namespace.Options{
		KubeClient:   kubeClient,
		JXClient:     jxClient,
		ConfigAccess: access,
		PickEnv:      true,
		BatchMode:    true,
		Input:        &fakeInput{values: map[string]string{"Pick environment:": "staging"}},
	}
	require.NoError(t, o.Run())
	assert.Equal(t, "jx-staging", contextNamespace(t, path, "dev"))
}

// fakeInput is a minimal input.Interface returning canned answers.
type fakeInput struct {
	values map[string]string
}

func (f *fakeInput) PickValue(message, defaultValue string, _ bool, _ string) (string, error) {
	if v, ok := f.values[message]; ok {
		return v, nil
	}
	return defaultValue, nil
}

func (f *fakeInput) PickValidValue(message, defaultValue string, _ func(interface{}) error, help string) (string, error) {
	return f.PickValue(message, defaultValue, false, help)
}

func (f *fakeInput) PickPassword(string, string) (string, error) {
	return "", errors.New("not implemented")
}

func (f *fakeInput) PickNameWithDefault(_ []string, message string, defaultValue interface{}, _ string) (string, error) {
	if v, ok := f.values[message]; ok {
		return v, nil
	}
	if s, ok := defaultValue.(string); ok {
		return s, nil
	}
	return "", nil
}

func (f *fakeInput) SelectNamesWithFilter(_ []string, message string, _ bool, _ string, help string) ([]string, error) {
	v, err := f.PickValue(message, "", false, help)
	return []string{v}, err
}

func (f *fakeInput) SelectNames(_ []string, message string, _ bool, help string) ([]string, error) {
	v, err := f.PickValue(message, "", false, help)
	return []string{v}, err
}

func (f *fakeInput) Confirm(message string, defaultValue bool, help string) (bool, error) {
	v, err := f.PickValue(message, "", false, help)
	return v == "true", err
}
