package localresource

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pkg/errors"

	commonv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	record "k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	plumbingevent "github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/kubeutil/eventrecorder"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/meta"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"

	"github.com/krateo-platformops/plumbing/ptr"
	contexttools "github.com/krateo-platformops/provider-runtime/pkg/context"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	localResourcev1alpha1 "github.com/krateoplatformops/git-provider/apis/localresource/v1alpha1"
	"github.com/krateoplatformops/git-provider/internal/clients/git"
	credentialhelper "github.com/krateoplatformops/git-provider/internal/controllers/common/credentialHelper"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/footer"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/option"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/templating"
	"github.com/krateoplatformops/git-provider/internal/tools/copier"
	"github.com/krateoplatformops/git-provider/internal/tools/localfs"
	"github.com/krateoplatformops/git-provider/internal/tools/template"

	corev1 "k8s.io/api/core/v1"
)

const (
	errNotLocalResource = "managed resource is not a LocalResource custom resource"
)

// Setup adds a controller that reconciles Token managed resources.
func Setup(mgr ctrl.Manager, o option.SetupOptions) error {
	name := reconciler.ControllerName(localResourcev1alpha1.LocalResourceGroupKind)

	log := o.Controller.Logger.WithValues("controller", name)

	recorder, err := eventrecorder.Create(context.Background(), mgr.GetConfig(), name, nil)
	if err != nil {
		return fmt.Errorf("failed to create event recorder: %w", err)
	}

	r := reconciler.NewReconciler(mgr,
		resource.ManagedKind(localResourcev1alpha1.LocalResourceGroupVersionKind),
		reconciler.WithExternalConnecter(&connector{
			kube:     mgr.GetClient(),
			log:      log,
			dynamic:  dynamic.NewForConfigOrDie(mgr.GetConfig()),
			recorder: recorder,
			homeDir:  o.Git.HomeDir,
		}),
		reconciler.WithPollInterval(o.Controller.PollInterval),
		reconciler.WithLogger(log),
		reconciler.WithRecorder(plumbingevent.NewAPIRecorder(recorder)),
		reconciler.WithTimeout(o.Controller.Timeout),
	)

	git.CommitAuthorEmail = o.Git.CommitAuthorEmail
	git.CommitAuthorName = o.Git.CommitAuthorName

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.Controller.ForControllerRuntime()).
		For(&localResourcev1alpha1.LocalResource{}).
		Complete(ratelimiter.New(name, r, o.Controller.GlobalRateLimiter))
}

type connector struct {
	kube     client.Client
	dynamic  dynamic.Interface
	log      logging.Logger
	recorder record.EventRecorder
	homeDir  string
}
type gitClientOpts struct {
	Insecure                bool
	UnsupportedCapabilities bool
	ToRepoCreds             *credentialhelper.Credentials
	HomeDir                 string
}

func (c *connector) Connect(ctx context.Context, mg resource.Managed) (reconciler.ExternalClient, error) {
	cr, ok := mg.(*localResourcev1alpha1.LocalResource)
	if !ok {
		return nil, errors.New(errNotLocalResource)
	}

	token, err := resource.GetSecret(ctx, c.kube, cr.Spec.ToRepo.Credentials.SecretRef)
	if err != nil {
		return nil, fmt.Errorf("retrieving .toRepo token: %w", err)
	}

	credOpts := credentialhelper.CredentialHelperOpts{
		AuthMethod: cr.Spec.ToRepo.Credentials.AuthMethod,
		Token:      token,
	}

	// Only basic auth consumes a username: GetCredentials drops it for bearer and cookiefile, and
	// both the CRD field description and docs/local-resource.md document usernameRef as ignored
	// there. Resolving it unconditionally made a nil ref a hard connect failure ("retrieving
	// .toRepo username: no credentials secret referenced"), so no bearer LocalResource could ever
	// reconcile. This mirrors what the repo controller already does in getRepoCredentials.
	if credentialhelper.UsesUsername(credOpts.AuthMethod) {
		credOpts.Username = credentialhelper.DefaultUsername
		if cr.Spec.ToRepo.Credentials.UsernameRef != nil {
			username, err := resource.GetSecret(ctx, c.kube, cr.Spec.ToRepo.Credentials.UsernameRef)
			if err != nil {
				return nil, fmt.Errorf("retrieving .toRepo username: %w", err)
			}
			credOpts.Username = username
		}
	}

	creds, err := credentialhelper.GetCredentials(credOpts)
	if err != nil {
		return nil, fmt.Errorf("getting .toRepo credentials: %w", err)
	}

	cfg := &gitClientOpts{
		Insecure:                cr.Spec.Insecure,
		UnsupportedCapabilities: cr.Spec.UnsupportedCapabilities,
		ToRepoCreds:             creds,
	}

	log := c.log.WithValues("name", cr.Name, "namespace", cr.Namespace)

	return &external{
		kube:    c.kube,
		log:     log,
		cfg:     cfg,
		dynamic: c.dynamic,
		rec:     c.recorder,
	}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	kube    client.Client
	log     logging.Logger
	cfg     *gitClientOpts
	dynamic dynamic.Interface
	rec     record.EventRecorder
}

func (e *external) Observe(ctx context.Context, mg resource.Managed) (reconciler.ExternalObservation, error) {
	cr, ok := mg.(*localResourcev1alpha1.LocalResource)
	if !ok {
		return reconciler.ExternalObservation{}, errors.New(errNotLocalResource)
	}

	log := e.log.WithValues("operation", "observe")

	ctx = contexttools.CtxWithLogger(ctx, log)

	log.Debug("Observing resource")

	if cr.GetCondition(commonv1.TypeReady).Reason == commonv1.ReasonDeleting {
		return reconciler.ExternalObservation{
			ResourceExists:   false,
			ResourceUpToDate: true,
		}, nil
	}

	if !cr.DeletionTimestamp.IsZero() && cr.GetCondition(commonv1.TypeSynced).Reason == commonv1.ReasonReconcileError {
		if !meta.IsActionAllowed(cr, meta.ActionDelete) {
			log.Debug("External resource should not be deleted by provider, skip deleting.")
		} else {
			return reconciler.ExternalObservation{
				ResourceExists:   false,
				ResourceUpToDate: true,
			}, nil
		}
	}

	if cr.Status.TargetCommitId != "" {
		meta.SetExternalName(cr, cr.Status.TargetCommitId)
	}

	var hasGitProviderPreviuslyCommitted bool
	if hash, err := git.IsFuncInGitCommitHistory(ctx, git.ListOptions{
		URL:        cr.Spec.ToRepo.Url,
		Auth:       e.cfg.ToRepoCreds.Transport,
		Insecure:   e.cfg.Insecure,
		Branch:     cr.Spec.ToRepo.Branch,
		GitCookies: e.cfg.ToRepoCreds.Cookie,
		HomeDir:    e.cfg.HomeDir, // Use the configured home directory for temporary files
	}, func(commit *object.Commit) bool {
		e.log.Debug("Analyzing commit", "commitId", commit.Hash.String())
		namespace, name, specHash, err := footer.ParseLocalResourceCommitFooter(commit.Message)
		if err != nil {
			return false
		}
		e.log.Debug("Parsed commit footer", "namespace", namespace, "name", name, "specHash", specHash)
		if namespace == cr.GetNamespace() && name == cr.GetName() {
			hasGitProviderPreviuslyCommitted = true
			e.log.Debug("Found previous commit from git-provider for this LocalResource", "commitId", commit.Hash.String())
		}
		currentSpecHash, err := footer.CalculateLocalResourceSpecHash(ctx, cr, e.dynamic)
		if err != nil {
			return false
		}

		e.log.Debug("Comparing spec hash with commit footer hash", "specHash", currentSpecHash, "commitFooterHash", specHash)
		return specHash == currentSpecHash
	}); err != nil {
		log.Debug("Unable to check if target LocalResource spec is up-to-date", "msg", err.Error())
		return reconciler.ExternalObservation{}, err
	} else if hash.IsZero() && hasGitProviderPreviuslyCommitted {
		log.Debug("Target LocalResource spec is not up-to-date", "commitId", cr.Status.TargetCommitId, "branch", cr.Status.TargetBranch)
		return reconciler.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	} else if !hash.IsZero() && hasGitProviderPreviuslyCommitted {
		meta.SetExternalName(cr, hash.String())
		cr.Status.TargetCommitId = hash.String()
		cr.Status.TargetBranch = cr.Spec.ToRepo.Branch

		log.Debug("Target LocalResource spec is synced", "commitId", cr.Status.TargetCommitId, "branch", cr.Status.TargetBranch)
	} else {
		log.Debug("No previous commit from git-provider found for this LocalResource", "hash", hash.String(), "hasGitProviderPreviuslyCommitted", hasGitProviderPreviuslyCommitted)
	}

	if meta.GetExternalName(cr) == "" {
		return reconciler.ExternalObservation{
			ResourceExists:   false,
			ResourceUpToDate: true,
		}, nil
	}

	cr.Status.SetConditions(commonv1.Available())

	return reconciler.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: true,
	}, nil
}

func (e *external) Create(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*localResourcev1alpha1.LocalResource)
	if !ok {
		return errors.New(errNotLocalResource)
	}
	log := e.log.WithValues("operation", "create")
	ctx = contexttools.CtxWithLogger(ctx, log)
	if !meta.IsActionAllowed(cr, meta.ActionCreate) {
		log.Debug("External resource should not be created by provider, skip creating.")
		return nil
	}
	log.Info("Creating resource")
	cr.Status.SetConditions(commonv1.Creating())
	return e.syncWithRetry(ctx, cr, cr.Spec.CreateCommitMessage)
}

func (e *external) Update(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*localResourcev1alpha1.LocalResource)
	if !ok {
		return errors.New(errNotLocalResource)
	}
	log := e.log.WithValues("operation", "update")
	ctx = contexttools.CtxWithLogger(ctx, log)
	if !cr.Spec.SyncEnabled {
		log.Warn("External resource should not be updated by provider, skip updating. SyncEnabled is false.")
		return nil
	}
	if !meta.IsActionAllowed(cr, meta.ActionUpdate) {
		log.Debug("External resource should not be updated by provider, skip updating.")
		return nil
	}

	log.Info("Updating resource")
	cr.Status.SetConditions(commonv1.Creating())
	return e.syncWithRetry(ctx, cr, cr.Spec.UpdateCommitMessage)
}

func (e *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*localResourcev1alpha1.LocalResource)
	if !ok {
		return errors.New(errNotLocalResource)
	}
	log := e.log.WithValues("operation", "delete")
	// You may want to use the logger in the context for further calls
	// ctx = contexttools.CtxWithLogger(ctx, log)
	if !meta.IsActionAllowed(cr, meta.ActionDelete) {
		log.Debug("External resource should not be deleted by provider, skip deleting.")
		return nil
	}

	log.Info("Deleting resource")

	cr.Status.SetConditions(commonv1.Deleting())

	return nil // noop
}

// refContentionRetry paces re-attempts when a concurrent push loses the ref race. Jittered by the
// caller so siblings that started together do not retry in lockstep and collide again.
var refContentionRetry = wait.Backoff{Duration: 400 * time.Millisecond, Factor: 2.0, Jitter: 0.5, Steps: 6}

// isRefContention reports whether err is another writer having moved the branch first.
//
// A publish fans out one LocalResource PER FILE and they all push to the SAME branch at once. Git
// only advances a ref from the commit the pusher expects, so the losers get:
//
//	cannot lock ref 'refs/heads/builder/<slug>': is at <sha> but expected <sha>
//
// That is the ordinary outcome of concurrent writers, not a failure of this resource. Retrying is
// the correct response — and it must re-CLONE, which is why syncWithRetry retries the whole sync
// rather than just the push: the local ref is stale, so pushing it again fails identically.
func isRefContention(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "cannot lock ref") ||
		strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "reference already exists")
}

// syncWithRetry runs SyncLocalResources, re-cloning and retrying while the branch is being moved by
// a sibling. Any other error is returned immediately — this is a contention retry, not a blanket one.
func (e *external) syncWithRetry(ctx context.Context, cr *localResourcev1alpha1.LocalResource, commitMessage string) error {
	log := contexttools.LoggerFromCtx(ctx, e.log)
	var lastErr error
	attempt := 0
	err := wait.ExponentialBackoffWithContext(ctx, refContentionRetry, func(ctx context.Context) (bool, error) {
		attempt++
		lastErr = e.SyncLocalResources(ctx, cr, commitMessage)
		if lastErr == nil {
			return true, nil
		}
		if !isRefContention(lastErr) {
			return false, lastErr
		}
		log.Debug("branch moved by a concurrent push, re-cloning and retrying",
			"attempt", attempt, "branch", cr.Spec.ToRepo.Branch, "err", lastErr.Error())
		return false, nil
	})
	if wait.Interrupted(err) && lastErr != nil {
		return lastErr
	}
	return err
}

func (e *external) SyncLocalResources(ctx context.Context, cr *localResourcev1alpha1.LocalResource, commitMessage string) error {
	spec := cr.Spec.DeepCopy()

	toRepo, err := git.Clone(git.CloneOptions{
		URL:                     spec.ToRepo.Url,
		Auth:                    e.cfg.ToRepoCreds.Transport,
		Insecure:                e.cfg.Insecure,
		UnsupportedCapabilities: e.cfg.UnsupportedCapabilities,
		Branch:                  spec.ToRepo.Branch,
		AlternativeBranch:       ptr.To(cr.Spec.ToRepo.CloneFromBranch),
		GitCookies:              e.cfg.ToRepoCreds.Cookie,
		HomeDir:                 e.cfg.HomeDir, // Use the configured home directory for temporary files
	})
	if err != nil {
		return fmt.Errorf("cloning toLocalResource: %w", err)
	}
	defer toRepo.Cleanup()

	log := contexttools.LoggerFromCtx(ctx, e.log)

	log.Debug("Target LocalResource cloned", "url", spec.ToRepo.Url)
	// `action` (5th arg) is REQUIRED by the events API. It was "", so the apiserver rejected every
	// event this controller emitted — "Event ... is invalid: action: Required value" — and the
	// controller produced no usable event trail at all, which is part of why the wedge below was
	// hard to diagnose from the cluster.
	e.rec.Eventf(cr, nil, corev1.EventTypeNormal, "TargetLocalResourceCloned", "Clone",
		"Successfully cloned target LocalResource: %s", spec.ToRepo.Url)
	log.Debug(fmt.Sprintf("Target LocalResource on branch %s", toRepo.CurrentBranch()))

	fromLocal, err := localfs.NewLocalFS(e.cfg.HomeDir)
	if err != nil {
		return fmt.Errorf("creating local filesystem: %w", err)
	}
	defer fromLocal.Cleanup()

	fromPath := "/"
	toPath := spec.ToRepo.Path
	override := spec.Override
	if len(toPath) == 0 {
		toPath = "/"
	}

	filename := spec.FromResource.FileName
	if spec.FromResource.FromYaml != nil {
		filename, err = fromLocal.WriteK8sResource(filename, *spec.FromResource.FromYaml)
		if err != nil {
			return fmt.Errorf("writing fromResource.FromYaml to local filesystem: %w", err)
		}
		log.Debug("fromResource.FromYaml written to local filesystem", "fileName", filename)
	} else if spec.FromResource.FromRef != nil {
		gv, err := schema.ParseGroupVersion(spec.FromResource.FromRef.ApiVersion)
		if err != nil {
			return fmt.Errorf("parsing group version from fromResource.FromRef: %w", err)
		}

		var cli dynamic.ResourceInterface
		if spec.FromResource.FromRef.Namespace == "" {
			cli = e.dynamic.Resource(schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: spec.FromResource.FromRef.Resource,
			})
		} else {
			cli = e.dynamic.Resource(schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: spec.FromResource.FromRef.Resource,
			}).Namespace(spec.FromResource.FromRef.Namespace)
		}
		uRes, err := cli.Get(ctx, spec.FromResource.FromRef.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting resource from fromResource.FromRef: %w", err)
		}
		filename, err = fromLocal.WriteK8sResource(filename, runtime.RawExtension{Object: uRes})
		if err != nil {
			return fmt.Errorf("writing fromResource.FromRef to local filesystem: %w", err)
		}
		log.Debug("fromResource.FromRef written to local filesystem", "fileName", filename)
	} else if spec.FromResource.FromString != nil {
		filename, err = fromLocal.WriteStringResource(filename, *spec.FromResource.FromString)
		if err != nil {
			return fmt.Errorf("writing fromResource.FromString to local filesystem: %w", err)
		}
		log.Debug("fromResource.FromString written to local filesystem", "fileName", filename)
	}

	var values []template.TemplateValue
	if spec.PlaceholdersToOverride != nil {
		for _, p := range spec.PlaceholdersToOverride {
			values = append(values, template.TemplateValue{
				Key:   p.Name,
				Value: p.Value,
			})
		}
	}
	opts := []copier.Option{
		copier.WithOriginCopyPath(fromPath),
		copier.WithTargetCopyPath(toPath),
	}

	tplOpts, err := templatingOptions(cr.GetAnnotations(), values)
	if err != nil {
		return err
	}
	opts = append(opts, tplOpts...)

	co, err := copier.NewCopier(fromLocal, toRepo.FS(), opts...)
	if err != nil {
		return fmt.Errorf("unable to create copier: %w", err)
	}
	if err := co.Copy(override); err != nil {
		// Name the offending files on the status. The renderer's own message positions itself
		// against an anonymous template, so without this the operator sees a line number with
		// no file attached.
		var failed *copier.RenderFailed
		if errors.As(err, &failed) {
			cr.Status.TemplatingErrors = templating.StatusErrors(failed.Errors)
		}
		return fmt.Errorf("unable to copy files: %w", err)
	}
	cr.Status.TemplatingErrors = nil

	log.Debug("Origin and target LocalResource synchronized",
		"toUrl", spec.ToRepo.Url,
		"fromPath", fromPath,
		"toPath", toPath)

	commitFooter, err := footer.LocalResourceCommitFooter(ctx, cr, e.dynamic)
	if err != nil {
		return fmt.Errorf("unable to compute LocalResource commit footer: %w", err)
	}
	commitMessage = fmt.Sprintf("%s\n\n%s", commitMessage, commitFooter)
	toLocalResourceCommitIdObj, err := toRepo.Commit(".", commitMessage, &git.IndexOptions{
		OriginRepo: nil,
		FromPath:   fromPath,
		ToPath:     toPath,
	})
	toLocalResourceCommitId := toLocalResourceCommitIdObj.String()
	if err == git.NoErrAlreadyUpToDate {
		toLocalResourceCommitId, err := toRepo.GetLatestCommit(toRepo.CurrentBranch())
		if err != nil {
			return fmt.Errorf("unable to get latest commit from target LocalResource: %w", err)
		}
		log.Debug("Target LocalResource not commited", "branch", toRepo.CurrentBranch(), "status", "repository already up-to-date")

		meta.SetExternalName(cr, toLocalResourceCommitId)
		cr.Status.TargetCommitId = toLocalResourceCommitId
		cr.Status.TargetBranch = toRepo.CurrentBranch()

		// SAME RETRY AS THE COMMITTED PATH, and this branch needs it MORE, not less.
		//
		// A bare Update here is byte-for-byte the failure #16 describes: lose the conflict and the
		// sync returns with external-create-pending set and no recorded result. This variant is the
		// one that cannot self-heal — the branch makes NO commit, so a later Observe finds no footer
		// to recognise the resource by and cannot adopt it.
		//
		// The sync retry added in #17 makes this path MORE likely to be taken, not less: a retried
		// attempt re-clones a tree that may already contain the file from the attempt that actually
		// won the push, so the commit becomes a no-op and lands exactly here.
		if err := e.updateStatusWithRetry(ctx, cr); err != nil {
			return fmt.Errorf("unable to update status: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("unable to commit target LocalResource: %w", err)
	}
	log.Debug("Target LocalResource committed", "branch", toRepo.CurrentBranch(), "commitId", toLocalResourceCommitId)

	err = toRepo.Push("origin", toRepo.CurrentBranch(), e.cfg.Insecure)
	if err != nil {
		return fmt.Errorf("unable to push target LocalResource: %w", err)
	}
	log.Info("Target LocalResource pushed", "branch", toRepo.CurrentBranch(), "commitId", toLocalResourceCommitId)
	e.rec.Eventf(cr, nil, corev1.EventTypeNormal, "LocalResourcePushSuccess", "Push",
		fmt.Sprintf("Target LocalResource pushed branch %s", toRepo.CurrentBranch()))

	meta.SetExternalName(cr, toLocalResourceCommitId)
	cr.Status.TargetCommitId = toLocalResourceCommitId
	cr.Status.TargetBranch = toRepo.CurrentBranch()
	if err := e.updateStatusWithRetry(ctx, cr); err != nil {
		return fmt.Errorf("unable to update status: %w", err)
	}

	return nil
}

// updateStatusWithRetry persists the sync outcome, re-reading the object when the write loses an
// optimistic-concurrency race.
//
// A LOST CONFLICT USED TO BE PERMANENT, and that is the whole bug. This write happens AFTER the
// commit has already been pushed. The reconciler holds its own copy of the object, so any
// concurrent write — the owning composition re-applying, another controller touching metadata —
// makes the update fail with "the object has been modified; please apply your changes to the latest
// version". Returning that aborts the sync with the push already done, so the create bookkeeping
// never records success: `krateo.io/external-create-pending` stays set, `external-create-succeeded`
// is never written, and every later reconcile correctly refuses to proceed with "cannot determine
// creation result". The resource is then stuck until a human removes the annotation, even though
// the work it was asked to do completed.
//
// Observed four times on one cluster, always on the LATER indices of a multi-file publish — the
// ones whose pushes land inside the contention window created by their own siblings.
func (e *external) updateStatusWithRetry(ctx context.Context, cr *localResourcev1alpha1.LocalResource) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := e.kube.Status().Update(ctx, cr)
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
		// Refresh metadata (resourceVersion) from the server but KEEP the status this sync produced
		// and the external name just set, then let RetryOnConflict try the write again.
		latest := &localResourcev1alpha1.LocalResource{}
		if getErr := e.kube.Get(ctx, client.ObjectKeyFromObject(cr), latest); getErr != nil {
			return getErr
		}
		status := cr.Status
		externalName := meta.GetExternalName(cr)
		latest.DeepCopyInto(cr)
		cr.Status = status
		meta.SetExternalName(cr, externalName)
		return err
	})
}

// templatingOptions decides WHETHER to install a templating pass, and says so in one place so a
// reader does not have to reconstruct the rule from a switch buried in the sync path.
//
// Absent annotation (the default) means "template only if there is something to substitute".
// That is the safe reading: rendering is NOT a no-op on content that merely looks like a
// template, so a resource with no values to inject can only be damaged by a render pass.
// `{{- toYaml x | nindent 4 }}` and `{{ include "y" . }}` fail outright — both are Helm builtins,
// not sprig ones — and the quieter half is worse: `{{ .Values.n }}` parses, executes against an
// empty value set, and is committed as the literal string `<no value>`. This is what blocked a
// Helm chart publish (LocalResource publish-sock-shop-003). It mirrors the repo controller,
// which likewise installs a pass only when it has values.
//
// The annotation overrides that inference in either direction, because "no values" is evidence
// of intent, not a statement of it: a file can legitimately template with no inputs at all
// (`{{ now | date "2006-01-02" }}`, `{{ uuidv4 }}`, `{{ env "USER" }}`), and a file that does
// declare values can still contain regions that must survive verbatim.
func templatingOptions(annotations map[string]string, values []template.TemplateValue) ([]copier.Option, error) {
	left, right, err := templating.Delims(annotations)
	if err != nil {
		return nil, err
	}

	switch engine := annotations[templating.Annotation]; engine {
	case "":
		if len(values) > 0 {
			return []copier.Option{copier.WithGoTemplateDelims(values, left, right)}, nil
		}
		return nil, nil
	case templating.EngineGoTemplate:
		return []copier.Option{copier.WithGoTemplateDelims(values, left, right)}, nil
	case templating.EngineNone:
		return nil, nil
	default:
		// Refuse rather than guess. Silently picking a behaviour for a value nobody recognised
		// is how the mismatch this fixes went unnoticed in the first place.
		return nil, fmt.Errorf("unsupported %s %q: expected %q, %q, or the annotation to be absent",
			templating.Annotation, engine, templating.EngineGoTemplate, templating.EngineNone)
	}
}
