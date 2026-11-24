# `jx ns` — previous namespace and offline testability

This note records the behaviour fixed in `pkg/cmd/namespace`, how `jx ns -` works, and
the rollback path for each deliberate behaviour change.

## How the previous namespace is recorded

Before changing the namespace, `changeNamespace` serialises the context's *current*
namespace as a JSON string and stores it in that context's extensions under
`previous-ns.jayex.io`. `jx ns -` reads that field and performs another switch, so two
`jx ns -` calls return to the original namespace.

The record is **per kube context**. Switching to another context with
`kubectl config use-context` and back returns to *that context's* own previously selected
namespace, not a global "last namespace". For the sequence

```
dev-cluster, current namespace jx:
  jx ns jx-staging   -> namespace jx-staging, previous recorded as jx
  jx ns -            -> namespace jx,         previous recorded as jx-staging
  jx ns -            -> namespace jx-staging, previous recorded as jx
```

switching away after step 2 and back still makes step 3 return to `jx-staging`, because
the record travelled with the `dev-cluster` context. This is covered end-to-end (checking
the kubeconfig file after each step) by `TestRunSwitchSequence`.

The extension key (`previous-ns.jayex.io`) and the JSON encoding (a JSON string) are part
of the on-disk format and are unchanged, so records written by older releases are still
read, and older releases can still read records written here. `TestPreviousNamespaceExtensionFormat`
pins this.

## Edge cases now fixed

- **`jx ns -` with no record.** Previously the namespace became `""` and interactive mode
  fell into the namespace picker, so a user typing `-` was shown a selection list. Now:
  an explicit error is returned in both batch and interactive mode
  (`no previous namespace was recorded for context ...; select one first with 'jx ns <namespace>'`).
- **`jx ns -` when the recorded namespace was deleted.** A warning is logged and the
  current namespace is kept; nothing is changed.
- **`jx ns -` when there is no current context** (e.g. running inside a pod).
  `kube.CurrentContext` can return `nil`; the code used to dereference `ctx.Extensions`
  before checking. It now checks first and returns a clear error — there is no previous
  namespace in that situation.
- **Switching with no current context** creates an in-memory `pod` context only; it does
  not write the kubeconfig file. This is unchanged and covered by a test case.
- **Record equals the current namespace.** `jx ns <current>` is treated as no change and
  does **not** overwrite the existing record.
- **`--env` and a positional namespace together** now report a conflict instead of
  silently ignoring the positional argument.
- **`--env` with an Environment whose `Spec.Namespace` is empty** now reports an error
  instead of returning an empty namespace and silently doing nothing.
- **`--env` name resolution** prefers the current namespace; it only falls back to the dev
  (team) namespace when the current namespace has no Environments.

## Testability

`Options` now carries an optional `clientcmd.ConfigAccess`. When nil, behaviour is
unchanged (`kubeclient.LoadConfig` / the `KUBECONFIG` env var / `~/.kube/config`). Tests
inject an accessor backed by a file in `t.TempDir()`, so the kubeconfig is read and
written without touching the process environment. The Kubernetes and JX clients use
`fake.NewSimpleClientset` / `jxfake.NewSimpleClientset`, and input goes through the
existing injectable `input.Interface`.

Run the tests with:

```
GOEXPERIMENT=nojsonv2 go test ./pkg/cmd/namespace/
```

(`GOEXPERIMENT=nojsonv2` is needed to work around a pre-existing `k8s.io/kube-openapi`
build issue on this toolchain; it is unrelated to this command.)

## Behaviour changes and rollback

These three changes are user-visible. Each has a small, local revert:

1. **`jx ns -` without a record is now an error** (in both batch and interactive mode)
   instead of dropping into the picker.
   *Rollback:* in `Run`, when `previousNamespace` reports "no record", restore the old
   path that leaves `ns == ""` so a non-batch run reaches `pickNamespace`. The helper
   `previousNamespace` already separates "no record" from other errors, so this is a
   localised change in one branch.

2. **`--env` together with a positional namespace is now a conflict** instead of the
   positional argument being ignored.
   *Rollback:* delete the `if o.Env != "" && argNS != "" { return ... }` guard in `Run`;
   `argNS` is then ignored whenever `--env` is set, as before.

3. **`--quiet` no longer exits the process.** It returns the sentinel
   `namespace.ErrNamespaceSkipped`, and only the cobra `Run` wrapper turns that into a
   successful (status 0) exit. Callers of `Run` can now do their own cleanup.
   *Rollback:* in `ensureNamespaceExists`, replace `return ErrNamespaceSkipped` with the
   old `os.Exit(0); return nil`, and remove the `errors.Is(err, ErrNamespaceSkipped)`
   branch from the cobra `Run` wrapper. Note this reintroduces the untestability that
   made `Run` impossible to exercise in a unit test, which is why it was changed.

The `Now using namespace` and `Using namespace` output strings are unchanged; scripts
parse them.
