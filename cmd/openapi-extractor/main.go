// Copyright 2022 OnMetal authors
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
	"time"

	"github.com/go-logr/logr"
	"github.com/onmetal/controller-utils/buildutils"
	"github.com/onmetal/openapi-extractor/envtestutils"
	"github.com/onmetal/openapi-extractor/envtestutils/apiserver"
	flag "github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

const (
	apiServiceTimeout = 5 * time.Minute
)

var (
	testEnv            *envtest.Environment
	testEnvExt         *envtestutils.EnvironmentExtensions
	log                = ctrl.Log.WithName("openapi-extractor")
	apiServerCommand   []string
	outputDir          = "."
	apiServicePaths    []string
	timeout            = 5 * time.Second
	apiServerPackage   string
	apiServerBuildOpts []string
)

func main() {
	flag.StringVar(&apiServerPackage, "apiserver-package", apiServerPackage, "Package to build the api server")
	flag.StringSliceVar(&apiServerBuildOpts, "apiserver-build-opts", apiServerBuildOpts, "Flags for building the api server")
	flag.StringSliceVar(&apiServerCommand, "apiserver-command", apiServerCommand, "Command to run the api server")
	flag.StringSliceVar(&apiServicePaths, "apiservices", apiServicePaths, "Comma separated list of api service definitions")
	flag.StringVar(&outputDir, "output", outputDir, "Directory to store the extracted OpenAPI specs (default: current directory)")
	flag.DurationVar(&timeout, "timeout", timeout, "Timeout to wait for api services to become available")

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
	testEnv = &envtest.Environment{}
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

	if err := waitForApiServices(ctx, log, clientSet, timeout, testEnvExt.APIServiceInstallOptions.APIServices); err != nil {
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

func waitForApiServices(ctx context.Context, log logr.Logger, clientSet *kubernetes.Clientset, duration time.Duration, services []*apiregistrationv1.APIService) error {
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	processDone := make(chan bool)
	go func() {
		var available bool
		for !available {
			for _, apiService := range services {
				available = true
				gv := fmt.Sprintf("%s/%s", apiService.Spec.Group, apiService.Spec.Version)
				err := clientSet.RESTClient().Verb(http.MethodHead).AbsPath(fmt.Sprintf("/openapi/v3/apis/%s", gv)).Do(ctx).Error()
				if err != nil {
					log.V(1).Info("API service is not available", "GroupVersion", gv)
					available = false
					break
				}
				log.Info("API service available", "GroupVersion", gv)
			}
		}
		processDone <- true
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("encountered timeout while waiting for api serivces to become available")
	case <-processDone:
		log.Info("All API services are available")
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

		if err := writeFile(resp, fmt.Sprintf("%s/%s", outputDir, "v3"), fileName); err != nil {
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

	if err := writeFile(resp, outputDir, "swagger.json"); err != nil {
		return fmt.Errorf("failed to write OpenAPI v2 file: %w", err)
	}

	return nil
}

func writeFile(resp []byte, outputDir string, fileName string) error {
	log.Info("Writing file", "OutputDirectory", outputDir, "File", fileName)

	if err := os.MkdirAll(outputDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create output directory %s: %w", outputDir, err)
	}

	var out bytes.Buffer
	if err := json.Indent(&out, resp, "", "\t"); err != nil {
		return fmt.Errorf("failed to pretty print JSON: %w", err)
	}
	file := filepath.Join(outputDir, filepath.Base(fileName))

	f, err := os.Create(file)
	defer func() {
		// TODO: properly handle error
		_ = f.Close()
	}()

	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", file, err)
	}
	_, err = f.Write(out.Bytes())
	if err != nil {
		return fmt.Errorf("failed to write file %s: %w", file, err)
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
