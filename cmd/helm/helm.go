/*
Copyright The Helm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main // import "helm.sh/helm/v3/cmd/helm"

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/config"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	// Import to initialize client auth plugins.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/gates"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// FeatureGateOCI is the feature gate for checking if `helm chart` and `helm registry` commands should work
const FeatureGateOCI = gates.Gate("HELM_EXPERIMENTAL_OCI")

var settings = cli.New()

type ZitiFlags struct {
	zConfig string
	service string
}

var configFilePath string
var serviceName string

var zFlags = ZitiFlags{}

type MinKubeConfig struct {
	Contexts []struct {
		Context Context `yaml:"context"`
		Name    string  `yaml:"name"`
	} `yaml:"contexts"`
}

type Context struct {
	ZConfig string `yaml:"zConfig"`
	Service string `yaml:"service"`
}

func init() {
	log.SetFlags(log.Lshortfile)
}

func debug(format string, v ...interface{}) {
	if settings.Debug {
		format = fmt.Sprintf("[debug] %s\n", format)
		log.Output(2, fmt.Sprintf(format, v...))
	}
}

func warning(format string, v ...interface{}) {
	format = fmt.Sprintf("WARNING: %s\n", format)
	fmt.Fprintf(os.Stderr, format, v...)
}

func main() {
	// Setting the name of the app for managedFields in the Kubernetes client.
	// It is set here to the full name of "helm" so that renaming of helm to
	// another name (e.g., helm2 or helm3) does not change the name of the
	// manager as picked up by the automated name detection.
	kube.ManagedFieldsManager = "helm"

	settings.SetWrapperConfigFn(wrapConfigFn)
	actionConfig := new(action.Configuration)
	cmd, err := newRootCmd(actionConfig, os.Stdout, os.Args[1:])
	if err != nil {
		warning("%+v", err)
		os.Exit(1)
	}

	cmd = setZitiFlags(cmd)
	cmd.PersistentFlags().Parse(os.Args)

	// try to get the ziti options from the flags
	configFilePath = cmd.Flag("zConfig").Value.String()
	serviceName = cmd.Flag("service").Value.String()

	// get the loaded kubeconfig
	kubeconfig := getKubeconfig()

	// if both the config file and service name are not set, parse the kubeconfig file
	if configFilePath == "" || serviceName == "" {
		parseKubeConfig(cmd, kubeconfig)
	}

	// run when each command's execute method is called
	cobra.OnInitialize(func() {
		helmDriver := os.Getenv("HELM_DRIVER")
		if err := actionConfig.Init(settings.RESTClientGetter(), settings.Namespace(), helmDriver, debug); err != nil {
			log.Fatal(err)
		}
		if helmDriver == "memory" {
			loadReleasesInMemory(actionConfig)
		}
	})

	if err := cmd.Execute(); err != nil {
		debug("%+v", err)
		switch e := err.(type) {
		case pluginError:
			os.Exit(e.code)
		default:
			os.Exit(1)
		}
	}
}

func checkOCIFeatureGate() func(_ *cobra.Command, _ []string) error {
	return func(_ *cobra.Command, _ []string) error {
		if !FeatureGateOCI.IsEnabled() {
			return FeatureGateOCI.Error()
		}
		return nil
	}
}

// This function loads releases into the memory storage if the
// environment variable is properly set.
func loadReleasesInMemory(actionConfig *action.Configuration) {
	filePaths := strings.Split(os.Getenv("HELM_MEMORY_DRIVER_DATA"), ":")
	if len(filePaths) == 0 {
		return
	}

	store := actionConfig.Releases
	mem, ok := store.Driver.(*driver.Memory)
	if !ok {
		// For an unexpected reason we are not dealing with the memory storage driver.
		return
	}

	actionConfig.KubeClient = &kubefake.PrintingKubeClient{Out: ioutil.Discard}

	for _, path := range filePaths {
		b, err := ioutil.ReadFile(path)
		if err != nil {
			log.Fatal("Unable to read memory driver data", err)
		}

		releases := []*release.Release{}
		if err := yaml.Unmarshal(b, &releases); err != nil {
			log.Fatal("Unable to unmarshal memory driver data: ", err)
		}

		for _, rel := range releases {
			if err := store.Create(rel); err != nil {
				log.Fatal(err)
			}
		}
	}
	// Must reset namespace to the proper one
	mem.SetNamespace(settings.Namespace())
}

func wrapConfigFn(restConfig *rest.Config) *rest.Config {

	restConfig.Dial = dialFunc
	return restConfig
}

// function for handling the dialing with ziti
func dialFunc(ctx context.Context, network, address string) (net.Conn, error) {
	service := serviceName
	configFile, err := config.NewFromFile(configFilePath)

	if err != nil {
		logrus.WithError(err).Error("Error loading config file")
		os.Exit(1)
	}

	context := ziti.NewContextWithConfig(configFile)
	return context.Dial(service)
}

func setZitiFlags(command *cobra.Command) *cobra.Command {

	command.PersistentFlags().StringVarP(&zFlags.zConfig, "zConfig", "c", "", "Path to ziti config file")
	command.PersistentFlags().StringVarP(&zFlags.service, "service", "S", "", "Service name")

	return command
}

// function for getting the current kubeconfig
func getKubeconfig() clientcmd.ClientConfig {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules,
		configOverrides)

	return kubeConfig
}

func parseKubeConfig(command *cobra.Command, kubeconfig clientcmd.ClientConfig) {
	// attempt to get the kubeconfig path from the command flags
	kubeconfigPath := command.Flag("kubeconfig").Value.String()

	// if the path is not set, attempt to get it from the kubeconfig precedence
	if kubeconfigPath == "" {
		// obtain the list of kubeconfig files from the current kubeconfig
		kubeconfigPrcedence := kubeconfig.ConfigAccess().GetLoadingPrecedence()

		// get the raw API config
		apiConfig, err := kubeconfig.RawConfig()

		if err != nil {
			panic(err)
		}

		// set the ziti options from one of the config files
		getZitiOptionsFromConfigList(kubeconfigPrcedence, apiConfig.CurrentContext)

	} else {
		// get the ziti options form the specified path
		getZitiOptionsFromConfig(kubeconfigPath)
	}

}

func getZitiOptionsFromConfigList(kubeconfigPrcedence []string, currentContext string) {
	// for the kubeconfig files in the precedence
	for _, path := range kubeconfigPrcedence {

		// read the config file
		config := readKubeConfig(path)

		// loop through the context list
		for _, context := range config.Contexts {

			// if the context name matches the current context
			if currentContext == context.Name {

				// set the config file path if it's not already set
				if configFilePath == "" {
					configFilePath = context.Context.ZConfig
				}

				// set the service name if it's not already set
				if serviceName == "" {
					serviceName = context.Context.Service
				}

				break
			}
		}
	}
}

func readKubeConfig(kubeconfig string) MinKubeConfig {
	// get the file name from the path
	filename, _ := filepath.Abs(kubeconfig)

	// read the yaml file
	yamlFile, err := ioutil.ReadFile(filename)

	if err != nil {
		panic(err)
	}

	var minKubeConfig MinKubeConfig

	//parse the yaml file
	err = yaml.Unmarshal(yamlFile, &minKubeConfig)
	if err != nil {
		panic(err)
	}

	return minKubeConfig

}

func getZitiOptionsFromConfig(kubeconfig string) {

	// get the config from the path
	config := clientcmd.GetConfigFromFileOrDie(kubeconfig)

	// get the current context
	currentContext := config.CurrentContext

	// read the yaml file
	minKubeConfig := readKubeConfig(kubeconfig)

	var context Context
	// find the context that matches the current context
	for _, ctx := range minKubeConfig.Contexts {

		if ctx.Name == currentContext {
			context = ctx.Context
		}
	}

	// set the config file if not already set
	if configFilePath == "" {
		configFilePath = context.ZConfig
	}

	// set the service name if not already set
	if serviceName == "" {
		serviceName = context.Service
	}
}
