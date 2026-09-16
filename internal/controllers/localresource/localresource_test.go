//go:build integration
// +build integration

package localresource

import (
	"context"
	"crypto/tls"
	"encoding/base64"
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

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/krateoplatformops/git-provider/apis"
	"github.com/krateoplatformops/git-provider/apis/localresource/v1alpha1"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/option"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/go-logr/logr"
	"github.com/krateo-platformops/plumbing/logger"
	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"

	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
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
	crdPath       = "../../../crds"
	testdataPath  = "../../../testdata/"
	manifestsPath = "./testdata"

	namespace = "test-system"
)

var (
	giteaUsername = "admin"
	giteaPassword = "admin123"
)

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	clusterName = "krateo-git-provider-controller"
	testenv = env.New()
	kindCluster := kind.NewCluster(clusterName)

	_ = apiextensionsv1.AddToScheme(clientsetscheme.Scheme)
	_ = apis.AddToScheme(clientsetscheme.Scheme)

	cli, err := client.New(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		panic(err)
	}
	defer cli.Close()

	var containerId string

	testenv.Setup(
		envfuncs.CreateCluster(kindCluster, clusterName),
		e2e.CreateNamespace(namespace),

		// Start docker gitea instance
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			// Crea il client Docker
			tmpdir, err := os.MkdirTemp(os.TempDir(), "local-resource-test-gitea-*")
			if err != nil {
				panic(err)
			}

			imageName := "gitea/gitea:latest"
			fmt.Println("Pulling Gitea image...")
			reader, err := cli.ImagePull(ctx, imageName, client.ImagePullOptions{})
			if err != nil {
				panic(err)
			}
			defer reader.Close()
			io.Copy(os.Stdout, reader)

			containerPort, err := network.ParsePort("3000/tcp")
			if err != nil {
				panic(err)
			}
			sshPort, err := network.ParsePort("22/tcp")
			if err != nil {
				panic(err)
			}
			httpsPort, err := network.ParsePort("443/tcp")
			if err != nil {
				panic(err)
			}

			portBinding := network.PortMap{
				containerPort: []network.PortBinding{
					{
						HostIP:   netip.AddrFrom4([4]byte{127, 0, 0, 1}),
						HostPort: "3000",
					},
				},
				sshPort: []network.PortBinding{
					{
						HostIP:   netip.AddrFrom4([4]byte{127, 0, 0, 1}),
						HostPort: "2222",
					},
				},
				httpsPort: []network.PortBinding{
					{
						HostIP:   netip.AddrFrom4([4]byte{127, 0, 0, 1}),
						HostPort: "8443",
					},
				},
			}

			containerConfig := &container.Config{
				Image: imageName,
				ExposedPorts: network.PortSet{
					containerPort: struct{}{},
					sshPort:       struct{}{},
					httpsPort:     struct{}{},
				},
				Env: []string{
					"GITEA__database__DB_TYPE=sqlite3",
					"GITEA__security__INSTALL_LOCK=true",
					"USER_UID=1000",
					"USER_GID=1000",

					// HTTPS with self-signed certs
					"GITEA__server__DOMAIN=127.0.0.1",
					"GITEA__server__HTTP_PORT=443",
					"GITEA__server__ROOT_URL=https://127.0.0.1:8443",
					"GITEA__server__PROTOCOL=https",
					"GITEA__server__CERT_FILE=/data/cert.pem",
					"GITEA__server__KEY_FILE=/data/key.pem",
				},
				Entrypoint: []string{"/bin/sh", "-c"},
				Cmd: []string{
					fmt.Sprintf(`
			
            # Generate self-signed certificates if they don't exist
            if [ ! -f /data/cert.pem ]; then
                echo "Generating self-signed certificates..."
                cd /data && /usr/local/bin/gitea cert --host localhost,127.0.0.1 --ca
            fi
            			
			chown -R 1000:1000 /data

            # Setup Gitea
            echo 'su-exec git /usr/local/bin/gitea migrate' >> /etc/s6/gitea/setup
            echo 'su-exec git /usr/local/bin/gitea admin user create --username %s --password %s --email admin@local --admin --must-change-password=false' >> /etc/s6/gitea/setup
            
            # Start Gitea
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

			networkConfig := &network.NetworkingConfig{}

			resp, err := cli.ContainerCreate(
				ctx,
				client.ContainerCreateOptions{
					Config:           containerConfig,
					HostConfig:       hostConfig,
					NetworkingConfig: networkConfig,
					Name:             "gitea-container",
				})
			if err != nil {
				panic(err)
			}

			fmt.Printf("Container created: %s\n", resp.ID)

			// Print the network settings of the container
			networkSettings, err := cli.ContainerInspect(ctx, resp.ID, client.ContainerInspectOptions{})
			if err != nil {
				panic(err)
			}
			r, _ := networkSettings.Raw.MarshalJSON()
			fmt.Printf("Network settings: %+v\n", r)

			// Print the container logs for debugging purposes
			go func() {
				logsReader, err := cli.ContainerLogs(ctx, resp.ID, client.ContainerLogsOptions{
					ShowStdout: true,
					ShowStderr: true,
					Follow:     true,
				})
				if err != nil {
					panic(err)
				}
				defer logsReader.Close()
				io.Copy(os.Stdout, logsReader)
			}()

			_, err = cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{})
			if err != nil {
				panic(err)
			}
			containerId = resp.ID

			fmt.Printf("Container started successfully!\n")
			fmt.Printf("Access Gitea at: https://127.0.0.1:8443\n")

			// Wait for Gitea to be ready
			err = waitForGitea(ctx)
			if err != nil {
				panic(err)
			}
			return ctx, nil
		},

		// Create test repository
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			tr := &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
			client := &http.Client{Transport: tr}

			// Con Basic Auth
			req2, _ := http.NewRequest("GET", "https://127.0.0.1:8443/api/v1/user", nil)
			req2.SetBasicAuth(giteaUsername, giteaPassword)

			resp2, err := client.Do(req2)
			if err != nil {
				panic(err)
			}
			defer resp2.Body.Close()

			if resp2.StatusCode != http.StatusOK {
				panic(fmt.Sprintf("Failed to get user info: %s", resp2.Status))
			}

			req3Body := `{
			  "name": "test-repo",
			  "description": "My test repository",
			  "private": false,
			  "auto_init": true,
			  "readme": "Default"
			}`
			req3, _ := http.NewRequest("POST", "https://127.0.0.1:8443/api/v1/user/repos", strings.NewReader(req3Body))
			req3.Header.Set("Content-Type", "application/json")
			req3.SetBasicAuth(giteaUsername, giteaPassword)
			resp3, err := client.Do(req3)
			if err != nil {
				panic(err)
			}
			defer resp3.Body.Close()
			if resp3.StatusCode != http.StatusCreated {
				panic(fmt.Sprintf("Failed to create repository: %s", resp3.Status))
			}

			return ctx, nil
		},

		// Create Git credentials secret from giteaAdmin/giteaAdminPassword
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				return ctx, err
			}
			r.WithNamespace(namespace)

			err = decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(manifestsPath)), "gitea-creds.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
			)
			if err != nil {
				return ctx, err
			}

			return ctx, nil
		},

		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				return ctx, err
			}
			r.WithNamespace(namespace)

			// Install CRDs
			err = decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(crdPath)), "*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
			)

			time.Sleep(2 * time.Second) // wait for the compositiondefinition CRD to be registered

			return ctx, nil
		},
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.TeardownCRDs(crdPath, "git.krateo.io_localresources.yaml"),
		envfuncs.DestroyCluster(clusterName),
		func(ctx context.Context, c *envconf.Config) (context.Context, error) {
			if v := ctx.Value(stopKey{}); v != nil {
				if stop, ok := v.(context.CancelFunc); ok {
					fmt.Println("Stopping controller manager at ", time.Now().String())
					stop() // stops mgr.Start and the background goroutine
				}
			}

			_, err := cli.ContainerStop(ctx, containerId, client.ContainerStopOptions{})
			if err != nil {
				panic(err)
			}
			_, err = cli.ContainerRemove(ctx, containerId, client.ContainerRemoveOptions{})
			if err != nil {
				panic(err)
			}
			return ctx, nil
		},
	)

	os.Exit(testenv.Run(m))
}

type stopKey struct{} // key for storing stop func in ctx

func TestController(t *testing.T) {
	os.Setenv("DEBUG", "TRUE")

	setupController := func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		lh := logger.NewHandler(true, os.Stderr)

		logrlog := logr.FromSlogHandler(slog.New(lh).Handler())
		log := logging.NewLogrLogger(logrlog)

		ctrl.SetLogger(logrlog)

		mgr, err := ctrl.NewManager(cfg.Client().RESTConfig(), ctrl.Options{
			Metrics: server.Options{
				BindAddress: "0", // disable metrics for tests
			},
		})
		if err != nil {
			return ctx, err
		}

		o := controller.Options{
			Logger:                  log,
			MaxConcurrentReconciles: 1,
			PollInterval:            10 * time.Second,
			GlobalRateLimiter:       ratelimiter.NewGlobalExponential(1*time.Second, 1*time.Minute),
		}

		tmpdir, err := os.MkdirTemp(os.TempDir(), "local-resource-test-*")
		if err != nil {
			log.Info("Cannot create temp dir", "error", err)
			os.Exit(1)
		}
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
			log.Info("Cannot setup controllers", "error", err)
			os.Exit(1)
		}
		if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
			log.Info("Cannot start controller manager", "error", err)
			os.Exit(1)
		}
		return ctx, nil
	}

	var r *resources.Resources

	f := features.New("Setup").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			stopCtx, stop := context.WithCancel(context.Background())

			var err error
			r, err = resources.New(cfg.Client().RESTConfig())
			if err != nil {
				t.Fatal(err)
			}

			err = apis.AddToScheme(r.GetScheme())
			if err != nil {
				t.Fatal(err)
			}
			err = apiextensionsv1.AddToScheme(r.GetScheme())
			if err != nil {
				t.Fatal(err)
			}

			go func() {
				_, err := setupController(stopCtx, cfg)
				if err != nil {
					fmt.Printf("Error starting controller manager: %v\n", err)
				}
			}()

			// store cancel func in returned context so callers can stop the manager
			ctx = context.WithValue(ctx, stopKey{}, stop)
			return ctx
		}).
		Assess("Test Create", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			toCreate := []struct {
				filename    string
				expected    string
				createError bool
			}{
				{
					filename: "local_fromYaml.yaml",
					expected: `metadata:
  name: example-repo
  namespace: default`,
				},
				{
					filename: "local_fromString.yaml",
					expected: `kind: RemoteRepo
apiVersion: test.com/v6
metadata:
  name: example-repo
  namespace: default
spec:
  zipArchive: false
  authMethod: generic
  branch: main
  placeholderTest: "42"`, // order of field should be preserved if fromString is used
				},
				{
					filename: "local_fromRef.yaml",
					expected: `testkey: testvalue`, // content of the configmap key
				},
				{
					filename: "local_fromString_syncEnabled_false.yaml",
					expected: `kind: RemoteRepo
apiVersion: test.com/v6
metadata:
  name: example-repo
  namespace: default
spec:
  zipArchive: false
  authMethod: generic
  branch: main
  placeholderTest: "42"`, // order of field should be preserved if fromString is used
				},
				{
					filename: "local_fromString_override_false.yaml",
					expected: `kind: RemoteRepo
apiVersion: test.com/v6
metadata:
  name: example-repo
  namespace: default
spec:
  zipArchive: false
  authMethod: generic
  branch: main
  placeholderTest: "42"`, // order of field should be preserved if fromString is used
				},
			}

			// Create an configmap cr to reference in the localresource fromRef test
			configMapResource := v1.ConfigMap{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "test-configmap",
					Namespace: namespace,
				},
				Data: map[string]string{
					"testkey": "testvalue",
				},
			}
			err := r.Create(ctx, &configMapResource)
			if err != nil {
				t.Fatalf("Failed to create ConfigMap %s: %v", configMapResource.Name, err)
			}

			time.Sleep(5 * time.Second) // wait for the configmap to be created

			r.WithNamespace(namespace)

			for _, test := range toCreate {
				var res v1alpha1.LocalResource
				err := decoder.DecodeFile(
					os.DirFS(filepath.Join(testdataPath)), test.filename,
					&res,
					decoder.MutateNamespace(namespace),
				)
				if err != nil {
					t.Fatal(err)
				}

				err = r.Create(ctx, &res)
				if err != nil {
					t.Fatalf("Failed to create LocalResource %s: %v", res.Name, err)
				}
			}
			time.Sleep(30 * time.Second) // wait for the controller to pick up the new resources

			// verify that the resources have been created in the git repo
			for _, test := range toCreate {
				var res v1alpha1.LocalResource
				err := decoder.DecodeFile(
					os.DirFS(filepath.Join(testdataPath)), test.filename,
					&res,
					decoder.MutateNamespace(namespace),
				)
				if err != nil {
					t.Fatal(err)
				}

				// we need to check if the resource has been created in the git repo
				tr := &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
				client := &http.Client{Transport: tr}
				repoName := strings.TrimSuffix(strings.Split(res.Spec.ToRepo.Url, "/")[len(strings.Split(res.Spec.ToRepo.Url, "/"))-1], ".git")
				url := fmt.Sprintf("https://127.0.0.1:8443/api/v1/repos/admin/%s/contents/%s?ref=%s", repoName, res.Spec.FromResource.FileName, res.Spec.ToRepo.Branch)
				req, _ := http.NewRequest("GET", url, nil)
				req.SetBasicAuth(giteaUsername, giteaPassword)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("Expected status 200 OK, got %s calling %s", resp.Status, url)
				}
				// Check if the content matches the expected resource
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				bodyStr := string(body)

				// The content is base64 encoded inside the "content" field
				decodedContent, err := decodeGiteaContent(bodyStr)
				if err != nil {
					t.Fatalf("Failed to decode content for LocalResource %s: %v", res.Name, err)
				}

				if !strings.Contains(decodedContent, test.expected) {
					t.Fatalf("Expected content (filename: %s) to contain: \n %q, but it was not found in response body:\n %s", test.filename, test.expected, decodedContent)
				}
			}
			return ctx
		}).Assess("Test Change Something", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		toUpgrade := []struct {
			filename   string
			patch      string
			expected   string
			ref        bool
			patchError bool
		}{
			{
				filename: "local_fromYaml.yaml",
				patch:    `[{"op": "replace", "path": "/spec/fromResource/fromYaml/spec/zipArchive", "value": true}]`,
				expected: `zipArchive: true`,
			},
			{
				filename: "local_fromString.yaml",
				patch: `[{
            "op": "replace",
            "path": "/spec/fromResource/fromString",
            "value": "Hello, Nginx v1.1.0!"
        }]`,
				expected: `Hello, Nginx v1.1.0!`,
			},
			{
				ref:      true,
				filename: "local_fromRef.yaml",
				patch:    `[{"op": "replace", "path": "/data/testkey", "value": "test-update"}]`,
				expected: `testkey: test-update`, // content of the configmap key
			},
			{
				filename:   "local_fromString_syncEnabled_false.yaml",
				patch:      `[{"op": "replace","path": "/spec/fromResource/fromString","value": "Hello, World v2!"}]`,
				patchError: true,
				expected: `kind: RemoteRepo
apiVersion: test.com/v6
metadata:
  name: example-repo
  namespace: default
spec:
  zipArchive: false
  authMethod: generic
  branch: main
  placeholderTest: "42"`, // value should not change as syncEnabled is false
			},
			{
				filename:   "local_fromString_override_false.yaml",
				patch:      `[{"op": "replace","path": "/spec/fromResource/fromString","value": "Overridden Value!"}]`,
				patchError: true,
				expected: `kind: RemoteRepo
apiVersion: test.com/v6
metadata:
  name: example-repo
  namespace: default
spec:
  zipArchive: false
  authMethod: generic
  branch: main
  placeholderTest: "42"`, // value should not change as override is false
			},
		}

		r, err := resources.New(cfg.Client().RESTConfig())
		if err != nil {
			t.Fail()
		}

		for _, test := range toUpgrade {
			var res v1alpha1.LocalResource
			err := decoder.DecodeFile(
				os.DirFS(filepath.Join(testdataPath)), test.filename,
				&res,
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}

			// This only works for Configmap refs for now
			if test.ref {
				var refCm v1.ConfigMap
				err = r.Get(ctx, res.Spec.FromResource.FromRef.Name, res.Spec.FromResource.FromRef.Namespace, &refCm)
				if err != nil {
					t.Fatalf("Failed to get referenced ConfigMap: %v", err)
				}
				err = r.Patch(ctx, &refCm, k8s.Patch{PatchType: types.JSONPatchType, Data: []byte(test.patch)})
				if err != nil && test.patchError == false {
					t.Fatalf("Failed to patch referenced ConfigMap %s: %v", refCm.Name, err)
				}
			} else {
				err = r.Patch(ctx, &res, k8s.Patch{PatchType: types.JSONPatchType, Data: []byte(test.patch)})
				if err != nil && test.patchError == false {
					t.Fatalf("Failed to patch LocalResource %s: %v", res.Name, err)
				}
			}
		}

		time.Sleep(30 * time.Second) // wait for the controller to pick up the new resources

		// verify that the changes have been applied
		for _, test := range toUpgrade {
			var res v1alpha1.LocalResource
			err := decoder.DecodeFile(
				os.DirFS(filepath.Join(testdataPath)), test.filename,
				&res,
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}

			var updatedRes v1alpha1.LocalResource
			err = r.Get(ctx, res.GetName(), res.GetNamespace(), &updatedRes)
			if err != nil {
				t.Fatal(err)
			}

			// verify that the git-provider has updated the resource in the git repo
			tr := &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
			client := &http.Client{Transport: tr}
			repoName := strings.TrimSuffix(strings.Split(res.Spec.ToRepo.Url, "/")[len(strings.Split(res.Spec.ToRepo.Url, "/"))-1], ".git")
			url := fmt.Sprintf("https://127.0.0.1:8443/api/v1/repos/admin/%s/contents/%s?ref=%s", repoName, res.Spec.FromResource.FileName, res.Spec.ToRepo.Branch)
			req, _ := http.NewRequest("GET", url, nil)
			req.SetBasicAuth(giteaUsername, giteaPassword)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Expected status 200 OK, got %s calling %s", resp.Status, url)
			}
			// Check if the content matches the updated resource
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			bodyStr := string(body)

			// The content is base64 encoded inside the "content" field
			decodedContent, err := decodeGiteaContent(bodyStr)
			if err != nil {
				t.Fatalf("Failed to decode content for LocalResource %s: %v", res.Name, err)
			}
			if !strings.Contains(decodedContent, test.expected) {
				t.Fatalf("Expected content to contain %q, but it was not found in response body: %s", test.expected, decodedContent)
			}
		}

		return ctx
	}).Assess("Test Delete and Recreate", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		toDeleteAndPatch := []struct {
			filename   string
			patch      string
			expected   string
			patchError bool
			ref        bool
		}{
			{
				filename: "local_fromYaml.yaml",
				patch:    `[{"op": "replace", "path": "/spec/fromResource/fromYaml/spec/zipArchive", "value": true}]`,
				expected: `zipArchive: true`,
			},
			{
				filename: "local_fromString.yaml",
				patch: `[{
            "op": "replace",
            "path": "/spec/fromResource/fromString",
            "value": "Hello, Nginx v1.1.0!"
        }]`,
				expected: `Hello, Nginx v1.1.0!`,
			},
			{
				ref:      true,
				filename: "local_fromRef.yaml",
				patch:    `[{"op": "replace", "path": "/data/testkey", "value": "test-recreate"}]`,
				expected: `testkey: test-recreate`, // content of the configmap key
			},
			{
				filename: "local_fromString_syncEnabled_false.yaml",
				patch:    `[{"op": "replace","path": "/spec/fromResource/fromString","value": "Hello, World v2!"}]`,
				expected: `kind: RemoteRepo
apiVersion: test.com/v6
metadata:
  name: example-repo
  namespace: default
spec:
  zipArchive: false
  authMethod: generic
  branch: main
  placeholderTest: "42"`, // value should not change as syncEnabled is false
			},
			{
				filename: "local_fromString_override_false.yaml",
				patch:    `[{"op": "replace","path": "/spec/fromResource/fromString","value": "Overridden Value!"}]`,
				expected: `kind: RemoteRepo
apiVersion: test.com/v6
metadata:
  name: example-repo
  namespace: default
spec:
  zipArchive: false
  authMethod: generic
  branch: main
  placeholderTest: "42"`, // value should not change as override is false
			},
		}
		r, err := resources.New(cfg.Client().RESTConfig())
		if err != nil {
			t.Fail()
		}

		for _, test := range toDeleteAndPatch {
			var res v1alpha1.LocalResource
			err := decoder.DecodeFile(
				os.DirFS(filepath.Join(testdataPath)), test.filename,
				&res,
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}

			err = r.Delete(ctx, &res)
			if err != nil {
				t.Fatalf("Failed to delete LocalResource %s: %v", res.Name, err)
			}
		}

		for _, test := range toDeleteAndPatch {
			var res v1alpha1.LocalResource
			err := decoder.DecodeFile(
				os.DirFS(filepath.Join(testdataPath)), test.filename,
				&res,
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}

			err = waitForLocalResourceDeletion(ctx, r, res.GetName(), res.GetNamespace(), 90*time.Second)
			if err != nil {
				t.Fatalf("Failed waiting for LocalResource %s deletion: %v", res.Name, err)
			}
		}

		// Now we recreate them with different specs and we check if values are overritten or not according to the specs
		for _, test := range toDeleteAndPatch {
			var res v1alpha1.LocalResource
			err := decoder.DecodeFile(
				os.DirFS(filepath.Join(testdataPath)), test.filename,
				&res,
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}

			if test.ref {
				var refCm v1.ConfigMap
				err = r.Get(ctx, res.Spec.FromResource.FromRef.Name, res.Spec.FromResource.FromRef.Namespace, &refCm)
				if err != nil {
					t.Fatalf("Failed to get referenced ConfigMap: %v", err)
				}
				err = r.Patch(ctx, &refCm, k8s.Patch{PatchType: types.JSONPatchType, Data: []byte(test.patch)})
				if err != nil && test.patchError == false {
					t.Fatalf("Failed to patch referenced ConfigMap %s: %v", refCm.Name, err)
				}
			} else {
				// now we patch with the new spec to the resource (that does not exist in the cluster yet) so we need to change res object
				err = applyPatchToCR(&res, test.patch)
				if err != nil {
					t.Fatalf("Failed to apply patch to LocalResource %s: %v", res.Name, err)
				}
			}

			err = r.Create(ctx, &res)
			if err != nil {
				t.Fatalf("Failed to recreate LocalResource %s: %v", res.Name, err)
			}
		}

		time.Sleep(15 * time.Second)

		// verify that the changes have been applied
		for _, test := range toDeleteAndPatch {
			var res v1alpha1.LocalResource
			err := decoder.DecodeFile(
				os.DirFS(filepath.Join(testdataPath)), test.filename,
				&res,
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}

			var updatedRes v1alpha1.LocalResource
			err = r.Get(ctx, res.GetName(), res.GetNamespace(), &updatedRes)
			if err != nil {
				t.Fatal(err)
			}

			// verify that the git-provider has updated the resource in the git repo
			tr := &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
			client := &http.Client{Transport: tr}
			repoName := strings.TrimSuffix(strings.Split(res.Spec.ToRepo.Url, "/")[len(strings.Split(res.Spec.ToRepo.Url, "/"))-1], ".git")
			url := fmt.Sprintf("https://127.0.0.1:8443/api/v1/repos/admin/%s/contents/%s?ref=%s", repoName, res.Spec.FromResource.FileName, res.Spec.ToRepo.Branch)
			req, _ := http.NewRequest("GET", url, nil)
			req.SetBasicAuth(giteaUsername, giteaPassword)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Expected status 200 OK, got %s calling %s", resp.Status, url)
			}
			// Check if the content matches the updated resource
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			bodyStr := string(body)

			// The content is base64 encoded inside the "content" field
			decodedContent, err := decodeGiteaContent(bodyStr)
			if err != nil {
				t.Fatalf("Failed to decode content for LocalResource %s: %v", res.Name, err)
			}
			if !strings.Contains(decodedContent, test.expected) {
				t.Fatalf("Expected content to contain %q, but it was not found in response body: %s", test.expected, decodedContent)
			}
		}

		return ctx
	}).Feature()

	testenv.Test(t, f)
}

func decodeGiteaContent(body string) (string, error) {
	// Find the "content" field
	contentKey := `"content":"`
	startIndex := strings.Index(body, contentKey)
	if startIndex == -1 {
		return "", fmt.Errorf("content field not found")
	}
	startIndex += len(contentKey)
	endIndex := strings.Index(body[startIndex:], `"`)
	if endIndex == -1 {
		return "", fmt.Errorf("end of content field not found")
	}
	encodedContent := body[startIndex : startIndex+endIndex]

	// Decode base64 content
	decodedBytes, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encodedContent)))
	if err != nil {
		return "", fmt.Errorf("failed to decode base64 content: %v", err)
	}

	return string(decodedBytes), nil
}

// applyPatchToCR applies a JSON Patch (provided as a JSON string) to a Custom Resource.
//
// 'resource' must be a pointer to your CR structure (e.g., *v1alpha1.LocalResource).
// 'patchString' is the JSON Patch in string format.
// The function modifies 'resource' in place.
func applyPatchToCR(resource interface{}, patchString string) error {
	// 1. Convert the original CR into JSON bytes (Target Document)
	originalJSON, err := json.Marshal(resource)
	if err != nil {
		return err
	}

	// 2. Decode the patch string into a Patch object
	patch, err := jsonpatch.DecodePatch([]byte(patchString))
	if err != nil {
		return err
	}

	// 3. Apply the patch to the original JSON bytes
	patchedJSON, err := patch.Apply(originalJSON)
	if err != nil {
		return err
	}

	// 4. Decode the patched JSON back into the resource structure
	// This updates the value pointed to by 'resource'.
	if err := json.Unmarshal(patchedJSON, resource); err != nil {
		return err
	}

	return nil
}

func waitForGitea(ctx context.Context) error {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}

	// Con Basic Auth (necessario per l'endpoint /api/v1/user)
	req, _ := http.NewRequest("GET", "https://127.0.0.1:8443/api/v1/user", nil)
	req.SetBasicAuth(giteaUsername, giteaPassword)

	const maxAttempts = 60
	const delay = 5 * time.Second

	for i := 0; i < maxAttempts; i++ {
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			fmt.Println("Gitea is ready!")
			return nil // Successo
		}

		if resp != nil {
			fmt.Printf("Attempt %d/%d: Gitea not ready yet (Status: %s, Error: %v). Retrying in %v...\n", i+1, maxAttempts, resp.Status, err, delay)
		} else {
			fmt.Printf("Attempt %d/%d: Gitea not ready yet (Error: %v). Retrying in %v...\n", i+1, maxAttempts, err, delay)
		}

		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(delay)
	}

	return fmt.Errorf("Gitea failed to become ready after %v attempts", maxAttempts)
}

func waitForLocalResourceDeletion(ctx context.Context, r *resources.Resources, name, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current := &v1alpha1.LocalResource{}
		err := r.Get(ctx, name, namespace, current)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("timed out waiting for LocalResource %s/%s deletion", namespace, name)
}
