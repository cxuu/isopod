// Copyright 2019 GM Cruise LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"

	log "github.com/golang/glog"
	vaultapi "github.com/hashicorp/vault/api"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/cruise-automation/isopod/pkg/cloud"
	"github.com/cruise-automation/isopod/pkg/runtime"
	store "github.com/cruise-automation/isopod/pkg/store/kube"
	"github.com/cruise-automation/isopod/pkg/util"
)

var version = "<unknown>"

var (
	// required
	vaultToken = flag.String("vault_token", os.Getenv("VAULT_TOKEN"), "Vault token obtained during authentication.")
	namespace  = flag.String("namespace", "default", "Kubernetes namespace to store metadata in.")

	// optional
	kubeconfig     = flag.String("kubeconfig", "", "Kubernetes client config path.")
	addonRegex     = flag.String("match_addons", "", "Filters configured addons based on provided regex.")
	isopodCtx      = flag.String("context", "", "Comma-separated list of `foo=bar' context parameters passed to the clusters Starlark function.")
	dryRun         = flag.Bool("dry_run", false, "Print intended actions but don't mutate anything.")
	svcAcctKeyFile = flag.String("sa_key", "", "Path to the service account json file.")
	noSpin         = flag.Bool("nospin", false, "Disables command line status spinner.")
	kubeDiff       = flag.Bool("kube_diff", false, "Print diff against live Kubernetes objects.")
	showVersion    = flag.Bool("version", false, "Print binary version/system information and exit(0).")
	relativePath   = flag.String("rel_path", "", "The base path used to interpret double slash prefix.")
)

func init() {
	flag.Parse()
	if *vaultToken == "" {
		log.Fatalf("--vault_token or $VAULT_TOKEN must be set")
	}
}

func usageAndDie() {
	fmt.Fprintf(os.Stderr, `Isopod, an addons installer framework.

By default, isopod targets all addons on all clusters. One may confine the
selection with "--match_addons" and "--clusters_selector".

Usage: %s [options] <command> <ENTRYFILE_PATH | TEST_PATH>

The following commands are supported:
	install        install addons
	remove         uninstall addons
	list           list addons in the ENTRYFILE_PATH
	test           run unit tests in TEST_PATH

The following options are supported:
`, os.Args[0])
	flag.CommandLine.SetOutput(os.Stderr)
	flag.CommandLine.PrintDefaults()
	os.Exit(1)
}

func getCmdAndPath(argv []string) (cmd runtime.Command, path string) {
	if len(argv) < 1 {
		usageAndDie()
	}

	cmd = runtime.Command(argv[0])
	if len(argv) < 2 {
		if cmd == runtime.TestCommand {
			return
		}
		usageAndDie()
	}
	path = argv[1]
	return
}

func buildClustersRuntime(mainFile string) runtime.Runtime {
	clusters, err := runtime.New(&runtime.Config{
		EntryFile:         mainFile,
		GCPSvcAcctKeyFile: *svcAcctKeyFile,
		UserAgent:         "Isopod/" + version,
		KubeConfigPath:    *kubeconfig,
		DryRun:            *dryRun,
	})
	if err != nil {
		log.Exitf("Failed to initialize clusters runtime: %v", err)
	}
	return clusters
}

func buildAddonsRuntime(kubeC *rest.Config, mainFile string) (runtime.Runtime, error) {
	vaultC, err := vaultapi.NewClient(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Vault client: %v", err)
	}
	if *vaultToken != "" {
		vaultC.SetToken(*vaultToken)
	}

	cs, err := kubernetes.NewForConfig(kubeC)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %v", err)
	}
	helmBaseDir := *relativePath
	if helmBaseDir == "" {
		helmBaseDir = filepath.Dir(mainFile)
	}
	st := store.New(cs, *namespace)
	opts := []runtime.Option{
		runtime.WithVault(vaultC),
		runtime.WithKube(kubeC, *kubeDiff),
		runtime.WithHelm(helmBaseDir),
		runtime.WithAddonRegex(regexp.MustCompile(*addonRegex)),
	}
	if *noSpin {
		opts = append(opts, runtime.WithNoSpin())
	}

	addons, err := runtime.New(&runtime.Config{
		EntryFile:         mainFile,
		GCPSvcAcctKeyFile: *svcAcctKeyFile,
		UserAgent:         "Isopod/" + version,
		KubeConfigPath:    *kubeconfig,
		Store:             st,
		DryRun:            *dryRun,
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize addons runtime: %v", err)
	}

	return addons, nil
}

func main() {
	ctx := context.Background()

	// Redirects all output to standrad Go log to Google's log.
	log.CopyStandardLogTo("INFO")
	defer log.Flush()

	if *showVersion {
		fmt.Println("Version:", version)
		fmt.Printf("System: %s/%s\n", goruntime.GOOS, goruntime.GOARCH)
		return
	}

	cmd, path := getCmdAndPath(flag.Args())

	if cmd == runtime.TestCommand {
		ok, err := runtime.RunUnitTests(ctx, path, os.Stdout, os.Stderr)
		if err != nil {
			log.Exitf("Failed to run tests: %v", err)
		} else if !ok {
			log.Flush()
			os.Exit(1)
		}
		return
	}

	mainFile := path
	if mainFile == "" {
		log.Exitf("path to main Starlark entry file must be set")
	}

	ctxParams, err := util.ParseCommaSeparatedParams(*isopodCtx)
	if err != nil {
		log.Exitf("Invalid value to --context: %v", err)
	}

	clusters := buildClustersRuntime(mainFile)
	if err := clusters.Load(ctx); err != nil {
		log.Exitf("Failed to load clusters runtime: %v", err)
	}

	errorReturned := false

	if err := clusters.ForEachCluster(ctx, ctxParams, func(k8sVendor cloud.KubernetesVendor) {
		kubeConfig, err := k8sVendor.KubeConfig(ctx)
		if err != nil {
			log.Exitf("Failed to build kube rest config for k8s vendor %v: %v", k8sVendor, err)
		}
		addons, err := buildAddonsRuntime(kubeConfig, mainFile)
		if err != nil {
			log.Exitf("Failed to initialize runtime: %v", err)
		}

		if err := addons.Load(ctx); err != nil {
			log.Exitf("Failed to load addons runtime: %v", err)
		}

		if err := addons.Run(ctx, cmd, k8sVendor.AddonSkyCtx()); err != nil {
			errorReturned = true
			log.Errorf("addons run failed: %v", err)
		}
	}); err != nil {
		log.Exitf("Failed to iterate through clusters: %v", err)
	}

	if errorReturned {
		os.Exit(2)
	}
}
