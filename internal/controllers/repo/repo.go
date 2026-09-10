package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	commonv1 "github.com/krateoplatformops/provider-runtime/apis/common/v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateoplatformops/provider-runtime/pkg/logging"
	"github.com/krateoplatformops/provider-runtime/pkg/meta"
	"github.com/krateoplatformops/provider-runtime/pkg/ratelimiter"

	"github.com/krateoplatformops/provider-runtime/pkg/reconciler"
	"github.com/krateoplatformops/provider-runtime/pkg/resource"

	repov1alpha1 "github.com/krateoplatformops/git-provider/apis/repo/v1alpha1"
	"github.com/krateoplatformops/git-provider/internal/clients/git"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/option"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/templating"
	"github.com/krateoplatformops/git-provider/internal/tools/copier"
	"github.com/krateoplatformops/git-provider/internal/tools/template"
	plumbingevent "github.com/krateoplatformops/plumbing/kubeutil/event"
	"github.com/krateoplatformops/plumbing/kubeutil/eventrecorder"
	"github.com/krateoplatformops/plumbing/ptr"
	record "k8s.io/client-go/tools/events"
)

var (
	errNotRepo = fmt.Errorf("managed resource is not a repo custom resource")
	homeDir    string
)

// Deprecated: prefer templating.Annotation. Kept as an alias so existing references keep
// compiling; the string itself now has a single definition, shared with the localresource
// controller so the two cannot drift.
const AnnotationTemplatingEngine = templating.Annotation

// Setup adds a controller that reconciles Token managed resources.
func Setup(mgr ctrl.Manager, o option.SetupOptions) error {
	name := reconciler.ControllerName(repov1alpha1.RepoGroupKind)

	log := o.Controller.Logger.WithValues("controller", name)

	recorder, err := eventrecorder.Create(context.Background(), mgr.GetConfig(), name, nil)
	if err != nil {
		return fmt.Errorf("failed to create event recorder: %w", err)
	}

	r := reconciler.NewReconciler(mgr,
		resource.ManagedKind(repov1alpha1.RepoGroupVersionKind),
		reconciler.WithExternalConnecter(&connector{
			kube:     mgr.GetClient(),
			log:      log,
			recorder: recorder,
		}),
		reconciler.WithPollInterval(o.Controller.PollInterval),
		reconciler.WithLogger(log),
		reconciler.WithRecorder(plumbingevent.NewAPIRecorder(recorder)),
		reconciler.WithTimeout(o.Controller.Timeout),
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.Controller.ForControllerRuntime()).
		For(&repov1alpha1.Repo{}).
		Complete(ratelimiter.New(name, r, o.Controller.GlobalRateLimiter))
}

type connector struct {
	kube     client.Client
	log      logging.Logger
	recorder record.EventRecorder
}

func (c *connector) Connect(ctx context.Context, mg resource.Managed) (reconciler.ExternalClient, error) {
	cr, ok := mg.(*repov1alpha1.Repo)
	if !ok {
		return nil, errNotRepo
	}

	cfg, err := loadExternalClientOpts(ctx, c.kube, cr)
	if err != nil {
		return nil, err
	}

	homeDir, err = os.UserHomeDir()
	if err != nil {
		homeDir = "/tmp"
	}

	log := c.log.WithValues("name", cr.Name, "namespace", cr.Namespace)

	rec := plumbingevent.NewAPIRecorder(c.recorder)
	if rec == nil {
		return nil, fmt.Errorf("failed to create event recorder")
	}

	return &external{
		kube: c.kube,
		log:  log,
		cfg:  cfg,
		rec:  *rec,
	}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	kube client.Client
	log  logging.Logger
	cfg  *externalClientOpts
	rec  plumbingevent.APIRecorder
}

func (e *external) Observe(ctx context.Context, mg resource.Managed) (reconciler.ExternalObservation, error) {
	cr, ok := mg.(*repov1alpha1.Repo)
	if !ok {
		return reconciler.ExternalObservation{}, errNotRepo
	}
	e.log.Info("Observing resource")

	if cr.GetCondition(commonv1.TypeReady).Reason == commonv1.ReasonDeleting {
		return reconciler.ExternalObservation{
			ResourceExists:   false,
			ResourceUpToDate: true,
		}, nil
	}

	if !cr.DeletionTimestamp.IsZero() && cr.GetCondition(commonv1.TypeSynced).Reason == commonv1.ReasonReconcileError {
		if !meta.IsActionAllowed(cr, meta.ActionDelete) {
			e.log.Debug("External resource should not be deleted by provider, skip deleting.")
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

	if meta.GetExternalName(cr) == "" {
		return reconciler.ExternalObservation{
			ResourceExists:   false,
			ResourceUpToDate: true,
		}, nil
	}
	latestCommit, err := git.GetLatestCommitRemote(git.ListOptions{
		URL:        cr.Spec.FromRepo.Url,
		Auth:       e.cfg.FromRepoCreds,
		Insecure:   e.cfg.Insecure,
		Branch:     cr.Spec.FromRepo.Branch,
		GitCookies: e.cfg.FromRepoCookieFile,
		HomeDir:    homeDir, // Use the configured home directory for temporary files
	})

	if err != nil {
		e.log.Debug("Unable to get latest commit from origin remote repository", "msg", err.Error())
		return reconciler.ExternalObservation{}, err
	}

	isTargetRepoSynced, err := git.IsInGitCommitHistoryContext(ctx, git.ListOptions{
		URL:        cr.Spec.ToRepo.Url,
		Auth:       e.cfg.ToRepoCreds,
		Insecure:   e.cfg.Insecure,
		Branch:     cr.Spec.ToRepo.Branch,
		GitCookies: e.cfg.ToRepoCookieFile,
		HomeDir:    homeDir, // Use the configured home directory for temporary files
	}, cr.Status.TargetCommitId)
	if err != nil {
		e.log.Debug("Unable to check if target repo is synced", "msg", err.Error())
		return reconciler.ExternalObservation{}, err
	}

	if !isTargetRepoSynced {
		e.log.Debug("Target commit not found in target remote repository", "commitId", cr.Status.TargetCommitId, "branch", cr.Status.TargetBranch)
		if !cr.Spec.EnableUpdate {
			return reconciler.ExternalObservation{}, e.failSync(ctx, cr, fmt.Errorf("target commit %s not found on branch %s while enableUpdate is false", cr.Status.TargetCommitId, cr.Status.TargetBranch))
		}
		return reconciler.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	if ptr.Deref(latestCommit, "") != cr.Status.OriginCommitId {
		e.log.Debug("Origin commit is no longer the latest commit on origin repository", "commitId", cr.Status.OriginCommitId, "branch", cr.Status.OriginBranch)
		if cr.Spec.EnableUpdate {
			return reconciler.ExternalObservation{
				ResourceExists:   true,
				ResourceUpToDate: false,
			}, nil
		}
		e.log.Debug("Origin commit changed but enableUpdate is false, keeping the target repository unchanged")
	}

	cr.Status.SetConditions(commonv1.Available())

	return reconciler.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: true,
	}, nil
}

func (e *external) Create(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*repov1alpha1.Repo)
	if !ok {
		return errNotRepo
	}
	if !meta.IsActionAllowed(cr, meta.ActionCreate) {
		e.log.Debug("External resource should not be created by provider, skip creating.")
		return nil
	}
	e.log.Info("Creating resource")
	cr.Status.SetConditions(commonv1.Creating())
	return e.SyncRepos(ctx, cr, "first commit")
}

func (e *external) Update(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*repov1alpha1.Repo)
	if !ok {
		return errNotRepo
	}

	if !cr.Spec.EnableUpdate {
		e.log.Debug("External resource should not be updated by provider, skip updating.")
		return nil
	}
	if !meta.IsActionAllowed(cr, meta.ActionUpdate) {
		e.log.Debug("External resource should not be updated by provider, skip updating.")
		return nil
	}

	e.log.Info("Updating resource")
	cr.Status.SetConditions(commonv1.Creating())
	return e.SyncRepos(ctx, cr, "updated target repo with origin repo")
}

func (e *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*repov1alpha1.Repo)
	if !ok {
		return errNotRepo
	}
	if !meta.IsActionAllowed(cr, meta.ActionDelete) {
		e.log.Debug("External resource should not be deleted by provider, skip deleting.")
		return nil
	}

	e.log.Info("Deleting resource")

	cr.Status.SetConditions(commonv1.Deleting())

	return nil // noop
}

func (e *external) loadValuesFromConfigMap(ctx context.Context, ref *commonv1.ConfigMapKeySelector) (map[string]interface{}, error) {
	var res map[string]interface{}

	js, err := resource.GetConfigMapValue(ctx, e.kube, ref)
	if err != nil {
		e.log.Warn(err.Error(), "name", ref.Name, "key", ref.Key, "namespace", ref.Namespace)
		return nil, err
	}

	js = strings.TrimPrefix(js, "'")
	js = strings.TrimSuffix(js, "'")

	err = json.Unmarshal([]byte(js), &res)
	if err != nil {
		e.log.Warn(err.Error(), "json", js)
		return nil, err
	}

	return res, nil
}

func (e *external) failSync(ctx context.Context, cr *repov1alpha1.Repo, err error) error {
	if err == nil {
		return nil
	}

	cr.Status.SetConditions(commonv1.Unavailable(), commonv1.ReconcileError(err))
	return err
}

func (e *external) SyncRepos(ctx context.Context, cr *repov1alpha1.Repo, commitMessage string) error {

	spec := cr.Spec.DeepCopy()

	var altBranch *string
	if len(spec.ToRepo.CloneFromBranch) > 0 {
		altBranch = ptr.To(spec.ToRepo.CloneFromBranch)
	}

	toRepo, err := git.Clone(git.CloneOptions{
		URL:                     spec.ToRepo.Url,
		Auth:                    e.cfg.ToRepoCreds,
		Insecure:                e.cfg.Insecure,
		UnsupportedCapabilities: e.cfg.UnsupportedCapabilities,
		Branch:                  spec.ToRepo.Branch,
		AlternativeBranch:       altBranch,
		GitCookies:              e.cfg.ToRepoCookieFile,
		HomeDir:                 homeDir, // Use the configured home directory for temporary files
	})
	if err != nil {
		return e.failSync(ctx, cr, fmt.Errorf("cloning toRepo: %w", err))
	}
	defer toRepo.Cleanup()

	e.log.Debug("Target repo cloned", "url", spec.ToRepo.Url)
	e.rec.Event(cr, plumbingevent.Normal("TargetRepoCloned", "Reconciling", fmt.Sprintf("Successfully cloned target repo: %s", spec.ToRepo.Url)))
	e.log.Debug(fmt.Sprintf("Target repo on branch %s", toRepo.CurrentBranch()))

	fromRepo, err := git.Clone(git.CloneOptions{
		URL:                     spec.FromRepo.Url,
		Auth:                    e.cfg.FromRepoCreds,
		Insecure:                e.cfg.Insecure,
		UnsupportedCapabilities: e.cfg.UnsupportedCapabilities,
		Branch:                  spec.FromRepo.Branch,
		GitCookies:              e.cfg.FromRepoCookieFile,
		HomeDir:                 homeDir, // Use the configured home directory for temporary files
	})
	if err != nil {
		return e.failSync(ctx, cr, fmt.Errorf("cloning fromRepo: %w", err))
	}
	defer fromRepo.Cleanup()
	e.log.Debug("Origin repo cloned", "url", spec.FromRepo.Url)
	e.rec.Event(cr, plumbingevent.Normal("OriginRepoCloned", "Reconciling", fmt.Sprintf("Successfully cloned origin repo: %s", spec.FromRepo.Url)))
	e.log.Debug(fmt.Sprintf("Origin repo on branch %s", fromRepo.CurrentBranch()))

	fromRepoCommitId, err := fromRepo.GetLatestCommit(fromRepo.CurrentBranch())
	if err != nil {
		return e.failSync(ctx, cr, err)
	}

	fromPath := spec.FromRepo.Path
	toPath := spec.ToRepo.Path
	override := spec.Override
	if len(toPath) == 0 {
		toPath = "/"
	}
	if len(fromPath) == 0 {
		fromPath = "/"
	}
	var values map[string]interface{}
	if spec.ConfigMapKeyRef != nil {
		values, err = e.loadValuesFromConfigMap(ctx, spec.ConfigMapKeyRef)
		if err != nil {
			e.log.Warn("Unable to load configmap with template data", "msg", err.Error())
			e.rec.Event(cr, plumbingevent.Warning("CannotLoadConfigMap", "Reconciling", fmt.Errorf("Unable to load configmap with template data: %s", err.Error())))
		}

		e.log.Debug("Loaded values from config map",
			"name", spec.ConfigMapKeyRef.Name,
			"key", spec.ConfigMapKeyRef.Key,
			"namespace", spec.ConfigMapKeyRef.Namespace,
			"values", values,
		)
	}
	opts := []copier.Option{
		copier.WithOriginCopyPath(fromPath),
		copier.WithTargetCopyPath(toPath),
		copier.WithIgnorePath(spec.FromRepo.KrateoIgnorePath),
	}

	if values != nil {
		engine := cr.GetAnnotations()[AnnotationTemplatingEngine]
		if engine == "gotemplate" {
			tplVals := make([]template.TemplateValue, 0, len(values))
			for k, v := range values {
				tplVals = append(tplVals, template.TemplateValue{
					Key:   k,
					Value: fmt.Sprintf("%v", v),
				})
			}
			left, right, derr := templating.Delims(cr.GetAnnotations())
			if derr != nil {
				return e.failSync(ctx, cr, derr)
			}
			opts = append(opts, copier.WithGoTemplateDelims(tplVals, left, right))
		} else {
			opts = append(opts, copier.WithMustacheTemplate(values))
		}
	}

	co, err := copier.NewCopier(fromRepo.FS(), toRepo.FS(), opts...)
	if err != nil {
		return e.failSync(ctx, cr, fmt.Errorf("unable to create copier: %w", err))
	}
	if override {
		e.log.Debug("Override is true, overriding all files in target repo")
		if fromPath == "/" && toPath == "/" {
			e.rec.Event(cr, plumbingevent.Warning("OverrideWarning", "Reconciling", fmt.Errorf("Override is set to true, but originPath and targetPath are both set to '/', this will override also service folders like .git, .github, .gitignore, etc. Consider using a different path for originPath or targetPath. This can broke the target repository causing the impossibility to push changes.")))
			e.log.Info("Override is set to true, but originPath and targetPath are both set to '/', this will override also service folders like .git, .github, .gitignore, etc. Consider using a different path for originPath or targetPath. This can broke the target repository causing the impossibility to push changes.")
		}
	}
	if err := co.Copy(override); err != nil {
		var failed *copier.RenderFailed
		if errors.As(err, &failed) {
			cr.Status.TemplatingErrors = templating.StatusErrors(failed.Errors)
		}
		return e.failSync(ctx, cr, fmt.Errorf("unable to copy files: %w", err))
	}
	cr.Status.TemplatingErrors = nil

	e.log.Info("Origin and target repo synchronized",
		"fromUrl", spec.FromRepo.Url,
		"toUrl", spec.ToRepo.Url,
		"fromPath", fromPath,
		"toPath", toPath)
	e.rec.Event(cr, plumbingevent.Normal("RepoSyncSuccess", "Reconciling", "Origin and target repo synchronized"))

	toRepoCommitIdObj, err := toRepo.Commit(".", commitMessage, &git.IndexOptions{
		OriginRepo: fromRepo,
		FromPath:   fromPath,
		ToPath:     toPath,
	})
	if err == git.NoErrAlreadyUpToDate {
		toRepoCommitId, err := toRepo.GetLatestCommit(toRepo.CurrentBranch())
		if err != nil {
			return e.failSync(ctx, cr, fmt.Errorf("unable to get latest commit from target repo: %w", err))
		}
		e.log.Info("Target repo not commited", "branch", toRepo.CurrentBranch(), "status", "repository already up-to-date")
		e.rec.Event(cr, plumbingevent.Normal("RepoAlreadyUpToDate", "Reconciling", fmt.Sprintf("Target repo already up-to-date on branch %s", toRepo.CurrentBranch())))

		meta.SetExternalName(cr, toRepoCommitId)
		cr.Status.OriginCommitId = fromRepoCommitId
		cr.Status.TargetCommitId = toRepoCommitId
		cr.Status.TargetBranch = toRepo.CurrentBranch()
		cr.Status.OriginBranch = fromRepo.CurrentBranch()

		err = e.kube.Status().Update(ctx, cr)
		if err != nil {
			return e.failSync(ctx, cr, fmt.Errorf("unable to update status: %w", err))
		}
		return nil
	} else if err != nil {
		return e.failSync(ctx, cr, fmt.Errorf("unable to commit target repo: %w", err))
	}
	toRepoCommitId := toRepoCommitIdObj.String()

	e.log.Info("Target repo committed", "branch", toRepo.CurrentBranch(), "commitId", toRepoCommitId)
	e.rec.Event(cr, plumbingevent.Normal("RepoCommitSuccess", "Reconciling", fmt.Sprintf("Target repo committed on branch %s", toRepo.CurrentBranch())))

	err = toRepo.Push("origin", toRepo.CurrentBranch(), e.cfg.Insecure)
	if err != nil {
		return e.failSync(ctx, cr, fmt.Errorf("unable to push target repo: %w", err))
	}
	e.log.Info("Target repo pushed", "branch", toRepo.CurrentBranch(), "commitId", toRepoCommitId)
	e.rec.Event(cr, plumbingevent.Normal("RepoPushSuccess", "Reconciling", fmt.Sprintf("Target repo pushed branch %s", toRepo.CurrentBranch())))

	meta.SetExternalName(cr, toRepoCommitId)
	cr.Status.OriginCommitId = fromRepoCommitId
	cr.Status.TargetCommitId = toRepoCommitId
	cr.Status.TargetBranch = toRepo.CurrentBranch()
	cr.Status.OriginBranch = fromRepo.CurrentBranch()
	err = e.kube.Status().Update(ctx, cr)
	if err != nil {
		return e.failSync(ctx, cr, fmt.Errorf("unable to update status: %w", err))
	}
	return nil
}
