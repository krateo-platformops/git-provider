//go:build integration
// +build integration

package repo

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"

	"github.com/krateoplatformops/git-provider/apis"
	repov1alpha1 "github.com/krateoplatformops/git-provider/apis/repo/v1alpha1"
	gitclient "github.com/krateoplatformops/git-provider/internal/clients/git"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/option"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/templating"
	prettylog "github.com/krateoplatformops/plumbing/slogs/pretty"
	commonv1 "github.com/krateoplatformops/provider-runtime/apis/common/v1"
	"github.com/krateoplatformops/provider-runtime/pkg/controller"
	"github.com/krateoplatformops/provider-runtime/pkg/logging"
	"github.com/krateoplatformops/provider-runtime/pkg/ratelimiter"

	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	xenv "github.com/krateoplatformops/plumbing/env"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"
)

var (
	testenv     env.Environment
	clusterName string
)

const (
	crdPath              = "../../../crds"
	namespace            = "test-system"
	giteaBaseURL         = "https://127.0.0.1:8443"
	gitAuthSecretName    = "git-creds"
	gitBadAuthSecretName = "git-creds-bad"
	repoContentPath      = "/content"
)

var (
	giteaUsername = "admin"
	giteaPassword = "admin123"
)

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	clusterName = "krateo-repo-provider-controller"
	testenv = env.New()
	kindCluster := kind.NewCluster(clusterName)

	_ = apiextensionsv1.AddToScheme(clientsetscheme.Scheme)
	_ = apis.AddToScheme(clientsetscheme.Scheme)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}
	defer cli.Close()

	var containerID string

	testenv.Setup(
		envfuncs.CreateCluster(kindCluster, clusterName),
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				return ctx, err
			}
			return ctx, r.Create(ctx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
		},
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			tmpdir, err := os.MkdirTemp(os.TempDir(), "repo-test-gitea-*")
			if err != nil {
				return ctx, err
			}

			imageName := "gitea/gitea:latest"
			reader, err := cli.ImagePull(ctx, imageName, client.ImagePullOptions{})
			if err != nil {
				return ctx, err
			}
			defer reader.Close()
			_, _ = io.Copy(os.Stdout, reader)

			httpPort, _ := network.ParsePort("3000/tcp")
			sshPort, _ := network.ParsePort("22/tcp")
			httpsPort, _ := network.ParsePort("443/tcp")

			portBinding := network.PortMap{
				httpPort:  []network.PortBinding{{HostIP: netip.AddrFrom4([4]byte{127, 0, 0, 1}), HostPort: "3000"}},
				sshPort:   []network.PortBinding{{HostIP: netip.AddrFrom4([4]byte{127, 0, 0, 1}), HostPort: "2222"}},
				httpsPort: []network.PortBinding{{HostIP: netip.AddrFrom4([4]byte{127, 0, 0, 1}), HostPort: "8443"}},
			}

			containerConfig := &container.Config{
				Image: imageName,
				ExposedPorts: network.PortSet{
					httpPort:  struct{}{},
					sshPort:   struct{}{},
					httpsPort: struct{}{},
				},
				Env: []string{
					"GITEA__database__DB_TYPE=sqlite3",
					"GITEA__security__INSTALL_LOCK=true",
					"USER_UID=1000",
					"USER_GID=1000",
					"GITEA__server__DOMAIN=127.0.0.1",
					"GITEA__server__HTTP_PORT=443",
					fmt.Sprintf("GITEA__server__ROOT_URL=%s", giteaBaseURL),
					"GITEA__server__PROTOCOL=https",
					"GITEA__server__CERT_FILE=/data/cert.pem",
					"GITEA__server__KEY_FILE=/data/key.pem",
				},
				Entrypoint: []string{"/bin/sh", "-c"},
				Cmd: []string{
					fmt.Sprintf(`
			if [ ! -f /data/cert.pem ]; then
				cd /data && /usr/local/bin/gitea cert --host localhost,127.0.0.1 --ca
			fi
			chown -R 1000:1000 /data
			echo 'su-exec git /usr/local/bin/gitea migrate' >> /etc/s6/gitea/setup
			echo 'su-exec git /usr/local/bin/gitea admin user create --username %s --password %s --email admin@local --admin --must-change-password=false' >> /etc/s6/gitea/setup
			/usr/bin/entrypoint /usr/bin/s6-svscan /etc/s6
		`, giteaUsername, giteaPassword),
				},
			}

			hostConfig := &container.HostConfig{
				PortBindings: portBinding,
				RestartPolicy: container.RestartPolicy{
					Name: "always",
				},
				Binds: []string{
					fmt.Sprintf("%s:/data", tmpdir),
				},
			}

			resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config:           containerConfig,
				HostConfig:       hostConfig,
				NetworkingConfig: &network.NetworkingConfig{},
				Name:             "gitea-repo-test",
			})
			if err != nil {
				return ctx, err
			}

			go func() {
				logsReader, err := cli.ContainerLogs(ctx, resp.ID, client.ContainerLogsOptions{
					ShowStdout: true,
					ShowStderr: true,
					Follow:     true,
				})
				if err != nil {
					return
				}
				defer logsReader.Close()
				_, _ = io.Copy(os.Stdout, logsReader)
			}()

			_, err = cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{})
			if err != nil {
				return ctx, err
			}
			containerID = resp.ID

			return ctx, waitForGitea(ctx)
		},
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				return ctx, err
			}
			r.WithNamespace(namespace)

			err = decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(crdPath)), "*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
			)
			return ctx, err
		},
	)

	testenv.Finish(
		envfuncs.DestroyCluster(clusterName),
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			if containerID != "" {
				_, _ = cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{})
				_, _ = cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
			}
			return ctx, nil
		},
	)

	os.Exit(testenv.Run(m))
}

func waitForGitea(ctx context.Context) error {
	httpClient := newInsecureHTTPClient()
	for i := 0; i < 30; i++ {
		resp, err := httpClient.Get(giteaBaseURL + "/api/v1/swagger")
		if err == nil && resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("gitea not ready")
}

func setupController(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	lh := prettylog.New(&slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: false,
	},
		prettylog.WithDestinationWriter(os.Stderr),
		prettylog.WithColor(),
		prettylog.WithOutputEmptyAttrs(),
	)

	logrlog := logr.FromSlogHandler(slog.New(lh).Handler())
	log := logging.NewLogrLogger(logrlog)

	ctrl.SetLogger(logrlog)

	mgr, err := ctrl.NewManager(cfg.Client().RESTConfig(), ctrl.Options{
		Metrics: server.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		return ctx, err
	}

	o := controller.Options{
		Logger:                  log,
		MaxConcurrentReconciles: 1,
		PollInterval:            2 * time.Second,
		GlobalRateLimiter:       ratelimiter.NewGlobalExponential(1*time.Second, 1*time.Minute),
	}

	tmpdir, _ := os.MkdirTemp(os.TempDir(), "repo-test-home-*")
	if err := Setup(mgr, option.SetupOptions{
		Controller: option.ControllerOptions{
			Options: o,
			Timeout: 3 * time.Minute,
		},
		Git: option.GitOptions{
			CommitAuthorName:  "test-author",
			CommitAuthorEmail: "test@email.com",
			HomeDir:           tmpdir,
		},
	}); err != nil {
		return ctx, err
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(err)
		}
	}()
	return ctx, nil
}

func newInsecureHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}
}

func createGiteaRepo(t *testing.T, name, defaultBranch string) {
	t.Helper()

	payload := map[string]any{
		"name":        name,
		"description": "integration test repository",
		"private":     false,
		"auto_init":   true,
		"readme":      "Default",
	}
	if defaultBranch != "" {
		payload["default_branch"] = defaultBranch
	}

	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, giteaBaseURL+"/api/v1/user/repos", strings.NewReader(string(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(giteaUsername, giteaPassword)

	resp, err := newInsecureHTTPClient().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.True(t, resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusConflict, "unexpected status creating repo %s: %s", name, resp.Status)
}

func deleteGiteaRepo(t *testing.T, name string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodDelete, giteaBaseURL+"/api/v1/repos/"+giteaUsername+"/"+name, nil)
	require.NoError(t, err)
	req.SetBasicAuth(giteaUsername, giteaPassword)

	resp, err := newInsecureHTTPClient().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.True(t, resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound, "unexpected status deleting repo %s: %s", name, resp.Status)
}

func cloneGiteaRepo(t *testing.T, repoName, branch string) *gitclient.Repo {
	t.Helper()

	repo, err := gitclient.Clone(gitclient.CloneOptions{
		URL:      giteaRepoURL(repoName),
		Auth:     gitHTTPAuth(),
		Insecure: true,
		Branch:   branch,
		HomeDir:  os.TempDir(),
	})
	require.NoError(t, err)
	return repo
}

func gitHTTPAuth() *githttp.BasicAuth {
	return &githttp.BasicAuth{
		Username: giteaUsername,
		Password: giteaPassword,
	}
}

func giteaRepoURL(name string) string {
	return fmt.Sprintf("%s/%s/%s.git", giteaBaseURL, giteaUsername, name)
}

func writeRepoFile(t *testing.T, fs billy.Filesystem, path, content string) {
	t.Helper()

	cleanPath := strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	dir := filepath.Dir(cleanPath)
	if dir != "." {
		require.NoError(t, fs.MkdirAll(dir, 0o755))
	}

	f, err := fs.OpenFile(cleanPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	defer f.Close()

	_, err = f.Write([]byte(content))
	require.NoError(t, err)
}

func readRepoFile(t *testing.T, fs billy.Filesystem, path string) string {
	t.Helper()

	cleanPath := strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	f, err := fs.Open(cleanPath)
	require.NoError(t, err)
	defer f.Close()

	bs, err := io.ReadAll(f)
	require.NoError(t, err)
	return string(bs)
}

func commitFilesToRepo(t *testing.T, repoName, branch, message string, files map[string]string) string {
	t.Helper()

	repo := cloneGiteaRepo(t, repoName, branch)
	defer repo.Cleanup()

	for path, content := range files {
		writeRepoFile(t, repo.FS(), path, content)
	}

	_, err := repo.Commit(".", message, &gitclient.IndexOptions{
		OriginRepo: repo,
		FromPath:   "/",
		ToPath:     "/",
	})
	require.NoError(t, err)
	require.NoError(t, repo.Push("origin", branch, true))

	commitID, err := repo.GetLatestCommit(repo.CurrentBranch())
	require.NoError(t, err)
	return commitID
}

func readRemoteFile(t *testing.T, repoName, branch, path string) string {
	t.Helper()

	repo := cloneGiteaRepo(t, repoName, branch)
	defer repo.Cleanup()

	return readRepoFile(t, repo.FS(), path)
}

func assertRemoteFileAbsent(t *testing.T, repoName, branch, path string) {
	t.Helper()

	repo := cloneGiteaRepo(t, repoName, branch)
	defer repo.Cleanup()

	cleanPath := strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	_, err := repo.FS().Stat(cleanPath)
	require.True(t, os.IsNotExist(err), "expected %s to be absent in %s/%s, got err=%v", path, repoName, branch, err)
}

func latestRemoteCommit(t *testing.T, repoName, branch string) string {
	t.Helper()

	commitID, err := gitclient.GetLatestCommitRemote(gitclient.ListOptions{
		URL:      giteaRepoURL(repoName),
		Auth:     gitHTTPAuth(),
		Insecure: true,
		Branch:   branch,
		HomeDir:  os.TempDir(),
	})
	require.NoError(t, err)
	require.NotNil(t, commitID)
	return *commitID
}

func waitForRepo(ctx context.Context, r *resources.Resources, name string, timeout time.Duration, predicate func(*repov1alpha1.Repo) bool) (*repov1alpha1.Repo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current := &repov1alpha1.Repo{}
		err := r.Get(ctx, name, namespace, current)
		if err == nil && predicate(current) {
			return current, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("timeout waiting for repo %s", name)
}

func waitForRepoCondition(ctx context.Context, r *resources.Resources, name string, ctype commonv1.ConditionType, status metav1.ConditionStatus, timeout time.Duration) (*repov1alpha1.Repo, error) {
	return waitForRepo(ctx, r, name, timeout, func(repo *repov1alpha1.Repo) bool {
		return repo.GetCondition(ctype).Status == status
	})
}

func secretSelector(secretName, key string) *commonv1.SecretKeySelector {
	return &commonv1.SecretKeySelector{
		Key: key,
		Reference: commonv1.Reference{
			Name:      secretName,
			Namespace: namespace,
		},
	}
}

func configMapSelector(name, key string) *commonv1.ConfigMapKeySelector {
	return &commonv1.ConfigMapKeySelector{
		Key: key,
		Reference: commonv1.Reference{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func newRepoResource(name, fromRepo, fromBranch, toRepo, toBranch string) *repov1alpha1.Repo {
	return &repov1alpha1.Repo{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: repov1alpha1.RepoSpec{
			FromRepo: repov1alpha1.FromRepoOpts{
				KrateoIgnorePath: repoContentPath,
				RepoOpts: repov1alpha1.RepoOpts{
					Url:         giteaRepoURL(fromRepo),
					Path:        repoContentPath,
					Branch:      fromBranch,
					SecretRef:   secretSelector(gitAuthSecretName, "token"),
					UsernameRef: secretSelector(gitAuthSecretName, "username"),
				},
			},
			ToRepo: repov1alpha1.RepoOpts{
				Url:         giteaRepoURL(toRepo),
				Path:        repoContentPath,
				Branch:      toBranch,
				SecretRef:   secretSelector(gitAuthSecretName, "token"),
				UsernameRef: secretSelector(gitAuthSecretName, "username"),
			},
			Insecure: true,
		},
	}
}

func TestController(t *testing.T) {
	type testCase struct {
		name   string
		setup  func(ctx context.Context, t *testing.T, r *resources.Resources)
		repo   *repov1alpha1.Repo
		verify func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string)
	}

	cases := []testCase{
		{
			name: "TC01-InitialSync",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc01", "main")
				createGiteaRepo(t, "dst-tc01", "main")
				commitFilesToRepo(t, "src-tc01", "main", "seed source", map[string]string{
					"content/README.md":        "initial sync\n",
					"content/configs/app.yaml": "enabled: true\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc01-sync", "src-tc01", "main", "dst-tc01", "main")
				repo.Spec.Override = true
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				current, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)
				require.NotEmpty(t, current.Status.OriginCommitId)
				require.NotEmpty(t, current.Status.TargetCommitId)
				require.Equal(t, "main", current.Status.OriginBranch)
				require.Equal(t, "main", current.Status.TargetBranch)
				require.Equal(t, current.Status.OriginCommitId, latestRemoteCommit(t, "src-tc01", "main"))
				require.Equal(t, current.Status.TargetCommitId, latestRemoteCommit(t, "dst-tc01", "main"))
				require.Equal(t, "initial sync\n", readRemoteFile(t, "dst-tc01", "main", "content/README.md"))
				require.Equal(t, "enabled: true\n", readRemoteFile(t, "dst-tc01", "main", "content/configs/app.yaml"))
			},
		},
		{
			name: "TC02-Idempotency",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc02", "main")
				createGiteaRepo(t, "dst-tc02", "main")
				commitFilesToRepo(t, "src-tc02", "main", "seed source", map[string]string{
					"content/idempotent.txt": "one pass only\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc02-idempotency", "src-tc02", "main", "dst-tc02", "main")
				repo.Spec.Override = true
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				current, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)

				firstOrigin := current.Status.OriginCommitId
				firstTarget := current.Status.TargetCommitId
				firstRemoteTarget := latestRemoteCommit(t, "dst-tc02", "main")

				time.Sleep(6 * time.Second)

				later, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 30*time.Second)
				require.NoError(t, err)
				require.Equal(t, firstOrigin, later.Status.OriginCommitId)
				require.Equal(t, firstTarget, later.Status.TargetCommitId)
				require.Equal(t, firstRemoteTarget, latestRemoteCommit(t, "dst-tc02", "main"))
			},
		},
		{
			name: "TC03-ContentUpdate",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc03", "main")
				createGiteaRepo(t, "dst-tc03", "main")
				commitFilesToRepo(t, "src-tc03", "main", "seed source", map[string]string{
					"content/app.txt": "v1\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc03-update", "src-tc03", "main", "dst-tc03", "main")
				repo.Spec.EnableUpdate = true
				repo.Spec.Override = true
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				current, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)

				initialOrigin := current.Status.OriginCommitId
				initialTarget := current.Status.TargetCommitId

				updatedOrigin := commitFilesToRepo(t, "src-tc03", "main", "update source", map[string]string{
					"content/app.txt":     "v2\n",
					"content/feature.txt": "new content\n",
				})

				updated, err := waitForRepo(ctx, r, repoName, 2*time.Minute, func(repo *repov1alpha1.Repo) bool {
					return repo.GetCondition(commonv1.TypeReady).Status == metav1.ConditionTrue &&
						repo.Status.OriginCommitId != "" &&
						repo.Status.TargetCommitId != "" &&
						repo.Status.OriginCommitId != initialOrigin &&
						repo.Status.TargetCommitId != initialTarget
				})
				require.NoError(t, err)
				require.Equal(t, updatedOrigin, updated.Status.OriginCommitId)
				require.Equal(t, "v2\n", readRemoteFile(t, "dst-tc03", "main", "content/app.txt"))
				require.Equal(t, "new content\n", readRemoteFile(t, "dst-tc03", "main", "content/feature.txt"))
			},
		},
		{
			name: "TC04-FallbackBranch",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc04", "main")
				createGiteaRepo(t, "dst-tc04", "main")
				commitFilesToRepo(t, "src-tc04", "main", "seed source", map[string]string{
					"content/from-source.txt": "copied through fallback\n",
				})
				commitFilesToRepo(t, "dst-tc04", "master", "seed master", map[string]string{
					"content/bootstrap.txt": "master baseline\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc04-fallback", "src-tc04", "main", "dst-tc04", "release")
				repo.Spec.ToRepo.CloneFromBranch = "master"
				repo.Spec.Override = true
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				current, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)
				require.Equal(t, "release", current.Status.TargetBranch)
				require.Equal(t, "copied through fallback\n", readRemoteFile(t, "dst-tc04", "release", "content/from-source.txt"))
			},
		},
		{
			name: "TC05-AuthFailure",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc05", "main")
				createGiteaRepo(t, "dst-tc05", "main")
				commitFilesToRepo(t, "src-tc05", "main", "seed source", map[string]string{
					"content/auth.txt": "should never sync\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc05-authfail", "src-tc05", "main", "dst-tc05", "main")
				repo.Spec.FromRepo.SecretRef = secretSelector(gitBadAuthSecretName, "token")
				repo.Spec.FromRepo.UsernameRef = secretSelector(gitBadAuthSecretName, "username")
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				current, err := waitForRepo(ctx, r, repoName, 90*time.Second, func(repo *repov1alpha1.Repo) bool {
					cond := repo.GetCondition(commonv1.TypeSynced)
					return cond.Status == metav1.ConditionFalse && cond.Reason == commonv1.ReasonReconcileError
				})
				require.NoError(t, err)
				require.Equal(t, commonv1.ReasonReconcileError, current.GetCondition(commonv1.TypeSynced).Reason)
			},
		},
		{
			name: "TC06-OverrideFalse",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc06", "main")
				createGiteaRepo(t, "dst-tc06", "main")
				commitFilesToRepo(t, "src-tc06", "main", "seed source", map[string]string{
					"content/existing.txt": "source version\n",
					"content/new.txt":      "fresh file\n",
				})
				commitFilesToRepo(t, "dst-tc06", "main", "seed target", map[string]string{
					"content/existing.txt": "target version\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc06-override-false", "src-tc06", "main", "dst-tc06", "main")
				repo.Spec.Override = false
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				_, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)
				require.Equal(t, "target version\n", readRemoteFile(t, "dst-tc06", "main", "content/existing.txt"))
				require.Equal(t, "fresh file\n", readRemoteFile(t, "dst-tc06", "main", "content/new.txt"))
			},
		},
		{
			name: "TC07-KrateoIgnore",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc07", "main")
				createGiteaRepo(t, "dst-tc07", "main")
				commitFilesToRepo(t, "src-tc07", "main", "seed source", map[string]string{
					"content/.krateoignore": "/content/ignored.txt\n",
					"content/ignored.txt":   "Hello {{name}}!\n",
					"content/kept.txt":      "Hello {{name}}!\n",
				})
				require.NoError(t, r.Create(ctx, &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "tc07-values", Namespace: namespace},
					Data: map[string]string{
						"values": `{"name":"Krateo"}`,
					},
				}))
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc07-krateoignore", "src-tc07", "main", "dst-tc07", "main")
				repo.Spec.Override = true
				repo.Spec.ConfigMapKeyRef = configMapSelector("tc07-values", "values")
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				_, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)
				require.Equal(t, "Hello Krateo!\n", readRemoteFile(t, "dst-tc07", "main", "content/kept.txt"))
				require.Equal(t, "Hello {{name}}!\n", readRemoteFile(t, "dst-tc07", "main", "content/ignored.txt"))
			},
		},
		{
			name: "TC08-MustacheDefault",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc08", "main")
				createGiteaRepo(t, "dst-tc08", "main")
				commitFilesToRepo(t, "src-tc08", "main", "seed source", map[string]string{
					"content/template.txt": "Hello {{name}}!\n",
				})
				require.NoError(t, r.Create(ctx, &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "tc08-values", Namespace: namespace},
					Data: map[string]string{
						"values": `{"name":"Krateo"}`,
					},
				}))
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc08-mustache", "src-tc08", "main", "dst-tc08", "main")
				repo.Spec.Override = true
				repo.Spec.ConfigMapKeyRef = configMapSelector("tc08-values", "values")
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				_, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)
				require.Equal(t, "Hello Krateo!\n", readRemoteFile(t, "dst-tc08", "main", "content/template.txt"))
			},
		},
		{
			name: "TC09-GoTemplateAnnotation",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc09", "main")
				createGiteaRepo(t, "dst-tc09", "main")
				// The repo's own placeholder uses the default delimiters; the `{{ }}` line is
				// Helm's and must survive untouched — that coexistence is the point of the
				// delimiter choice, and it is only observable end to end.
				commitFilesToRepo(t, "src-tc09", "main", "seed source", map[string]string{
					"content/template.txt": "Hello {% .name %}!\nreplicas: {{ .Values.replicas }}\n",
				})
				require.NoError(t, r.Create(ctx, &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "tc09-values", Namespace: namespace},
					Data: map[string]string{
						"values": `{"name":"GoTemplate"}`,
					},
				}))
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc09-gotemplate", "src-tc09", "main", "dst-tc09", "main")
				repo.ObjectMeta.Annotations = map[string]string{
					AnnotationTemplatingEngine: "gotemplate",
				}
				repo.Spec.Override = true
				repo.Spec.ConfigMapKeyRef = configMapSelector("tc09-values", "values")
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				_, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)
				require.Equal(t, "Hello GoTemplate!\nreplicas: {{ .Values.replicas }}\n",
					readRemoteFile(t, "dst-tc09", "main", "content/template.txt"))
			},
		},
		{
			name: "TC10-EnableUpdateOverrideFalse",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc10", "main")
				createGiteaRepo(t, "dst-tc10", "main")
				commitFilesToRepo(t, "src-tc10", "main", "seed source", map[string]string{
					"content/existing.txt": "source version v1\n",
					"content/shared.txt":   "shared v1\n",
				})
				commitFilesToRepo(t, "dst-tc10", "main", "seed target", map[string]string{
					"content/existing.txt": "target protected\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc10-update-override-false", "src-tc10", "main", "dst-tc10", "main")
				repo.Spec.EnableUpdate = true
				repo.Spec.Override = false
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				current, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)

				initialOrigin := current.Status.OriginCommitId
				initialTarget := current.Status.TargetCommitId

				updatedOrigin := commitFilesToRepo(t, "src-tc10", "main", "update source", map[string]string{
					"content/existing.txt":  "source version v2\n",
					"content/shared.txt":    "shared v2\n",
					"content/new-after.txt": "arrived later\n",
				})

				updated, err := waitForRepo(ctx, r, repoName, 2*time.Minute, func(repo *repov1alpha1.Repo) bool {
					return repo.GetCondition(commonv1.TypeReady).Status == metav1.ConditionTrue &&
						repo.Status.OriginCommitId == updatedOrigin &&
						repo.Status.TargetCommitId != "" &&
						repo.Status.TargetCommitId != initialTarget &&
						repo.Status.OriginCommitId != initialOrigin
				})
				require.NoError(t, err)
				require.NotEqual(t, initialTarget, updated.Status.TargetCommitId)
				require.Equal(t, "target protected\n", readRemoteFile(t, "dst-tc10", "main", "content/existing.txt"))
				require.Equal(t, "shared v1\n", readRemoteFile(t, "dst-tc10", "main", "content/shared.txt"))
				require.Equal(t, "arrived later\n", readRemoteFile(t, "dst-tc10", "main", "content/new-after.txt"))
			},
		},
		{
			name: "TC11-GoTemplateDelimsAnnotation",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc11", "main")
				createGiteaRepo(t, "dst-tc11", "main")
				// Opting back in to Go's native delimiters, for content known not to be a chart.
				commitFilesToRepo(t, "src-tc11", "main", "seed source", map[string]string{
					"content/template.txt": "Hello {{ .name }}!\n",
				})
				require.NoError(t, r.Create(ctx, &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "tc11-values", Namespace: namespace},
					Data: map[string]string{
						"values": `{"name":"Delims"}`,
					},
				}))
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc11-delims", "src-tc11", "main", "dst-tc11", "main")
				repo.ObjectMeta.Annotations = map[string]string{
					AnnotationTemplatingEngine:  "gotemplate",
					templating.DelimsAnnotation: "{{,}}",
				}
				repo.Spec.Override = true
				repo.Spec.ConfigMapKeyRef = configMapSelector("tc11-values", "values")
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				_, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)
				require.Equal(t, "Hello Delims!\n", readRemoteFile(t, "dst-tc11", "main", "content/template.txt"))
			},
		},
		{
			name: "TC11-DisableUpdateOverrideTrue",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc11", "main")
				createGiteaRepo(t, "dst-tc11", "main")
				commitFilesToRepo(t, "src-tc11", "main", "seed source", map[string]string{
					"content/app.txt": "v1\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc11-disable-update-override-true", "src-tc11", "main", "dst-tc11", "main")
				repo.Spec.Override = true
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				current, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)

				initialOrigin := current.Status.OriginCommitId
				initialTarget := current.Status.TargetCommitId
				initialRemoteTarget := latestRemoteCommit(t, "dst-tc11", "main")

				updatedOrigin := commitFilesToRepo(t, "src-tc11", "main", "update source", map[string]string{
					"content/app.txt":   "v2\n",
					"content/extra.txt": "should not sync\n",
				})
				require.NotEqual(t, initialOrigin, updatedOrigin)

				require.Never(t, func() bool {
					current := &repov1alpha1.Repo{}
					if err := r.Get(ctx, repoName, namespace, current); err != nil {
						return false
					}
					synced := current.GetCondition(commonv1.TypeSynced)
					return synced.Status == metav1.ConditionFalse && synced.Reason == commonv1.ReasonReconcileError
				}, 15*time.Second, 2*time.Second)

				later := &repov1alpha1.Repo{}
				err = r.Get(ctx, repoName, namespace, later)
				require.NoError(t, err)
				require.Equal(t, initialOrigin, later.Status.OriginCommitId)
				require.Equal(t, initialTarget, later.Status.TargetCommitId)
				require.Equal(t, initialRemoteTarget, latestRemoteCommit(t, "dst-tc11", "main"))
				require.Equal(t, metav1.ConditionTrue, later.GetCondition(commonv1.TypeReady).Status)
				require.Equal(t, metav1.ConditionTrue, later.GetCondition(commonv1.TypeSynced).Status)
				require.Equal(t, "v1\n", readRemoteFile(t, "dst-tc11", "main", "content/app.txt"))
				assertRemoteFileAbsent(t, "dst-tc11", "main", "content/extra.txt")
			},
		},
		{
			name: "TC12-DisableUpdateDetectsTargetDrift",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc12", "main")
				createGiteaRepo(t, "dst-tc12", "main")
				commitFilesToRepo(t, "src-tc12", "main", "seed source", map[string]string{
					"content/app.txt": "baseline\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc12-detect-target-drift", "src-tc12", "main", "dst-tc12", "main")
				repo.Spec.Override = true
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				current, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)
				require.NotEmpty(t, current.Status.TargetCommitId)

				deleteGiteaRepo(t, "dst-tc12")
				createGiteaRepo(t, "dst-tc12", "main")

				updated, err := waitForRepo(ctx, r, repoName, 45*time.Second, func(repo *repov1alpha1.Repo) bool {
					ready := repo.GetCondition(commonv1.TypeReady)
					synced := repo.GetCondition(commonv1.TypeSynced)
					return ready.Status == metav1.ConditionFalse &&
						synced.Status == metav1.ConditionFalse &&
						synced.Reason == commonv1.ReasonReconcileError
				})
				require.NoError(t, err)
				require.Equal(t, commonv1.ReasonUnavailable, updated.GetCondition(commonv1.TypeReady).Reason)
				require.Contains(t, updated.GetCondition(commonv1.TypeSynced).Message, "target commit")
			},
		},
		{
			name: "TC13-AutoHealAfterEnableUpdateFalse",
			setup: func(ctx context.Context, t *testing.T, r *resources.Resources) {
				createGiteaRepo(t, "src-tc13", "main")
				createGiteaRepo(t, "dst-tc13", "main")
				commitFilesToRepo(t, "src-tc13", "main", "seed source", map[string]string{
					"content/app.txt": "v1\n",
				})
			},
			repo: func() *repov1alpha1.Repo {
				repo := newRepoResource("tc13-autoheal", "src-tc13", "main", "dst-tc13", "main")
				repo.Spec.Override = true
				repo.Spec.EnableUpdate = false // Parte disabilitato
				return repo
			}(),
			verify: func(ctx context.Context, t *testing.T, r *resources.Resources, repoName string) {
				// Il primo sync (fase di Create) avviene a prescindere da EnableUpdate
				_, err := waitForRepoCondition(ctx, r, repoName, commonv1.TypeReady, metav1.ConditionTrue, 90*time.Second)
				require.NoError(t, err)
				initialTarget := latestRemoteCommit(t, "dst-tc13", "main")

				// Simuliamo un aggiornamento nel repo sorgente
				updatedOrigin := commitFilesToRepo(t, "src-tc13", "main", "update source", map[string]string{
					"content/app.txt": "v2\n",
				})

				// Essendo EnableUpdate = false, il controller resta passivo ma non deve segnalare errori.
				require.Never(t, func() bool {
					current := &repov1alpha1.Repo{}
					if err := r.Get(ctx, repoName, namespace, current); err != nil {
						return false
					}
					synced := current.GetCondition(commonv1.TypeSynced)
					return synced.Status == metav1.ConditionFalse && synced.Reason == commonv1.ReasonReconcileError
				}, 15*time.Second, 2*time.Second)

				current := &repov1alpha1.Repo{}
				err = r.Get(ctx, repoName, namespace, current)
				require.NoError(t, err)
				require.Equal(t, initialTarget, current.Status.TargetCommitId)
				require.Equal(t, initialTarget, latestRemoteCommit(t, "dst-tc13", "main"))
				require.Equal(t, metav1.ConditionTrue, current.GetCondition(commonv1.TypeReady).Status)
				require.Equal(t, metav1.ConditionTrue, current.GetCondition(commonv1.TypeSynced).Status)

				// AUTO-HEAL: L'utente riabilita l'update
				current.Spec.EnableUpdate = true
				err = r.Update(ctx, current)
				require.NoError(t, err)

				// Verifichiamo che il controller guarisca automaticamente e completi l'allineamento
				_, err = waitForRepo(ctx, r, repoName, 90*time.Second, func(repo *repov1alpha1.Repo) bool {
					return repo.GetCondition(commonv1.TypeReady).Status == metav1.ConditionTrue &&
						repo.Status.OriginCommitId == updatedOrigin
				})
				require.NoError(t, err)
				require.Equal(t, "v2\n", readRemoteFile(t, "dst-tc13", "main", "content/app.txt"))
			},
		},
	}

	f := features.New("RepoControllerFeatures").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			mgrCtx, mgrCancel := context.WithCancel(context.Background())
			ctx = context.WithValue(ctx, "mgrCancel", mgrCancel)

			_, err := setupController(mgrCtx, cfg)
			require.NoError(t, err)

			r, err := resources.New(cfg.Client().RESTConfig())
			require.NoError(t, err)

			require.NoError(t, r.Create(ctx, &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: gitAuthSecretName, Namespace: namespace},
				Data: map[string][]byte{
					"token":    []byte(giteaPassword),
					"username": []byte(giteaUsername),
				},
			}))

			require.NoError(t, r.Create(ctx, &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: gitBadAuthSecretName, Namespace: namespace},
				Data: map[string][]byte{
					"token":    []byte("wrong-password"),
					"username": []byte(giteaUsername),
				},
			}))

			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if cancel, ok := ctx.Value("mgrCancel").(context.CancelFunc); ok {
				cancel()
				time.Sleep(1 * time.Second) // Give manager time to stop
			}
			return ctx
		})

	for _, tc := range cases {
		tc := tc
		f.Assess(tc.name, func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			r, err := resources.New(cfg.Client().RESTConfig())
			require.NoError(t, err)

			if tc.setup != nil {
				tc.setup(ctx, t, r)
			}

			require.NoError(t, r.Create(ctx, tc.repo))

			if tc.verify != nil {
				tc.verify(ctx, t, r, tc.repo.Name)
			}

			return ctx
		})
	}

	testenv.Test(t, f.Feature())
}
