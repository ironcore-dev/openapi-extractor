// Copyright 2022 IronCore authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	goflag "flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"github.com/ironcore-dev/controller-utils/buildutils"
	"github.com/ironcore-dev/openapi-extractor/envtestutils"
	"github.com/ironcore-dev/openapi-extractor/envtestutils/apiserver"
	flag "github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	apiServiceTimeout = 5 * time.Minute
)

var (
	testEnv                  *envtest.Environment
	testEnvExt               *envtestutils.EnvironmentExtensions
	log                      = ctrl.Log.WithName("openapi-extractor")
	apiServerCommand         []string
	outputDir                = "."
	apiServicePaths          []string
	openapiTimeout           = 30 * time.Second
	apiServerPackage         string
	apiServerBuildOpts       []string
	attachControlPlaneOutput bool
	attachAPIServerOutput    bool
)

func main() {
	flag.StringVar(&apiServerPackage, "apiserver-package", apiServerPackage, "Package to build the api server")
	flag.StringSliceVar(&apiServerBuildOpts, "apiserver-build-opts", apiServerBuildOpts, "Flags for building the api server")
	flag.StringSliceVar(&apiServerCommand, "apiserver-command", apiServerCommand, "Command to run the api server")
	flag.StringSliceVar(&apiServicePaths, "apiservices", apiServicePaths, "Comma separated list of api service definitions")
	flag.BoolVar(&attachControlPlaneOutput, "attach-control-plane-output", attachControlPlaneOutput, "Whether to print control plane output to stdout/stderr")
	flag.BoolVar(&attachAPIServerOutput, "attach-apiserver-output", attachAPIServerOutput, "Whether to print api server output to stdout/stderr")
	flag.StringVar(&outputDir, "output", outputDir, "Directory to store the extracted OpenAPI specs (default: current directory)")
	flag.DurationVar(&openapiTimeout, "openapi-timeout", openapiTimeout, "Timeout to wait for the /openapi/v3 endpoint for all api services to become available")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(goflag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	if err := extractOpenAPI(ctx); err != nil {
		cancel()
		log.Error(err, "failed to extract OpenAPI")
		os.Exit(1)
	}
}

func extractOpenAPI(ctx context.Context) error {
	testEnv = &envtest.Environment{
		AttachControlPlaneOutput: attachControlPlaneOutput,
	}
	testEnvExt = &envtestutils.EnvironmentExtensions{
		APIServiceDirectoryPaths:       apiServicePaths,
		ErrorIfAPIServicePathIsMissing: true,
	}

	cfg, err := envtestutils.StartWithExtensions(testEnv, testEnvExt)
	if err != nil {
		return fmt.Errorf("failed to start testenv: %w", err)
	}
	defer func() {
		if err := envtestutils.StopWithExtensions(testEnv, testEnvExt); err != nil {
			log.Error(err, "failed to stop testenv")
		}
	}()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	var buildOpts []buildutils.BuildOption
	for _, buildOpt := range apiServerBuildOpts {
		buildOpts = append(buildOpts, buildutils.ModMode(buildOpt)) // TODO: This is not correct. Fix this.
	}

	apiSrv, err := apiserver.New(cfg, apiserver.Options{
		AttachOutput: attachAPIServerOutput,
		Command:      apiServerCommand,
		MainPath:     apiServerPackage,
		BuildOptions: buildOpts,
		ETCDServers:  []string{testEnv.ControlPlane.Etcd.URL.String()},
		Host:         testEnvExt.APIServiceInstallOptions.LocalServingHost,
		Port:         testEnvExt.APIServiceInstallOptions.LocalServingPort,
		CertDir:      testEnvExt.APIServiceInstallOptions.LocalServingCertDir,
	})
	if err != nil {
		return fmt.Errorf("failed to setup api server: %w", err)
	}

	if err := apiSrv.Start(); err != nil {
		return fmt.Errorf("failed to start api server: %w", err)
	}
	defer func() {
		if err := apiSrv.Stop(); err != nil {
			log.Error(err, "failed to stop api server")
		}
	}()

	if err := envtestutils.WaitUntilAPIServicesReadyWithTimeout(apiServiceTimeout, testEnvExt, k8sClient, scheme.Scheme); err != nil {
		return fmt.Errorf("failed to wait for api server to become ready: %w", err)
	}

	clientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create clientset from config: %w", err)
	}

	if err := waitForAPIServicesOpenAPIV3(ctx, log, clientSet, openapiTimeout, testEnvExt.APIServiceInstallOptions.APIServices); err != nil {
		return fmt.Errorf("failed to wait for the api services to become available: %w", err)
	}

	if err := extractOpenAPIv2(ctx, log, clientSet, outputDir); err != nil {
		return fmt.Errorf("failed to extract OpenAPI v2 spec: %w", err)
	}

	if err := extractOpenAPIv3(ctx, log, clientSet, testEnvExt, outputDir); err != nil {
		return fmt.Errorf("failed to extract OpenAPI v3 spec: %w", err)
	}

	return nil
}

func sortedGroupVersions(gvs []schema.GroupVersion) []schema.GroupVersion {
	sort.Slice(gvs, func(i, j int) bool {
		return gvs[i].String() < gvs[j].String()
	})
	return gvs
}

func waitForAPIServicesOpenAPIV3(
	ctx context.Context,
	log logr.Logger,
	clientSet *kubernetes.Clientset,
	timeout time.Duration,
	services []*apiregistrationv1.APIService,
) error {
	testGVs := sets.New[schema.GroupVersion]()
	for _, svc := range services {
		testGVs.Insert(schema.GroupVersion{
			Group:   svc.Spec.Group,
			Version: svc.Spec.Version,
		})
	}

	if err := wait.PollUntilContextTimeout(ctx, 1*time.Second, timeout, true, func(ctx context.Context) (done bool, err error) {
		newTestGVs := sets.New[schema.GroupVersion]()
		for testGV := range testGVs {
			err := clientSet.RESTClient().
				Verb(http.MethodHead).
				AbsPath(fmt.Sprintf("/openapi/v3/apis/%s/%s", testGV.Group, testGV.Version)).
				Do(ctx).
				Error()
			if err != nil {
				newTestGVs.Insert(testGV)
			}
		}

		if newTestGVs.Len() == 0 {
			log.Info("All API services are available")
			return true, nil
		}

		testGVs = newTestGVs
		log.Info("Not all API services are available", "UnavailableGroupVersions", sortedGroupVersions(testGVs.UnsortedList()))
		return false, nil
	}); err != nil {
		return fmt.Errorf("error waiting for api services to become available: %w", err)
	}
	return nil
}

func extractOpenAPIv3(ctx context.Context, log logr.Logger, clientSet *kubernetes.Clientset, ext *envtestutils.EnvironmentExtensions, outputDir string) error {
	log.Info("Extracting OpenAPI v3")
	apiServices := ext.APIServiceInstallOptions.APIServices

	for _, apiService := range apiServices {
		fileName := fmt.Sprintf("apis__%s__%s_openapi.json", apiService.Spec.Group, apiService.Spec.Version)
		path := fmt.Sprintf("/openapi/v3/apis/%s/%s", apiService.Spec.Group, apiService.Spec.Version)

		resp, err := getPath(ctx, clientSet, path)
		if err != nil {
			return fmt.Errorf("failed to get OpenAPI v3 path %s: %w", path, err)
		}

		if err := writeJSONFile(fmt.Sprintf("%s/%s", outputDir, "v3"), fileName, resp); err != nil {
			return fmt.Errorf("failed to write OpenAPI v3 file: %w", err)
		}
	}
	return nil
}

func extractOpenAPIv2(ctx context.Context, log logr.Logger, clientSet *kubernetes.Clientset, outputDir string) error {
	log.Info("Extracting OpenAPI v2")

	path := "/openapi/v2"
	resp, err := getPath(ctx, clientSet, path)
	if err != nil {
		return fmt.Errorf("failed to get OpenAPI v3 path %s: %w", path, err)
	}

	if err := writeJSONFile(outputDir, "swagger.json", resp); err != nil {
		return fmt.Errorf("failed to write OpenAPI v2 file: %w", err)
	}

	return nil
}

func writeJSONFile(dir string, name string, jsonData []byte) error {
	log.Info("Writing file", "OutputDirectory", dir, "File", name)

	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create output directory %s: %w", dir, err)
	}

	var out bytes.Buffer
	if err := json.Indent(&out, jsonData, "", "\t"); err != nil {
		return fmt.Errorf("failed to pretty print JSON: %w", err)
	}

	filename := filepath.Join(dir, filepath.Base(name))
	if err := os.WriteFile(filename, out.Bytes(), 0600); err != nil {
		return fmt.Errorf("error writing file %s: %w", filename, err)
	}
	return nil
}

func getPath(ctx context.Context, clientSet *kubernetes.Clientset, path string) ([]byte, error) {
	resp, err := clientSet.RESTClient().Get().AbsPath(path).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("failed to get path %s: %w", path, err)
	}
	return resp, nil
}
