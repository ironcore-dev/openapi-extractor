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
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"github.com/onmetal/openapi-extractor/envtestutils"
	"github.com/onmetal/openapi-extractor/envtestutils/apiserver"
	flag "github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
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
	testEnv    *envtest.Environment
	testEnvExt *envtestutils.EnvironmentExtensions
	log        = ctrl.Log.WithName("openapi-extractor")
)

func main() {
	var apiServerPath string
	var outputDir string
	var apiServicePaths []string
	var delay time.Duration

	flag.StringVar(&apiServerPath, "apiserver", "", "Path to the aggregated apiserver binary")
	flag.StringSliceVar(&apiServicePaths, "apiservices", []string{}, "Comma separated list of apiservice definitions")
	flag.StringVar(&outputDir, "output", "", "Directory to store the extracted OpenAPI specs (default: current directory)")
	flag.DurationVar(&delay, "delay", 2*time.Second, "Delay to wait for apiservices to become available")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(goflag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	testEnv = &envtest.Environment{}
	testEnvExt = &envtestutils.EnvironmentExtensions{
		APIServiceDirectoryPaths:       apiServicePaths,
		ErrorIfAPIServicePathIsMissing: true,
	}

	cfg, err := envtestutils.StartWithExtensions(testEnv, testEnvExt)
	if err != nil {
		log.Error(err, "failed to start testenv")
		os.Exit(1)
	}
	defer envtestutils.StopWithExtensions(testEnv, testEnvExt)

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		log.Error(err, "failed to create Kubernetes client")
		os.Exit(1)
	}

	apiSrv, err := apiserver.New(cfg, apiserver.Options{
		Command:     []string{apiServerPath},
		ETCDServers: []string{testEnv.ControlPlane.Etcd.URL.String()},
		Host:        testEnvExt.APIServiceInstallOptions.LocalServingHost,
		Port:        testEnvExt.APIServiceInstallOptions.LocalServingPort,
		CertDir:     testEnvExt.APIServiceInstallOptions.LocalServingCertDir,
	})
	if err != nil {
		log.Error(err, "failed to setup api server")
		os.Exit(1)
	}

	if err := apiSrv.Start(); err != nil {
		log.Error(err, "failed to start api server")
		os.Exit(1)
	}
	defer apiSrv.Stop()

	if err := envtestutils.WaitUntilAPIServicesReadyWithTimeout(apiServiceTimeout, testEnvExt, k8sClient, scheme.Scheme); err != nil {
		log.Error(err, "failed to wait for api server to become ready")
		os.Exit(1)
	}

	clientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error(err, "failed to create clientset from config")
		os.Exit(1)
	}

	// Dirty hack: Delaying the openapi extraction by waiting for the apiservices to become available.
	time.Sleep(delay)

	if err := extractOpenAPIv2(log, clientSet, outputDir); err != nil {
		log.Error(err, "failed to extract OpenAPI v2 spec")
		os.Exit(1)
	}

	if err := extractOpenAPIv3(log, clientSet, testEnvExt, outputDir); err != nil {
		log.Error(err, "failed to extract OpenAPI v3 spec")
		os.Exit(1)
	}
}

func extractOpenAPIv3(log logr.Logger, clientSet *kubernetes.Clientset, ext *envtestutils.EnvironmentExtensions, outputDir string) error {
	log.Info("Extracting OpenAPI v3")
	apiServices := ext.APIServiceInstallOptions.APIServices

	for _, apiService := range apiServices {
		fileName := fmt.Sprintf("apis__%s__%s_openapi.json", apiService.Spec.Group, apiService.Spec.Version)
		path := fmt.Sprintf("/openapi/v3/apis/%s/%s", apiService.Spec.Group, apiService.Spec.Version)

		resp, err := getPath(clientSet, path)
		if err != nil {
			return fmt.Errorf("failed to get OpenAPI v3 path %s: %w", path, err)
		}

		if err := writeFile(resp, fmt.Sprintf("%s/%s", outputDir, "v3"), fileName); err != nil {
			return fmt.Errorf("failed to write OpenAPI v3 file: %w", err)
		}
	}
	return nil
}

func extractOpenAPIv2(log logr.Logger, clientSet *kubernetes.Clientset, outputDir string) error {
	log.Info("Extracting OpenAPI v2")

	path := "/openapi/v2"
	resp, err := clientSet.RESTClient().Get().AbsPath(path).Do(context.Background()).Raw()
	if err != nil {
		return fmt.Errorf("failed to get OpenAPI v2 content: %w", err)
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
	defer f.Close()

	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", file, err)
	}
	_, err = f.Write(out.Bytes())
	if err != nil {
		return fmt.Errorf("failed to write file %s: %w", file, err)
	}
	return nil
}

func getPath(clientSet *kubernetes.Clientset, path string) ([]byte, error) {
	resp, err := clientSet.RESTClient().Get().AbsPath(path).Do(context.Background()).Raw()
	if err != nil {
		return nil, fmt.Errorf("failed to get path %s: %w", path, err)
	}
	return resp, nil
}
