package localresource

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pkg/errors"

	commonv1 "github.com/krateoplatformops/provider-runtime/apis/common/v1"
	"github.com/krateoplatformops/provider-runtime/pkg/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	record "k8s.io/client-go/tools/events"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	plumbingevent "github.com/krateoplatformops/plumbing/kubeutil/event"
	"github.com/krateoplatformops/plumbing/kubeutil/eventrecorder"
	"github.com/krateoplatformops/provider-runtime/pkg/logging"
	"github.com/krateoplatformops/provider-runtime/pkg/meta"
	"github.com/krateoplatformops/provider-runtime/pkg/ratelimiter"

	localResourcev1alpha1 "github.com/krateoplatformops/git-provider/apis/localresource/v1alpha1"
	"github.com/krateoplatformops/git-provider/internal/clients/git"
	credentialhelper "github.com/krateoplatformops/git-provider/internal/controllers/common/credentialHelper"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/footer"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/option"
	"github.com/krateoplatformops/git-provider/internal/tools/copier"
	"github.com/krateoplatformops/git-provider/internal/tools/localfs"
	"github.com/krateoplatformops/git-provider/internal/tools/template"
	"github.com/krateoplatformops/plumbing/ptr"
	contexttools "github.com/krateoplatformops/provider-runtime/pkg/context"
	"github.com/krateoplatformops/provider-runtime/pkg/reconciler"

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

	username, err := resource.GetSecret(ctx, c.kube, cr.Spec.ToRepo.Credentials.UsernameRef)
	if err != nil {
		return nil, fmt.Errorf("retrieving .toRepo username: %w", err)
	}
	token, err := resource.GetSecret(ctx, c.kube, cr.Spec.ToRepo.Credentials.SecretRef)
	if err != nil {
		return nil, fmt.Errorf("retrieving .toRepo token: %w", err)
	}

	credOpts := credentialhelper.CredentialHelperOpts{
		AuthMethod: cr.Spec.ToRepo.Credentials.AuthMethod,
		Username:   username,
		Token:      token,
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
	return e.SyncLocalResources(ctx, cr, cr.Spec.CreateCommitMessage)
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
	return e.SyncLocalResources(ctx, cr, cr.Spec.UpdateCommitMessage)
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
	e.rec.Eventf(cr, nil, corev1.EventTypeNormal, "TargetLocalResourceCloned", "",
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

	// Render ONLY when the CR actually asked for substitution. This mirrors the repo
	// controller, which likewise installs a templating pass only when it has values.
	//
	// It is load-bearing, not a tidy-up. WithGoTemplate runs every file through
	// text/template + sprig, so a file is rendered whether or not there is anything to
	// substitute — and rendering is not a no-op on content that merely LOOKS like a
	// template. A Helm chart is the common case: `{{- toYaml $svc.command | nindent 12 }}`
	// fails outright, because toYaml is a Helm builtin and not a sprig one, and
	// `{{ .Values.replicas }}` is worse — it parses, executes against an empty value set,
	// and commits an empty string. That is a corrupted chart with no error anywhere.
	// A LocalResource with no placeholders is asking to copy bytes, so copy bytes.
	if len(values) > 0 {
		opts = append(opts, copier.WithGoTemplate(values))
	}

	co, err := copier.NewCopier(fromLocal, toRepo.FS(), opts...)
	if err != nil {
		return fmt.Errorf("unable to create copier: %w", err)
	}
	if err := co.Copy(override); err != nil {
		return fmt.Errorf("unable to copy files: %w", err)
	}

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

		err = e.kube.Status().Update(ctx, cr)
		if err != nil {
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
	e.rec.Eventf(cr, nil, corev1.EventTypeNormal, "LocalResourcePushSuccess", "",
		fmt.Sprintf("Target LocalResource pushed branch %s", toRepo.CurrentBranch()))

	meta.SetExternalName(cr, toLocalResourceCommitId)
	cr.Status.TargetCommitId = toLocalResourceCommitId
	cr.Status.TargetBranch = toRepo.CurrentBranch()
	err = e.kube.Status().Update(ctx, cr)
	if err != nil {
		return fmt.Errorf("unable to update status: %w", err)
	}

	return nil
}
